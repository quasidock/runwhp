package layer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// putTar stores a layer holding one file and returns its diffID.
func putTar(t *testing.T, s *Store, body string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	sum := sha256.Sum256(buf.Bytes())
	diffID := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := s.Put(diffID, &buf); err != nil {
		t.Fatal(err)
	}
	return diffID
}

// newStore is an empty store, under RUNWHP_TEST_DIR when it is set, so
// that the tests can run on another file system, such as ReFS.
func newStore(t *testing.T) *Store {
	root := t.TempDir()
	if base := os.Getenv("RUNWHP_TEST_DIR"); base != "" {
		os.MkdirAll(base, 0o755)
		dir, err := os.MkdirTemp(base, "layer")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		root = dir
	}
	return &Store{Dir: filepath.Join(root, "layers"), Tmp: filepath.Join(root, "tmp")}
}

func TestListAndRemove(t *testing.T) {
	s := newStore(t)
	if got, err := s.List(); err != nil || len(got) != 0 {
		t.Errorf("List of an empty store: %v, %v", got, err)
	}
	a, b := putTar(t, s, "a"), putTar(t, s, "b")
	got, err := s.List()
	sort.Strings(got)
	want := []string{a, b}
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, %v; want %v", got, err, want)
	}
	if err := s.Remove(a); err != nil || s.Has(a) || !s.Has(b) {
		t.Errorf("Remove: %v; has a %v, has b %v", err, s.Has(a), s.Has(b))
	}
	if err := s.Remove(a); err != nil {
		t.Errorf("removing a missing layer: %v", err)
	}
	if err := s.Remove("md5:00"); err == nil {
		t.Error("removed a layer by a digest that is not sha256")
	}
}
