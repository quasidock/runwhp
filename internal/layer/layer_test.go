package layer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
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

// oddTar is a layer with what real layers hold: directories, a symlink, a
// hard link, a whiteout, an opaque directory, a name that is not UTF-8,
// PAX records, and padding after the end-of-archive marker.
func oddTar(t *testing.T) ([]byte, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := map[string]string{
		"etc/os-release":      "ID=test\n",
		"./usr/bin/tool":      strings.Repeat("x", 70000),
		"usr/lib/\xff\xfeodd": "not utf-8",
	}
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o755})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "usr/", Mode: 0o755, Uname: "root", Gname: "root"})
	for _, n := range []string{"etc/os-release", "./usr/bin/tool", "usr/lib/\xff\xfeodd"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Format: tar.FormatPAX,
			PAXRecords: map[string]string{"SCHILY.xattr.user.k": "v"}})
		tw.Write([]byte(files[n]))
	}
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "usr/bin/tool2", Linkname: "usr/bin/tool"})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "usr/bin/sh", Linkname: "tool"})
	tw.WriteHeader(&tar.Header{Name: "etc/.wh.gone", Mode: 0o644})
	tw.WriteHeader(&tar.Header{Name: "usr/lib/.wh..wh..opq", Mode: 0o644})
	tw.WriteHeader(&tar.Header{Name: "empty", Mode: 0o600})
	tw.Close()
	buf.Write(make([]byte, 4096)) // what some tar writers leave after the end
	return buf.Bytes(), files
}

func diffIDOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// mapGetter serves file contents by name, cleaned as tar2ext4 cleans them.
type mapGetter map[string]string

