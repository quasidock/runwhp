package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/layer"
)

// Loaded is an image Load stored.
type Loaded struct {
	ID   string
	Refs []string // normalized; none if the archive named it by none
}

// Load stores the images in an archive: their layers, with their tar
// metadata, their configurations, and a record per reference. An image
// the archive names by no reference gets an untagged record, unless a
// record already has its ID. Layers already in the store are not read
// again. The caller holds the store's lock shared.
func (s *Store) Load(a *Archive) ([]Loaded, error) {
	imgs, err := a.Images()
	if err != nil {
		return nil, err
	}
	var out []Loaded
	for _, ai := range imgs {
		cf := ai.ConfigFile
		if cf.OS != Platform.OS || cf.Architecture != Platform.Architecture {
			return out, fmt.Errorf("image %s is for %s/%s; runwhp runs only %s/%s",
				ai.ConfigDigest, cf.OS, cf.Architecture, Platform.OS, Platform.Architecture)
		}
		var refs []string
		for _, t := range ai.RepoTags {
			r, err := normalize(t)
			if err != nil {
				return out, err
			}
			refs = append(refs, r.Name())
		}
		for _, l := range ai.Layers {
			if l.DiffID == layer.EmptyDiffID || s.Layers.Has(l.DiffID) && s.Layers.HasSplit(l.DiffID) {
				continue
			}
			s.progressf("  %.19s  loading and converting to ext4", l.DiffID)
			if err := s.loadLayer(l); err != nil {
				return out, err
			}
		}
		if err := s.PutBlob(ai.ConfigDigest, ai.Config); err != nil {
			return out, err
		}
		img := &Image{ConfigDigest: ai.ConfigDigest, Config: json.RawMessage(ai.Config)}
		for _, ref := range refs {
			rec := *img
			rec.Ref = ref
			if err := s.Put(ref, &rec); err != nil {
				return out, err
			}
		}
		if len(refs) == 0 {
			if _, err := s.Find(ai.ConfigDigest); errors.Is(err, ErrNoSuchImage) {
				img.Ref = ai.ConfigDigest
				if err := s.Put(img.Ref, img); err != nil {
					return out, err
				}
			}
		}
		out = append(out, Loaded{ID: ai.ConfigDigest, Refs: refs})
	}
	return out, nil
}

func (s *Store) loadLayer(l ArchiveLayer) error {
	rc, err := l.Open()
	if err != nil {
		return fmt.Errorf("layer %s: %w", l.DiffID, err)
	}
	defer rc.Close()
	if _, err := s.Layers.Put(l.DiffID, rc); err != nil {
		return err
	}
	return nil
}

// Import makes a one-layer image from a root file system's tar, as
// `docker import` does, applying changes to its configuration, and records
// it under ref, or untagged if ref is empty. The caller holds the store's
// lock shared.
func (s *Store) Import(tarStream io.Reader, ref string, changes []string, message string, now time.Time) (*Image, error) {
	var target string
	if ref != "" {
		r, err := normalize(ref)
		if err != nil {
			return nil, err
		}
		target = r.Name()
	}
	cf := &v1.ConfigFile{
		Architecture: Platform.Architecture,
		OS:           Platform.OS,
		Created:      v1.Time{Time: now.UTC()},
	}
	if err := ApplyChanges(&cf.Config, changes); err != nil {
		return nil, err
	}
	diffID, _, err := s.Layers.Import(tarStream)
	if err != nil {
		return nil, err
	}
	h, err := v1.NewHash(diffID)
	if err != nil {
		return nil, err
	}
	cf.RootFS = v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{h}}
	if message == "" {
		message = "Imported from -"
	}
	cf.History = []v1.History{{Created: cf.Created, Comment: message}}
	raw, err := json.Marshal(cf)
	if err != nil {
		return nil, err
	}
	img := &Image{Ref: target, ConfigDigest: digestOf(raw), Config: raw}
	if err := s.PutBlob(img.ConfigDigest, raw); err != nil {
		return nil, err
	}
	if img.Ref == "" {
		img.Ref = img.ConfigDigest
	}
	if err := s.Put(img.Ref, img); err != nil {
		return nil, err
	}
	return img, nil
}
