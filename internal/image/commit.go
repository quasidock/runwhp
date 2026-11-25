package image

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Commit describes an image made from a container, as docker commit makes
// it.
type Commit struct {
	// Parent is the configuration of the container's image, or nil if it
	// is not known: the image then has the container's layers and no
	// history before this commit.
	Parent *v1.ConfigFile
	// Layers are the container's image's layers, by diff ID, bottom first.
	Layers []string
	// Config is the container's configuration, which replaces its
	// image's, before Changes.
	Config    v1.Config
	Container string // the container's ID
	Changes   []string
	Author    string
	Message   string
	// CreatedBy is the history's account of the new layer: the container's
	// command, as Docker gives it.
	CreatedBy string
	Now       time.Time
}

// Commit records the image c describes, with diff, the container's
// writable layer as a layer's tar, on top, under ref, or untagged if ref
// is empty. A diff with nothing in it adds no layer, as with Docker. The
// caller holds the store's lock shared.
func (s *Store) Commit(c Commit, diff io.Reader, ref string) (*Image, error) {
	var target string
	if ref != "" {
		r, err := normalize(ref)
		if err != nil {
			return nil, err
		}
		target = r.Name()
	}
	cf := &v1.ConfigFile{Architecture: Platform.Architecture, OS: Platform.OS}
	if c.Parent != nil {
		cf = c.Parent.DeepCopy()
	} else {
		for _, d := range c.Layers {
			h, err := v1.NewHash(d)
			if err != nil {
				return nil, err
			}
			cf.RootFS.DiffIDs = append(cf.RootFS.DiffIDs, h)
		}
	}
	cf.RootFS.Type = "layers"
	cf.Created = v1.Time{Time: c.Now.UTC()}
	cf.Container = c.Container
	cf.Author = c.Author
	cf.Config = *c.Config.DeepCopy()
	if err := ApplyChanges(&cf.Config, c.Changes); err != nil {
		return nil, err
	}

	diffID, empty, err := s.ImportDiff(diff)
	if err != nil {
		return nil, err
	}
	h := v1.History{Created: cf.Created, CreatedBy: c.CreatedBy, Author: c.Author, Comment: c.Message, EmptyLayer: empty}
	if !empty {
		d, err := v1.NewHash(diffID)
		if err != nil {
			return nil, err
		}
		cf.RootFS.DiffIDs = append(cf.RootFS.DiffIDs, d)
	}
	cf.History = append(cf.History, h)

	raw, err := json.Marshal(cf)
	if err != nil {
		return nil, err
	}
	return s.Record(target, raw)
}

// ImportDiff stores the layer whose tar diff is, and returns its diff ID;
// a tar with nothing in it, as a step that changed no file leaves, stores
// nothing, and reports empty.
func (s *Store) ImportDiff(diff io.Reader) (diffID string, empty bool, err error) {
	empty, br, err := emptyTar(diff)
	if err != nil || empty {
		return "", empty, err
	}
	diffID, _, err = s.Layers.Import(br)
	return diffID, false, err
}

// PutConfig stores an image's configuration blob, raw, and returns its
// digest, the image's ID.
func (s *Store) PutConfig(raw []byte) (string, error) {
	d := digestOf(raw)
	return d, s.PutBlob(d, raw)
}

// Record records the image whose configuration blob raw is under ref,
// already normalized, or untagged if ref is empty. The caller holds the
// store's lock shared.
func (s *Store) Record(ref string, raw []byte) (*Image, error) {
	d, err := s.PutConfig(raw)
	if err != nil {
		return nil, err
	}
	img := &Image{Ref: ref, ConfigDigest: d, Config: raw}
	if img.Ref == "" {
		img.Ref = img.ConfigDigest
	}
	if err := s.Put(img.Ref, img); err != nil {
		return nil, err
	}
	return img, nil
}

// emptyTar reports whether the tar r holds no entry, and returns r to read
// from its start: a tar's first block is a header, or the end marker,
// all zeros.
func emptyTar(r io.Reader) (bool, io.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(512)
	if errors.Is(err, io.EOF) && len(head) == 0 {
		return true, br, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, nil, err
	}
	for _, b := range head {
		if b != 0 {
			return false, br, nil
		}
	}
	return true, br, nil
}
