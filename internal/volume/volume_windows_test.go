//go:build windows

package volume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
)

func testRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	os.MkdirAll(base, 0o755)
	dir, err := os.MkdirTemp(base, "volume")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"data", "pg-data", "my_vol.v2", "A1", "0x", strings.Repeat("a", 128)} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for name, msg := range map[string]string{
		"a": "two or more", "-ab": "starting with", ".ab": "starting with", "a/b": "letters", `a\b`: "letters",
		"a:b": "letters", "a b": "letters", "": "two or more", "data.": "trailing dot",
		"con": "reserves", "NUL": "reserves", "com1": "reserves", "LPT9.log": "reserves", "aux.tar.gz": "reserves",
		strings.Repeat("a", 129): "128",
	} {
		if err := CheckName(name); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("CheckName(%q) = %v, want an error about %q", name, err, msg)
		}
	}
	// Names that only start like a device name are fine.
	for _, ok := range []string{"console", "com10", "nullable", "auxiliary"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
}

func TestCreateGetList(t *testing.T) {
	s := Open(testRoot(t))
	if vs, err := s.List(); err != nil || len(vs) != 0 {
		t.Fatalf("a new store lists %v, %v", vs, err)
	}
	v, err := s.Create("Data", 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "Data" || v.Size != 2<<30 || v.CreatedAt.IsZero() {
		t.Errorf("created %+v", v)
	}
	f, err := os.Open(v.Disk())
	if err != nil {
		t.Fatal(err)
	}
	st, err := ext4.ReadState(f)
	f.Close()
	if err != nil || !st.Clean || !st.Journal {
		t.Errorf("a new volume's file system: %+v, %v", st, err)
	}
	if n, err := v.OnDisk(); err != nil || n > 1<<20 {
		t.Errorf("a new 2 GiB volume takes %d bytes (%v)", n, err)
	}
	if busy, err := v.InUse(); err != nil || busy {
		t.Errorf("InUse = %v, %v", busy, err)
	}

	if _, err := s.Create("data", 1<<30); !errors.Is(err, ErrExists) {
		t.Errorf("creating data when Data exists: %v", err)
	}
	if g, err := s.Get("DATA"); err != nil || g.Name != "Data" {
		t.Errorf("Get(DATA) = %+v, %v", g, err)
	}
	if _, err := s.Get("other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(other) = %v", err)
	}
	s.Create("alpha", 1<<30)
	vs, err := s.List()
	if err != nil || len(vs) != 2 || vs[0].Name != "alpha" || vs[1].Name != "Data" {
		t.Errorf("List = %v, %v", vs, err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "tmp"))
	if len(entries) != 0 {
		t.Errorf("tmp holds %d entries after creating volumes", len(entries))
	}
}

func TestEnsureConcurrent(t *testing.T) {
	s := Open(testRoot(t))
	var wg sync.WaitGroup
	created := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, made, err := s.Ensure("shared", 1<<30)
			if err != nil || v.Name != "shared" {
				t.Errorf("Ensure = %+v, %v", v, err)
			}
			created <- made
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for made := range created {
		if made {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d of 8 concurrent Ensure calls created the volume", n)
	}
}

func TestRemove(t *testing.T) {
	s := Open(testRoot(t))
	v, err := s.Create("gone", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	f, err := disk.Open(v.Disk(), disk.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := v.InUse(); err != nil || !busy {
		t.Errorf("InUse while open = %v, %v", busy, err)
	}
	if err := s.Remove("gone"); !errors.Is(err, ErrInUse) {
		t.Errorf("removing a volume in use: %v", err)
	}
	if _, err := s.Get("gone"); err != nil {
		t.Errorf("the volume in use is no longer there: %v", err)
	}
	f.Close()
	if err := s.Remove("GONE"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Remove: %v", err)
	}
	if err := s.Remove("gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it again: %v", err)
	}
}

func TestLabels(t *testing.T) {
	s := Open(testRoot(t))
	if _, err := s.CreateWith("labelled", 64<<20, map[string]string{"com.docker.compose.volume": "data"}); err != nil {
		t.Fatal(err)
	}
	v, made, err := s.EnsureWith("ensured", 64<<20, map[string]string{"a": "1"})
	if err != nil || !made || v.Labels["a"] != "1" {
		t.Fatalf("EnsureWith: %+v, %v, %v", v, made, err)
	}
	// An existing volume keeps its labels.
	if v, made, _ := s.EnsureWith("ensured", 64<<20, map[string]string{"a": "2"}); made || v.Labels["a"] != "1" {
		t.Errorf("EnsureWith again: %+v, %v", v, made)
	}
	list, _ := s.List()
	if len(list) != 2 || list[1].Labels["com.docker.compose.volume"] != "data" {
		t.Errorf("listed %+v", list)
	}
}
