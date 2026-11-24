// Package gc works out what in runwhp's store is in use, and removes what
// is not: image records, layers, kernels and volumes.
//
// Layers are shared, by image records and by containers alike, and nothing
// counts their users as they come and go. Instead, a layer is in use while
// something that lists it is in the store: an image record, or a
// container's configuration, stopped containers included. A container
// boots from the layers its configuration lists, never from its image's
// record, so it keeps the layers of an image pulled again since, or
// removed. Removing is then mark and sweep, over a store that cannot
// change meanwhile: runwhps that add records hold the store's lock shared
// (Hold), from before they resolve an image until what they add is
// recorded, and a prune holds it exclusively (Exclusive).
//
// A layer a guest has open cannot be removed anyway: guests open layers
// without sharing delete, and such a layer is skipped.
package gc

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/volume"
)

// Usage is what the store holds, and what uses what.
type Usage struct {
	Root       string
	Images     []*Image          // readable records, newest image first
	Junk       []*image.Record   // records that cannot be read
	Layers     map[string]*Layer // every layer in the store, by diffID
	Containers []*Container      // newest first
	Volumes    []*Volume
	Kernels    []*Kernel

	images  *image.Store
	kernels *kernel.Store
}

// Image is an image record and what uses it.
type Image struct {
	*image.Record
	Size   int64 // its layers' files
	Shared int64 // the part of Size in layers images with other IDs list too
	// Containers were made from this image: its reference and its digest.
	Containers []*container.Container
}

// Layer is a layer's file and what lists it.
type Layer struct {
	DiffID     string
	Size       int64
	Images     []*Image
	Containers []*container.Container
}

// Rooted reports whether anything lists the layer.
func (l *Layer) Rooted() bool { return len(l.Images) > 0 || len(l.Containers) > 0 }

// Container is a container and the space it takes: its writable layer and
// its logs.
type Container struct {
	*container.Container
	Size int64
}

// Volume is a volume, the space it takes, and the containers that mount it.
type Volume struct {
	*volume.Volume
	Size  int64
	Users []*container.Container
}

// Kernel is a kernel in the store.
type Kernel struct {
	Digest string
	Dir    string
	Size   int64
	// Kept is true for the kernel this runwhp boots, and for one a
	// container was given explicitly from the store.
	Kept bool
}

// Load reads the store under root. It lists containers as the container
// store does, which removes what runwhps that died left behind.
func Load(root string) (*Usage, error) {
	u := &Usage{
		Root:    root,
		Layers:  map[string]*Layer{},
		images:  image.Open(root),
		kernels: kernel.Open(root),
	}

	cs, err := container.Open(root).List()
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		size := allocated(c.RW())
		for _, p := range c.LogPaths() {
			size += allocated(p)
		}
		u.Containers = append(u.Containers, &Container{c, size})
	}

	diffIDs, err := u.images.Layers.List()
	if err != nil {
		return nil, err
	}
	for _, d := range diffIDs {
		l := &Layer{DiffID: d}
		// Its ext4 image and its tar metadata.
		for _, p := range u.images.Layers.Files(d) {
			l.Size += allocated(p)
		}
		u.Layers[d] = l
	}

	recs, err := u.images.List()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if rec.Image == nil {
			u.Junk = append(u.Junk, rec)
			continue
		}
		img := &Image{Record: rec}
		layers, _ := rec.Layers()
		for _, d := range layers {
			if l := u.Layers[d]; l != nil {
				l.Images = append(l.Images, img)
				img.Size += l.Size
			}
		}
		for _, c := range cs {
			if c.Config.ImageName == rec.Ref && c.Config.ImageDigest == rec.Digest {
				img.Containers = append(img.Containers, c)
			}
		}
		u.Images = append(u.Images, img)
	}
	for _, img := range u.Images {
		layers, _ := img.Layers()
		for _, d := range layers {
			l := u.Layers[d]
			if l == nil {
				continue
			}
			for _, other := range l.Images {
				if other.ID() != img.ID() {
					img.Shared += l.Size
					break
				}
			}
		}
	}

	layersDir := filepath.Join(root, "layers", "sha256")
	for _, c := range cs {
		for _, d := range c.Config.Layers {
			if l := u.Layers[d]; l != nil {
				l.Containers = append(l.Containers, c)
			}
		}
		// Layers given with --layer from the store, for development.
		for _, p := range c.Config.LayerFiles {
			if within(layersDir, p) {
				d := "sha256:" + strings.TrimSuffix(filepath.Base(p), ".ext4")
				if l := u.Layers[d]; l != nil {
					l.Containers = append(l.Containers, c)
				}
			}
		}
	}

	vs, err := volume.Open(root).List()
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		size, _ := v.OnDisk()
		vol := &Volume{Volume: v, Size: size}
		for _, c := range cs {
			for _, cv := range c.Config.Volumes {
				if strings.EqualFold(cv.Name, v.Name) {
					vol.Users = append(vol.Users, c)
					break
				}
			}
		}
		u.Volumes = append(u.Volumes, vol)
	}

	digests, err := u.kernels.List()
	if err != nil {
		return nil, err
	}
	for _, d := range digests {
		dir, _ := u.kernels.Dir(d)
		k := &Kernel{Digest: d, Dir: dir, Kept: d == kernel.Default.Digest}
		filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				k.Size += allocated(p)
			}
			return nil
		})
		for _, c := range cs {
			if c.Config.Kernel != "" && within(dir, c.Config.Kernel) {
				k.Kept = true
			}
		}
		u.Kernels = append(u.Kernels, k)
	}
	return u, nil
}

// allocated is the space a file takes on disk, or 0 if it is not there.
func allocated(path string) int64 {
	n, err := disk.Allocated(path)
	if err != nil {
		return 0
	}
	return n
}

// within reports whether path lies inside dir. Windows paths compare
// without regard to case.
func within(dir, path string) bool {
	rel, err := filepath.Rel(strings.ToLower(filepath.Clean(dir)), strings.ToLower(filepath.Clean(path)))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// Removable is true for a container that a container prune removes:
// stopped, or created and waiting to start.
func Removable(c *container.Container) bool {
	return c.State.Status == container.Stopped || c.State.Status == container.Created
}
