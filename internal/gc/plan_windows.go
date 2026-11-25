package gc

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/volume"
)

// Plan is what a removal will remove.
type Plan struct {
	Records      []*image.Record
	Layers       []*Layer
	Kernels      []*Kernel
	Volumes      []*Volume
	Cache        []*CacheEntry
	CacheVolumes []*Volume
}

// Report is what a removal removed.
type Report struct {
	Untagged []string // the references of the records removed, as Docker shows them
	Deleted  []string // the diffIDs of the layers removed
	Kernels  []string // the digests of the kernels removed
	Volumes  []string // the names of the volumes removed
	Cache    []string // the keys of the build cache entries removed
	Freed    int64    // bytes
	// InUse are layers and volumes a guest had open, which were left.
	InUse  []string
	Errors []error
}

// Dangling plans the removal of what no image or container can use:
// layers nothing lists, which is what an image pulled again leaves, and
// records that cannot be read or have lost layers, which Get treats as
// absent, and untagged images no container uses: those loaded or imported
// without a reference, as Docker's image prune removes them.
func (u *Usage) Dangling() *Plan {
	p := &Plan{Records: append([]*image.Record(nil), u.Junk...)}
	for _, img := range u.Images {
		if !img.Complete || image.Untagged(img.Ref) && len(img.Containers) == 0 {
			p.Records = append(p.Records, img.Record)
		}
	}
	p.Layers = u.unrooted(p.Records, nil)
	return p
}

// Unused plans the removal of every image no container uses, and of every
// layer that leaves unlisted, besides what Dangling removes.
func (u *Usage) Unused() *Plan {
	p := &Plan{Records: append([]*image.Record(nil), u.Junk...)}
	for _, img := range u.Images {
		if !img.Complete || len(img.Containers) == 0 {
			p.Records = append(p.Records, img.Record)
		}
	}
	p.Layers = u.unrooted(p.Records, nil)
	return p
}

