package buildcache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEntries(t *testing.T) {
	s := Open(t.TempDir())
	if es, err := s.List(); err != nil || len(es) != 0 {
		t.Fatalf("an empty cache: %v, %v", es, err)
	}
	key := strings.Repeat("ab", 32)
	if _, ok := s.Get(key); ok {
		t.Error("found an entry in an empty cache")
	}
	if err := s.Put(Entry{Key: "short"}); err == nil {
		t.Error("a bad key was put")
	}
	e := Entry{Key: key, CreatedBy: "RUN true # buildkit", DiffID: "sha256:" + strings.Repeat("cd", 32), Created: time.Unix(1, 0).UTC()}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(e); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, ok := s.Get(key)
	if !ok || got.DiffID != e.DiffID || !got.Created.Equal(e.Created) {
		t.Errorf("get: %+v, %v", got, ok)
	}
	// An entry that cannot be read is skipped.
	os.WriteFile(filepath.Join(s.dir, "entries", strings.Repeat("ef", 32)+".json"), []byte("{"), 0o644)
	if es, err := s.List(); err != nil || len(es) != 1 {
		t.Errorf("list: %v, %v", es, err)
	}
	if err := s.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(key); err != nil {
		t.Errorf("removing a missing entry: %v", err)
	}
	if _, ok := s.Get(key); ok {
		t.Error("a removed entry was found")
	}
}
