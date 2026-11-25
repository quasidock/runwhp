// Package buildcache keeps runwhp build's cache: an entry for each step
// that made a layer, or found nothing to change, keyed by what the step
// was given, and the volumes RUN --mount=type=cache mounts.
//
// Entries live in <root>/buildcache/entries, one JSON file each, named by
// their key, and are replaced whole, so concurrent builds may write the
// same entry. An entry names its layer by diff ID; the layer itself is in
// the image store, where the entry keeps it from being pruned. Cache
// mounts are journaled ext4 volumes in <root>/buildcache/volumes, named
// by their id.
package buildcache

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/storefile"
)

// Entry is one step's result.
type Entry struct {
	// Key is the step's cache key: 64 hex digits.
	Key string `json:"key"`
	// CreatedBy is the step, as the image's history gives it.
	CreatedBy string `json:"createdBy"`
	// DiffID is the layer the step made, or empty if it changed nothing.
	DiffID string `json:"diffID,omitempty"`
	// Created is when the step first made its layer, which the history of
	// every image built with it gives.
	Created time.Time `json:"created"`
	// Used is when a build last used the entry.
	Used time.Time `json:"used"`
}

// Store is the build cache of a runwhp store.
type Store struct {
	dir string
}

// Open opens the build cache of the store at root.
func Open(root string) *Store { return &Store{dir: filepath.Join(root, "buildcache")} }

var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) entryPath(key string) string {
	return filepath.Join(s.dir, "entries", key+".json")
}

// Get returns the entry for key, if there is a readable one.
func (s *Store) Get(key string) (Entry, bool) {
	if !keyPattern.MatchString(key) {
		return Entry{}, false
	}
	data, err := storefile.Read(s.entryPath(key))
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(data, &e) != nil || e.Key != key {
		return Entry{}, false
	}
	return e, true
}

// Put records e, replacing any entry with its key.
func (s *Store) Put(e Entry) error {
	if !keyPattern.MatchString(e.Key) {
		return errors.New("bad build cache key " + e.Key)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "entries"), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return storefile.Replace(s.entryPath(e.Key), data)
}

// List returns every readable entry, sorted by key.
func (s *Store) List() ([]Entry, error) {
	des, err := os.ReadDir(filepath.Join(s.dir, "entries"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		key, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok {
			continue
		}
		if e, ok := s.Get(key); ok {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// Remove removes the entry for key; a missing one is no error.
func (s *Store) Remove(key string) error {
	if !keyPattern.MatchString(key) {
		return nil
	}
	err := os.Remove(s.entryPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// VolumeRoot is the volume store the cache mounts are in, as a
// container.Volume's Root names it: their volumes are below it.
func (s *Store) VolumeRoot() string { return s.dir }
