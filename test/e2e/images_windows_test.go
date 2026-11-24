//go:build windows

package e2e

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
)

// TestTag names an image anew, runs it by that name and by its ID, and
// removes one name of two.
func TestTag(t *testing.T) {
	s := newOwnStore(t)
	alpine := mirrored(t, alpine)
	s.ok("pull", alpine)
	id := configDigest(t, alpine)

	s.ok("tag", alpine, "mine/app:1")
	if r := s.ok(runArgs("mine/app:1", "cat", "/etc/alpine-release")...); !strings.HasPrefix(r.stdout, "3.20") {
		t.Errorf("run by the new tag: %q", r.stdout)
	}
	if r := s.ok(runArgs(id[7:19], "cat", "/etc/alpine-release")...); !strings.HasPrefix(r.stdout, "3.20") {
		t.Errorf("run by ID: %q", r.stdout)
	}
	rows := map[string][]string{}
	for _, l := range strings.Split(s.ok("images", "--digests").stdout, "\n")[1:] {
		if f := strings.Fields(l); len(f) > 3 {
			rows[f[0]+":"+f[1]] = f
		}
	}
	mine, pulled := rows["mine/app:1"], rows[repoOf(alpine)+":3.20"]
	if mine == nil || pulled == nil || mine[3] != id[7:19] || pulled[3] != id[7:19] {
		t.Fatalf("images --digests: %v", rows)
	}
	if mine[2] != "<none>" || !strings.HasPrefix(pulled[2], "sha256:") {
		t.Errorf("digests: the tag's %s, the pull's %s", mine[2], pulled[2])
	}
	layers := s.layers()
	if r := s.ok("rmi", "mine/app:1"); !strings.Contains(r.stdout, "Untagged: mine/app:1") || strings.Contains(r.stdout, "Deleted") {
		t.Errorf("rmi of one tag of two:\n%s", r.stdout)
	}
	if got := s.layers(); len(got) != len(layers) {
		t.Errorf("rmi of one tag removed layers: %v, then %v", layers, got)
	}
	s.ok(runArgs(alpine, "true")...)
	if r := s.cli("", "tag", "nothere:1", "x:1"); r.code != 1 || !strings.Contains(r.stderr, "no such image") {
		t.Errorf("tag of a missing image: %+v", r)
	}
}

