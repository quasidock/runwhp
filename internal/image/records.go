package image

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/quasidock/runwhp/internal/storefile"
)

// Record is an image record as the store holds it.
type Record struct {
	*Image        // nil if the record cannot be read
	Path   string // the record's file
	Pulled time.Time
	// Complete is true if the record could be read and every layer it lists
	// is in the layer store, which is when Get finds it.
	Complete bool
	Err      error // why the record cannot be read
}

// ErrNoSuchImage means no record has the reference or ID asked for.
var ErrNoSuchImage = errors.New("no such image")

// ErrAmbiguous means an ID prefix matches more than one image.
var ErrAmbiguous = errors.New("ambiguous image ID prefix")

// List returns every record in the store, newest image first, records that
// cannot be read last. It skips records being written.
func (s *Store) List() ([]*Record, error) {
	dir := filepath.Join(s.Root, "images")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		out = append(out, s.readRecord(filepath.Join(dir, e.Name())))
	}
	created := func(r *Record) time.Time {
		if r.Image == nil {
			return time.Time{}
		}
		return r.Created()
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Image != nil, out[j].Image != nil; a != b {
			return a
		}
		if a, b := created(out[i]), created(out[j]); !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

func (s *Store) readRecord(path string) *Record {
	r := &Record{Path: path}
	if info, err := os.Stat(path); err == nil {
		r.Pulled = info.ModTime()
	}
	data, err := storefile.Read(path)
	if err != nil {
		r.Err = err
		return r
	}
	var img Image
	if err := json.Unmarshal(data, &img); err != nil {
		r.Err = fmt.Errorf("image record %s: %w", filepath.Base(path), err)
		return r
	}
	layers, err := img.Layers()
	if err != nil || img.Ref == "" {
		if err == nil {
			err = fmt.Errorf("image record %s names no reference", filepath.Base(path))
		}
		r.Err = err
		return r
	}
	r.Image = &img
	r.Complete = true
	for _, d := range layers {
		if !s.Layers.Has(d) {
			r.Complete = false
		}
	}
	return r
}

// Find returns the records arg names: the one pulled by that reference, or
// else every record whose ID is arg or starts with it, with or without
// "sha256:". A prefix that matches two different IDs is ambiguous.
func (s *Store) Find(arg string) ([]*Record, error) {
	if r, err := normalize(arg); err == nil {
		path := s.recordPath(r)
		if _, err := os.Stat(path); err == nil {
			rec := s.readRecord(path)
			if rec.Err != nil {
				return nil, rec.Err
			}
			return []*Record{rec}, nil
		}
	}
	prefix := strings.TrimPrefix(arg, "sha256:")
	if _, err := hex.DecodeString(prefix + strings.Repeat("0", len(prefix)%2)); err != nil || prefix == "" || len(prefix) > 64 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchImage, arg)
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*Record
	id := ""
	for _, rec := range all {
		if rec.Image == nil || !strings.HasPrefix(strings.TrimPrefix(rec.ID(), "sha256:"), prefix) {
			continue
		}
		if id != "" && rec.ID() != id {
			return nil, fmt.Errorf("%w: %s", ErrAmbiguous, arg)
		}
		id = rec.ID()
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchImage, arg)
	}
	return out, nil
}

// Remove removes a record. Its layers stay: the layer store's files are
// shared, and are removed by whatever knows who else uses them.
func (s *Store) Remove(rec *Record) error {
	if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Put records img as pulled by ref, as Pull does. It is for tests and for
// whatever brings images in other than by pulling them.
func (s *Store) Put(ref string, img *Image) error {
	r, err := normalize(ref)
	if err != nil {
		return err
	}
	return s.writeRecord(r, img)
}

// Familiar writes a normalized reference as Docker shows it, without
// Docker Hub's registry and its library namespace: alpine:3.20 for
// index.docker.io/library/alpine:3.20.
func Familiar(ref string) string {
	for _, p := range []string{"index.docker.io/", "docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			return strings.TrimPrefix(rest, "library/")
		}
	}
	return ref
}

// Display splits a normalized reference into the repository and tag
// columns Docker lists images with. An image pulled by digest has no tag.
func Display(ref string) (repo, tag string) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ref, "<none>"
	}
	repo = Familiar(r.Context().Name())
	if t, ok := r.(name.Tag); ok {
		return repo, t.TagStr()
	}
	return repo, "<none>"
}