// RemoveImages plans the removal of the images args name, by reference or
// by ID, as docker rmi removes them, and of their layers that nothing else
// lists. Without force, it refuses an image a container was made from, and
// an ID that several references name; with it, the record goes, and a
// container's configuration still keeps the layers it lists. It returns an
// error for each argument it refuses.
func (u *Usage) RemoveImages(args []string, force bool) (*Plan, []error) {
	p := &Plan{}
	var errs []error
	seen := map[string]bool{}
	for _, arg := range args {
		found, err := u.images.Find(arg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(found) > 1 && !force {
			var refs []string
			for _, r := range found {
				refs = append(refs, image.Familiar(r.Ref))
			}
			errs = append(errs, fmt.Errorf("image %s is referenced by %s: remove them by name, or with -f", arg, strings.Join(refs, ", ")))
			continue
		}
		var refused error
		for _, r := range found {
			if img := u.image(r.Path); img != nil && len(img.Containers) > 0 && !force {
				refused = fmt.Errorf("image %s is used by container %s: delete it first, or remove the image with -f", image.Familiar(r.Ref), describe(img.Containers))
				break
			}
		}
		if refused != nil {
			errs = append(errs, refused)
			continue
		}
		for _, r := range found {
			if !seen[strings.ToLower(r.Path)] {
				seen[strings.ToLower(r.Path)] = true
				p.Records = append(p.Records, r)
			}
		}
	}
	// Only these images' layers go, not every layer nothing lists: an
	// image prune removes those.
	only := map[string]bool{}
	for _, r := range p.Records {
		if r.Image != nil {
			layers, _ := r.Layers()
			for _, d := range layers {
				only[d] = true
			}
		}
	}
	p.Layers = u.unrooted(p.Records, only)
	return p, errs
}

// OldKernels plans the removal of kernels this runwhp does not boot and no
// container was given.
func (u *Usage) OldKernels() *Plan {
	p := &Plan{}
	for _, k := range u.Kernels {
		if !k.Kept {
			p.Kernels = append(p.Kernels, k)
		}
	}
	return p
}

// UnusedVolumes plans the removal of the volumes no container mounts,
// running or not.
func (u *Usage) UnusedVolumes() *Plan {
	p := &Plan{}
	for _, v := range u.Volumes {
		if len(v.Users) == 0 {
			p.Volumes = append(p.Volumes, v)
		}
	}
	return p
}

// BuildCache plans the removal of the build cache: with all, every entry
// and cache mount, as docker builder prune -a; without, the entries whose
// layer no image or container uses. Their layers that nothing else lists
// go with them.
func (u *Usage) BuildCache(all bool) *Plan {
	p := &Plan{}
	for _, e := range u.Cache {
		if all || e.Dangling() {
			p.Cache = append(p.Cache, e)
		}
	}
	if all {
		p.CacheVolumes = u.CacheVolumes
	}
	only := map[string]bool{}
	for _, e := range p.Cache {
		if e.Layer != nil {
			only[e.DiffID] = true
		}
	}
	p.Layers = u.unrootedWith(nil, p.Cache, only)
	return p
}

// unrooted returns the layers that nothing lists once the given records are
// gone, only among those named in only if it is not nil.
func (u *Usage) unrooted(removed []*image.Record, only map[string]bool) []*Layer {
	return u.unrootedWith(removed, nil, only)
}

// unrootedWith is unrooted, the given cache entries gone too.
func (u *Usage) unrootedWith(removed []*image.Record, cache []*CacheEntry, only map[string]bool) []*Layer {
	gone := map[string]bool{}
	for _, r := range removed {
		gone[strings.ToLower(r.Path)] = true
	}
	goneCache := map[string]bool{}
	for _, e := range cache {
		goneCache[e.Key] = true
	}
	var out []*Layer
	for d, l := range u.Layers {
		if only != nil && !only[d] || len(l.Containers) > 0 {
			continue
		}
		if slices.ContainsFunc(l.Cache, func(e *CacheEntry) bool { return !goneCache[e.Key] }) {
			continue // a build cache entry keeps it
		}
		kept := false
		for _, img := range l.Images {
			if !gone[strings.ToLower(img.Path)] {
				kept = true
				break
			}
		}
		if !kept {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DiffID < out[j].DiffID })
	return out
}

func (u *Usage) image(path string) *Image {
	for _, img := range u.Images {
		if strings.EqualFold(img.Path, path) {
			return img
		}
	}
	return nil
}

// Apply removes what p plans: records first, then layers, kernels and
// volumes. Layers and volumes a guest has open are left, and reported.
func (u *Usage) Apply(p *Plan) *Report {
	r := &Report{}
	for _, rec := range p.Records {
		if err := u.images.Remove(rec); err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		if rec.Image != nil {
			r.Untagged = append(r.Untagged, image.Familiar(rec.Ref))
		}
	}
	cache := buildcache.Open(u.Root)
	for _, e := range p.Cache {
		if err := cache.Remove(e.Key); err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		r.Cache = append(r.Cache, e.Key)
	}
	for _, l := range p.Layers {
		err := u.images.Layers.Remove(l.DiffID)
		switch {
		case errors.Is(err, layer.ErrInUse):
			r.InUse = append(r.InUse, l.DiffID)
		case err != nil:
			r.Errors = append(r.Errors, err)
		default:
			r.Deleted = append(r.Deleted, l.DiffID)
			r.Freed += l.Size
		}
	}
	r.Freed += u.sweepBlobs(p.Records, r)
	for _, k := range p.Kernels {
		if err := u.kernels.Remove(k.Digest); err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		r.Kernels = append(r.Kernels, k.Digest)
		r.Freed += k.Size
	}
	volumes := volume.Open(u.Root)
	for _, v := range p.Volumes {
		err := volumes.Remove(v.Name)
		switch {
		case errors.Is(err, volume.ErrInUse):
			r.InUse = append(r.InUse, v.Name)
		case err != nil:
			r.Errors = append(r.Errors, err)
		default:
			r.Volumes = append(r.Volumes, v.Name)
			r.Freed += v.Size
		}
	}
	cacheVolumes := volume.Open(cache.VolumeRoot())
	for _, v := range p.CacheVolumes {
		err := cacheVolumes.Remove(v.Name)
		switch {
		case errors.Is(err, volume.ErrInUse):
			r.InUse = append(r.InUse, v.Name)
		case err != nil:
			r.Errors = append(r.Errors, err)
		default:
			r.Freed += v.Size
		}
	}
	return r
}

// sweepBlobs removes the configuration blobs no record keeps once the
// removed records are gone, and returns what that freed. A record keeps
// the blob its ID names. Blobs are a few KB each, and not reported.
func (u *Usage) sweepBlobs(removed []*image.Record, r *Report) int64 {
	gone := map[string]bool{}
	for _, rec := range removed {
		gone[strings.ToLower(rec.Path)] = true
	}
	kept := map[string]bool{}
	for _, img := range u.Images {
		if !gone[strings.ToLower(img.Path)] {
			kept[img.ID()] = true
		}
	}
	// A container keeps its image's, which commit builds on, even once
	// the image is removed.
	for _, c := range u.Containers {
		if c.Config.ImageID != "" {
			kept[c.Config.ImageID] = true
		}
	}
	blobs, err := u.images.Blobs()
	if err != nil {
		r.Errors = append(r.Errors, err)
		return 0
	}
	var freed int64
	for _, d := range blobs {
		if kept[d] {
			continue
		}
		size := u.images.BlobSize(d)
		if err := u.images.RemoveBlob(d); err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		freed += size
	}
	return freed
}

// describe names containers for a message: by name, or by short ID.
func describe(cs []*container.Container) string {
	var names []string
	for _, c := range cs {
		if c.Name != "" {
			names = append(names, c.Name)
		} else {
			names = append(names, c.Short())
		}
	}
	return strings.Join(names, ", ")
}

// AndCache adds to p what BuildCache(all) would remove, and plans the
// removal of every layer nothing lists once both are gone, as system
// prune removes the build cache with the rest.
func (u *Usage) AndCache(p *Plan, all bool) *Plan {
	c := u.BuildCache(all)
	p.Cache, p.CacheVolumes = c.Cache, c.CacheVolumes
	p.Layers = u.unrootedWith(p.Records, p.Cache, nil)
	return p
}