// TestHistory shows how alpine was built, with its layer's size as Docker
// gives it: the size of its files.
func TestHistory(t *testing.T) {
	s := newOwnStore(t)
	alpine := mirrored(t, alpine)
	s.ok("pull", alpine)
	id := configDigest(t, alpine)

	lines := strings.Split(strings.TrimSpace(s.ok("history", "-H=false", alpine).stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "IMAGE") || !strings.HasPrefix(lines[1], id[7:19]) || !strings.HasPrefix(lines[2], "<missing>") {
		t.Fatalf("history:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], `CMD ["/bin/sh"]`) || !strings.Contains(lines[2], "ADD alpine-minirootfs") {
		t.Errorf("history:\n%s", strings.Join(lines, "\n"))
	}
	// The size Docker 29 gave alpine:3.20's layer.
	if f := strings.Fields(lines[2]); !contains(f, "7807582") {
		t.Errorf("the layer's size: %q", lines[2])
	}
	// A layer pulled before runwhp kept its tar's metadata shows its ext4
	// image's size instead.
	splits, _ := filepath.Glob(filepath.Join(s.root, "layers", "sha256", "*.tar-split.gz"))
	for _, p := range splits {
		os.Remove(p)
	}
	lines = strings.Split(strings.TrimSpace(s.ok("history", "-H=false", alpine).stdout), "\n")
	if f := strings.Fields(lines[2]); contains(f, "7807582") {
		t.Errorf("the size without metadata: %q", lines[2])
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// mirrorArchive is the archive the mirror serves ref from, as
// go-containerregistry wrote it: Docker's manifest.json layout, with
// gzipped layers.
func mirrorArchive(t *testing.T, ref string) (path, id string) {
	t.Helper()
	mirrored(t, ref)
	path, err := filepath.Abs(filepath.Join("..", "..", "out", "e2e-images", strings.NewReplacer("/", "_", ":", "_").Replace(ref)+".tar"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.ConfigName()
	return path, d.String()
}

// TestLoadArchives loads the mirror's archives, from a file and from
// stdin, and runs what they hold.
func TestLoadArchives(t *testing.T) {
	s := newOwnStore(t)
	for _, ref := range []string{alpine, busybox} {
		path, id := mirrorArchive(t, ref)
		if r := s.ok("load", "-i", path); r.stdout != "Loaded image: "+ref+"\n" {
			t.Errorf("load of %s: %q", ref, r.stdout)
		}
		if r := s.ok("images", "-q", ref); r.stdout != id[7:19]+"\n" {
			t.Errorf("%s's ID: %q, want %s", ref, r.stdout, id[7:19])
		}
	}
	if r := s.ok(runArgs(alpine, "cat", "/etc/alpine-release")...); !strings.HasPrefix(r.stdout, "3.20") {
		t.Errorf("run of the loaded image: %q", r.stdout)
	}
	s.ok("rmi", busybox)
	path, _ := mirrorArchive(t, busybox)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.cli(string(data), "load", "-q"); r.code != 0 || r.stdout != "Loaded image: "+busybox+"\n" {
		t.Errorf("load from stdin: %+v", r)
	}
	if r := s.ok(runArgs(busybox, "echo", "ok")...); r.stdout != "ok\n" {
		t.Errorf("run of the image loaded from stdin: %q", r.stdout)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.root, "tmp")); len(entries) != 0 {
		t.Errorf("load left %d files in tmp", len(entries))
	}
}

// TestImport makes images of alpine's root file system, plain and
// compressed, from a file and from stdin, and runs them.
func TestImport(t *testing.T) {
	s := newOwnStore(t)
	path, _ := mirrorArchive(t, alpine)
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := img.Layers()
	rc, _ := ls[0].Uncompressed()
	plain, _ := io.ReadAll(rc)
	rc.Close()
	diffID, _ := ls[0].DiffID()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	zstdWriter, _ := zstd.NewWriter(nil)
	dir := t.TempDir()
	files := map[string][]byte{"rootfs.tar": plain, "rootfs.tar.gz": gz.Bytes(), "rootfs.tar.zst": zstdWriter.EncodeAll(plain, nil)}
	for n, data := range files {
		os.WriteFile(filepath.Join(dir, n), data, 0o644)
	}

	for n := range files {
		ref := "imported:" + strings.ReplaceAll(n, ".", "-")
		r := s.ok("import", "-c", `CMD ["cat","/etc/alpine-release"]`, "-c", "ENV A=b", filepath.Join(dir, n), ref)
		if id := strings.TrimSpace(r.stdout); !strings.HasPrefix(id, "sha256:") || len(id) != 71 {
			t.Errorf("import of %s printed %q", n, r.stdout)
		}
		if r := s.ok(runArgs(ref)...); !strings.HasPrefix(r.stdout, "3.20") {
			t.Errorf("run of the image imported from %s: %q", n, r.stdout)
		}
		if !strings.Contains(s.ok("image", "inspect", ref).stdout, diffID.String()) {
			t.Errorf("the image imported from %s does not have alpine's layer", n)
		}
	}
	if r := s.ok(runArgs("imported:rootfs-tar", "sh", "-c", "echo $A")...); r.stdout != "b\n" {
		t.Errorf("ENV: %q", r.stdout)
	}
	if r := s.cli(string(plain), "import", "-m", "from stdin", "-", "imported:stdin"); r.code != 0 {
		t.Fatalf("import from stdin: %+v", r)
	}
	if r := s.ok("history", "imported:stdin"); !strings.Contains(r.stdout, "from stdin") {
		t.Errorf("history:\n%s", r.stdout)
	}
	if r := s.ok("history", "imported:rootfs-tar"); !strings.Contains(r.stdout, "Imported from -") {
		t.Errorf("history:\n%s", r.stdout)
	}
	xz := filepath.Join(dir, "rootfs.tar.xz")
	os.WriteFile(xz, []byte{0xfd, '7', 'z', 'X', 'Z', 0, 0, 0}, 0o644)
	if r := s.cli("", "import", xz); r.code != 1 || !strings.Contains(r.stderr, "xz") {
		t.Errorf("import of xz: %+v", r)
	}
}