func (m mapGetter) Get(name string) (io.ReadCloser, error) {
	c, ok := m[path.Clean("/" + name)[1:]]
	if !ok {
		return nil, fmt.Errorf("%q: not found", name)
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

// reassemble rebuilds a layer's tar from its metadata and its files.
func reassemble(t *testing.T, s *Store, diffID string, files map[string]string) []byte {
	t.Helper()
	m := mapGetter{}
	for n, c := range files {
		m[path.Clean("/" + n)[1:]] = c
	}
	r, err := s.Split(diffID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := asm.NewOutputTarStream(m, storage.NewJSONUnpacker(r))
	defer out.Close()
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSplitRoundTrip(t *testing.T) {
	s := newStore(t)
	b, files := oddTar(t)
	d := diffIDOf(b)
	if _, err := s.Put(d, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if !s.Has(d) || !s.HasSplit(d) {
		t.Fatalf("after Put: ext4 %v, metadata %v", s.Has(d), s.HasSplit(d))
	}
	if got := reassemble(t, s, d, files); !bytes.Equal(got, b) {
		t.Errorf("reassembled %d bytes, digest %s; want %d bytes, %s", len(got), diffIDOf(got), len(b), d)
	}
	// A file whose contents differ is caught by its checksum.
	files["etc/os-release"] = "ID=other\n"
	r, _ := s.Split(d)
	defer r.Close()
	m := mapGetter{}
	for n, c := range files {
		m[path.Clean("/" + n)[1:]] = c
	}
	if _, err := io.ReadAll(asm.NewOutputTarStream(m, storage.NewJSONUnpacker(r))); err == nil {
		t.Error("reassembled a layer from a changed file")
	}
}

func TestPutWrongDigest(t *testing.T) {
	s := newStore(t)
	b, _ := oddTar(t)
	d := diffIDOf(append(b, 0))
	if _, err := s.Put(d, bytes.NewReader(b)); err == nil {
		t.Fatal("stored a layer whose digest is not its diffID")
	}
	if s.Has(d) || s.HasSplit(d) {
		t.Error("a layer that failed left files")
	}
	if err := s.PutSplit(d, bytes.NewReader(b)); err == nil || s.HasSplit(d) {
		t.Errorf("PutSplit of the wrong tar: %v, left %v", err, s.HasSplit(d))
	}
	if _, err := s.Put(d, bytes.NewReader([]byte("not a tar at all, not even close........"))); err == nil {
		t.Error("stored junk")
	}
}

// TestSplitAdded gives a layer stored before runwhp kept tar metadata its
// metadata, as `save` does from the registry and Put does when a layer is
// loaded again.
func TestSplitAdded(t *testing.T) {
	for _, how := range []string{"PutSplit", "Put"} {
		t.Run(how, func(t *testing.T) {
			s := newStore(t)
			b, files := oddTar(t)
			d := diffIDOf(b)
			p, err := s.Put(d, bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			sp, _ := s.SplitPath(d)
			os.Remove(sp)
			if _, err := s.Split(d); !errors.Is(err, ErrNoSplit) {
				t.Fatalf("Split of a layer without metadata: %v", err)
			}
			before, _ := os.Stat(p)
			if how == "PutSplit" {
				err = s.PutSplit(d, bytes.NewReader(b))
			} else {
				_, err = s.Put(d, bytes.NewReader(b))
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(p)
			if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				t.Error("the ext4 image was written again")
			}
			if got := reassemble(t, s, d, files); !bytes.Equal(got, b) {
				t.Error("metadata added later does not reassemble the layer")
			}
		})
	}
}

func TestImport(t *testing.T) {
	s := newStore(t)
	b, files := oddTar(t)
	d, p, err := s.Import(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if want := diffIDOf(b); d != want {
		t.Fatalf("Import = %s; want %s", d, want)
	}
	if want, _ := s.Path(d); p != want || !s.Has(d) {
		t.Errorf("Import put the layer at %s; has %v", p, s.Has(d))
	}
	if got := reassemble(t, s, d, files); !bytes.Equal(got, b) {
		t.Error("an imported layer does not reassemble")
	}
	if entries, _ := os.ReadDir(s.Tmp); len(entries) != 0 {
		t.Errorf("Import left %d files in tmp", len(entries))
	}
}

func TestDamagedSplit(t *testing.T) {
	s := newStore(t)
	d := putTar(t, s, "a")
	sp, _ := s.SplitPath(d)
	os.WriteFile(sp, []byte("junk"), 0o644)
	if r, err := s.Split(d); err == nil {
		r.Close()
		t.Error("opened junk as tar metadata")
	}
}

func TestListAndRemoveSplits(t *testing.T) {
	s := newStore(t)
	a, b := putTar(t, s, "a"), putTar(t, s, "b")
	// a without metadata, as layers stored before iteration 14 are; b's
	// metadata alone, as a remove that stopped halfway leaves it.
	sp, _ := s.SplitPath(a)
	os.Remove(sp)
	p, _ := s.Path(b)
	os.Remove(p)
	got, err := s.List()
	sort.Strings(got)
	want := []string{a, b}
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, %v; want %v", got, err, want)
	}
	for _, d := range want {
		if err := s.Remove(d); err != nil {
			t.Errorf("Remove %s: %v", d, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Dir, "sha256")); len(entries) != 0 {
		t.Errorf("Remove left %d files", len(entries))
	}
}

func TestDiffSize(t *testing.T) {
	s := newStore(t)
	b, files := oddTar(t)
	d := diffIDOf(b)
	s.Put(d, bytes.NewReader(b))
	// The files, and the symlink usr/bin/sh's target, "tool"; the hard
	// link counts once.
	want := int64(len("tool"))
	for _, c := range files {
		want += int64(len(c))
	}
	if n, err := s.DiffSize(d); err != nil || n != want {
		t.Errorf("DiffSize = %d, %v; want %d", n, err, want)
	}
	sp, _ := s.SplitPath(d)
	os.Remove(sp)
	if _, err := s.DiffSize(d); !errors.Is(err, ErrNoSplit) {
		t.Errorf("DiffSize without metadata: %v", err)
	}
}
