//go:build windows

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
)

// TestTag names an image anew, runs it by that name and by its ID, and
// removes one name of two.
func TestTag(t *testing.T) {
	parallel(t)
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
	parallel(t)
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
	parallel(t)
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
	parallel(t)
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

// serveImage serves img at repo from a registry of its own on localhost,
// which stop stops.
func serveImage(t *testing.T, img v1.Image, repo string) (ref string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref = strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1) + "/" + repo
	if err := remote.Write(parseRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	return ref, srv.Close
}

// checkArchive reads a saved archive as go-containerregistry does, and
// checks each image in it has its ID and its layers their diffIDs.
func checkArchive(t *testing.T, path string, ids map[string]string) {
	t.Helper()
	for ref, id := range ids {
		tag, err := name.NewTag(ref)
		if err != nil {
			t.Fatal(err)
		}
		img, err := tarball.ImageFromPath(path, &tag)
		if err != nil {
			t.Fatalf("%s in %s: %v", ref, path, err)
		}
		if got, _ := img.ConfigName(); got.String() != id {
			t.Errorf("%s: ID %s, want %s", ref, got, id)
		}
		cf, _ := img.ConfigFile()
		ls, _ := img.Layers()
		for i, l := range ls {
			rc, err := l.Uncompressed()
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			io.Copy(h, rc)
			rc.Close()
			if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != cf.RootFS.DiffIDs[i].String() {
				t.Errorf("%s layer %d: %s, want %s", ref, i, got, cf.RootFS.DiffIDs[i])
			}
		}
	}
}

func manifestOf(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("%s has no manifest.json: %v", path, err)
		}
		if hdr.Name == "manifest.json" {
			var m []map[string]any
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
}

// TestSaveLoad saves two images, checks the archive as another tool reads
// it, loads it back with the same IDs, and saves by ID and by repository.
func TestSaveLoad(t *testing.T) {
	parallel(t)
	s := newOwnStore(t)
	alpine, python := mirrored(t, alpine), mirrored(t, "python:3.12-slim")
	s.ok("pull", alpine)
	s.ok("pull", python)
	ids := map[string]string{alpine: configDigest(t, alpine), python: configDigest(t, python)}
	out := filepath.Join(t.TempDir(), "saved.tar")
	s.ok("save", "-o", out, alpine, python)
	checkArchive(t, out, ids)
	data, _ := os.ReadFile(out)
	if r := s.cli("", "save", alpine, python); r.code != 0 || r.stdout != string(data) {
		t.Errorf("save to stdout: exit %d, %d bytes, not the %d of the file", r.code, len(r.stdout), len(data))
	}

	s.ok("rmi", alpine, python)
	if r := s.ok("load", "-i", out); r.stdout != "Loaded image: "+alpine+"\nLoaded image: "+python+"\n" {
		t.Errorf("load: %q", r.stdout)
	}
	for ref, id := range ids {
		if r := s.ok("images", "-q", ref); r.stdout != id[7:19]+"\n" {
			t.Errorf("%s loaded as %q, want %s", ref, r.stdout, id[7:19])
		}
	}
	if r := s.ok(runArgs(python, "python3", "-c", "print(2*21)")...); r.stdout != "42\n" {
		t.Errorf("run of the loaded image: %q", r.stdout)
	}

	// By ID: no name, in the archive or once loaded.
	byID := filepath.Join(t.TempDir(), "id.tar")
	s.ok("save", "-o", byID, ids[alpine][7:19])
	if m := manifestOf(t, byID); len(m) != 1 || m[0]["RepoTags"] != nil {
		t.Errorf("manifest.json of an image saved by ID: %v", m)
	}
	other := newOwnStore(t)
	if r := other.ok("load", "-i", byID); r.stdout != "Loaded image ID: "+ids[alpine]+"\n" {
		t.Errorf("load of an image saved by ID: %q", r.stdout)
	}
	if r := other.ok("images"); !strings.Contains(r.stdout, "<none>") {
		t.Errorf("images:\n%s", r.stdout)
	}
	other.ok("image", "prune", "-f")
	if r := other.ok("images", "-q"); r.stdout != "" {
		t.Errorf("image prune left %q", r.stdout)
	}

	// A repository alone: every tag of it.
	s.ok("tag", alpine, "mine/app:1")
	s.ok("tag", alpine, "mine/app:2")
	byRepo := filepath.Join(t.TempDir(), "repo.tar")
	s.ok("save", "-o", byRepo, "mine/app")
	if m := manifestOf(t, byRepo); len(m) != 1 || fmt.Sprint(m[0]["RepoTags"]) != "[mine/app:1 mine/app:2]" {
		t.Errorf("manifest.json of a repository: %v", m)
	}
	if r := s.cli("", "save", "-o", byRepo, "nothere:1"); r.code != 1 {
		t.Errorf("save of a missing image: %+v", r)
	}
}

// TestSaveBackfill saves images whose layers were stored before runwhp
// kept their tars' metadata: save fetches it from the registry, and fails
// when the registry is gone.
func TestSaveBackfill(t *testing.T) {
	parallel(t)
	s := newOwnStore(t)
	path, _ := mirrorArchive(t, alpine)
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, stop := serveImage(t, img, "e2e/backfill:1")
	s.ok("pull", ref)
	want := filepath.Join(t.TempDir(), "want.tar")
	s.ok("save", "-o", want, ref)
	forget := func() {
		splits, _ := filepath.Glob(filepath.Join(s.root, "layers", "sha256", "*.tar-split.gz"))
		for _, p := range splits {
			os.Remove(p)
		}
		os.RemoveAll(filepath.Join(s.root, "blobs"))
	}
	forget()
	got := filepath.Join(t.TempDir(), "got.tar")
	r := s.ok("save", "-o", got, ref)
	if !strings.Contains(r.stderr, "Fetching tar metadata for 1 layers") {
		t.Errorf("save said: %q", r.stderr)
	}
	a, _ := os.ReadFile(want)
	b, _ := os.ReadFile(got)
	if !bytes.Equal(a, b) {
		t.Error("the archive saved after fetching the metadata differs")
	}
	forget()
	stop()
	if r := s.cli("", "save", "-o", got, ref); r.code != 1 || !strings.Contains(r.stderr, "tar metadata") {
		t.Errorf("save with the registry gone: %+v", r)
	}
	if _, err := os.Stat(got); err != nil {
		t.Error("a save that failed removed the archive already there")
	}
}

// TestSaveManyLayers saves an image of more layers than one guest can
// serve.
func TestSaveManyLayers(t *testing.T) {
	parallel(t)
	s := newOwnStore(t)
	path, _ := mirrorArchive(t, busybox)
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var layers []v1.Layer
	for i := range 70 {
		layers = append(layers, e2eLayer(t, fmt.Sprintf("layer%02d", i), fmt.Sprintf("content of layer %d\n", i)))
	}
	if img, err = mutate.AppendLayers(img, layers...); err != nil {
		t.Fatal(err)
	}
	ref, _ := serveImage(t, img, "e2e/many:1")
	s.ok("pull", ref)
	out := filepath.Join(t.TempDir(), "many.tar")
	s.ok("save", "-o", out, ref)
	id, _ := img.ConfigName()
	checkArchive(t, out, map[string]string{ref: id.String()})
}

// e2eLayer is a layer holding one file.
func e2eLayer(t *testing.T, name, body string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestSavePrune prunes everything, over and over, while an image is
// saved: the prunes wait for the save, which keeps the image's layers.
func TestSavePrune(t *testing.T) {
	parallel(t)
	s := newOwnStore(t)
	python := mirrored(t, "python:3.12-slim")
	s.ok("pull", python)
	out := filepath.Join(t.TempDir(), "saved.tar")
	cmd := command(t, "save", "-o", out, python)
	cmd.Env = append(cmd.Env, "RUNWHP_ROOT="+s.root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	prunes := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("save during prunes: %v\n%s", err, stderr.String())
			}
			if prunes == 0 {
				t.Log("the save ended before a prune started")
			}
			checkArchive(t, out, map[string]string{python: configDigest(t, python)})
			return
		default:
			s.cli("", "system", "prune", "-a", "-f")
			prunes++
		}
	}
}
