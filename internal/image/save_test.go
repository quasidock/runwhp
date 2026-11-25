package image

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/quasidock/runwhp/internal/layer"
)

// mapSource serves layers' files from their tars, as the guest serves
// them from their ext4 images: by cleaned name, the last entry winning.
type mapSource struct {
	files map[string]map[string][]byte // diffID -> name -> contents
	lie   string                       // a name to serve wrong contents for
}

func newMapSource(t *testing.T, imgs ...v1.Image) *mapSource {
	t.Helper()
	m := &mapSource{files: map[string]map[string][]byte{}}
	for _, img := range imgs {
		ls, _ := img.Layers()
		for _, l := range ls {
			d, _ := l.DiffID()
			rc, _ := l.Uncompressed()
			files := map[string][]byte{}
			tr := tar.NewReader(rc)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(tr)
				files[path.Clean("/" + hdr.Name)[1:]] = b
			}
			rc.Close()
			m.files[d.String()] = files
		}
	}
	return m
}

type mapFiles struct {
	m     *mapSource
	files map[string][]byte
	names []string
	i     int
}

func (m *mapSource) Open(diffID string, names []string, sizes []int64) (LayerFiles, error) {
	files, ok := m.files[diffID]
	if !ok {
		return nil, fmt.Errorf("no layer %s", diffID)
	}
	return &mapFiles{m: m, files: files, names: names}, nil
}

func (f *mapFiles) Get(name string) (io.ReadCloser, error) {
	if f.i >= len(f.names) || f.names[f.i] != name {
		return nil, fmt.Errorf("%w: %q", ErrMismatch, name)
	}
	f.i++
	b := f.files[path.Clean("/" + name)[1:]]
	if name == f.m.lie {
		b = append([]byte("x"), b[1:]...)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *mapFiles) Close() error { return nil }

// saveImage is what save gives WriteArchive for a record.
func saveImage(t *testing.T, s *Store, refs ...string) SaveImage {
	t.Helper()
	img, err := s.Get(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.RawConfig(img)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	si := SaveImage{Config: raw, ConfigDigest: img.ID()}
	for _, d := range cf.RootFS.DiffIDs {
		si.DiffIDs = append(si.DiffIDs, d.String())
	}
	for _, r := range refs {
		n, _ := normalize(r)
		si.Refs = append(si.Refs, n.Name())
	}
	return si
}

func TestWriteArchive(t *testing.T) {
	// Two images sharing a layer, the second with an empty layer too.
	one := testImage(t, "amd64")
	base, _ := one.Layers()
	two, err := mutate.AppendLayers(empty.Image, base[0], tarLayer(t), tarLayer(t, entry{name: "two", body: "2"}))
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := two.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	two, _ = mutate.ConfigFile(two, cf)

	s := Open(t.TempDir())
	for ref, img := range map[string]v1.Image{"one:1": one, "two:1": two} {
		a, _ := OpenArchive(dockerArchive(t, img, []string{ref}, gzipped, true))
		if _, err := s.Load(a); err != nil {
			t.Fatal(err)
		}
	}
	src := newMapSource(t, one, two)
	imgs := []SaveImage{saveImage(t, s, "one:1"), saveImage(t, s, "two:1")}
	if got := SaveLayers(imgs); len(got) != 3 {
		t.Errorf("SaveLayers = %v: want the shared layer once, and no empty one", got)
	}
	out := filepath.Join(t.TempDir(), "saved.tar")
	f, _ := os.Create(out)
	if err := WriteArchive(f, imgs, s.Layers, src); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// go-containerregistry reads it with the same IDs and layers.
	for ref, img := range map[string]v1.Image{"one:1": one, "two:1": two} {
		want, _ := img.ConfigName()
		tag, _ := normalize(ref)
		got, err := tarball.ImageFromPath(out, tagOf(t, tag.Name()))
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if id, _ := got.ConfigName(); id != want {
			t.Errorf("%s: ID %s, want %s", ref, id, want)
		}
		wantLayers, _ := img.Layers()
		gotLayers, _ := got.Layers()
		for i := range wantLayers {
			wd, _ := wantLayers[i].DiffID()
			rc, _ := gotLayers[i].Uncompressed()
			h := sha256.New()
			io.Copy(h, rc)
			rc.Close()
			if "sha256:"+hex.EncodeToString(h.Sum(nil)) != wd.String() {
				t.Errorf("%s layer %d does not hash to its diffID", ref, i)
			}
		}
	}
	// So does its OCI layout, unpacked.
	dir := t.TempDir()
	untar(t, out, dir)
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 2 || im.Manifests[0].Annotations["io.containerd.image.name"] != "docker.io/library/one:1" ||
		im.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "1" {
		t.Errorf("index.json: %+v", im.Manifests)
	}
	for _, d := range im.Manifests {
		img, err := idx.Image(d.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateLayers(img); err != nil {
			t.Error(err)
		}
	}
	// And runwhp loads it back, into another store, with the same IDs.
	s2 := Open(t.TempDir())
	rf, _ := os.Open(out)
	defer rf.Close()
	a, err := OpenArchive(rf)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s2.Load(a)
	if err != nil || len(loaded) != 2 || loaded[0].ID != imgs[0].ConfigDigest || loaded[1].ID != imgs[1].ConfigDigest {
		t.Errorf("Load = %+v, %v", loaded, err)
	}
	var manifest []map[string]any
	json.Unmarshal(readEntry(t, out, "manifest.json"), &manifest)
	if len(manifest) != 2 || fmt.Sprint(manifest[0]["RepoTags"]) != "[one:1]" {
		t.Errorf("manifest.json: %v", manifest)
	}
	if string(readEntry(t, out, "oci-layout")) != `{"imageLayoutVersion": "1.0.0"}` || readEntry(t, out, "repositories") == nil {
		t.Error("oci-layout or repositories")
	}
}

func TestWriteArchiveUntagged(t *testing.T) {
	img := testImage(t, "amd64")
	s := Open(t.TempDir())
	a, _ := OpenArchive(dockerArchive(t, img, []string{"one:1"}, nil, true))
	s.Load(a)
	si := saveImage(t, s, "one:1")
	si.Refs = nil
	var buf bytes.Buffer
	if err := WriteArchive(&buf, []SaveImage{si}, s.Layers, newMapSource(t, img)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id.tar")
	os.WriteFile(p, buf.Bytes(), 0o644)
	var manifest []map[string]any
	json.Unmarshal(readEntry(t, p, "manifest.json"), &manifest)
	if manifest[0]["RepoTags"] != nil || readEntry(t, p, "repositories") != nil {
		t.Errorf("an untagged image has tags: %v", manifest)
	}
}

func TestWriteArchiveRefusesWrongLayers(t *testing.T) {
	img := testImage(t, "amd64")
	s := Open(t.TempDir())
	a, _ := OpenArchive(dockerArchive(t, img, []string{"one:1"}, nil, true))
	s.Load(a)
	si := saveImage(t, s, "one:1")
	// A source that serves a file's contents wrong.
	src := newMapSource(t, img)
	src.lie = "app"
	if err := WriteArchive(io.Discard, []SaveImage{si}, s.Layers, src); err == nil {
		t.Error("saved a layer from a changed file")
	}
	// A layer without tar metadata.
	sp, _ := s.Layers.SplitPath(si.DiffIDs[0])
	os.Remove(sp)
	if err := WriteArchive(io.Discard, []SaveImage{si}, s.Layers, newMapSource(t, img)); !errors.Is(err, layer.ErrNoSplit) {
		t.Errorf("a layer without metadata: %v", err)
	}
}

func validateLayers(img v1.Image) error {
	ls, err := img.Layers()
	if err != nil {
		return err
	}
	for _, l := range ls {
		d, _ := l.Digest()
		rc, err := l.Compressed()
		if err != nil {
			return err
		}
		h := sha256.New()
		io.Copy(h, rc)
		rc.Close()
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) != d.String() {
			return fmt.Errorf("blob %s does not hash to its name", d)
		}
	}
	return nil
}

func untar(t *testing.T, archive, dir string) {
	t.Helper()
	f, _ := os.Open(archive)
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return
		}
		p := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if hdr.Typeflag == tar.TypeDir {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		b, _ := io.ReadAll(tr)
		os.WriteFile(p, b, 0o644)
	}
}

func readEntry(t *testing.T, archive, name string) []byte {
	t.Helper()
	f, _ := os.Open(archive)
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil
		}
		if hdr.Name == name {
			b, _ := io.ReadAll(tr)
			return b
		}
	}
}

func tagOf(t *testing.T, ref string) *name.Tag {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	return &tag
}

// TestRebuildLayer rebuilds one layer's tar, as push does, and refuses one
// whose files are not the layer's.
func TestRebuildLayer(t *testing.T) {
	s := Open(t.TempDir())
	l := tarLayer(t, entry{name: "dir/", dir: true}, entry{name: "dir/a", body: "alpha"}, entry{name: "b", body: "bravo"})
	rc, _ := l.Uncompressed()
	want, _ := io.ReadAll(rc)
	d, _, err := s.ImportDiff(bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	src := &mapSource{files: map[string]map[string][]byte{d: {"dir/a": []byte("alpha"), "b": []byte("bravo")}}}
	if size, err := LayerSize(s.Layers, d); err != nil || size != int64(len(want)) {
		t.Errorf("size %d, %v; want %d", size, err, len(want))
	}
	var got bytes.Buffer
	if n, err := RebuildLayer(&got, s.Layers, d, src); err != nil || n != int64(len(want)) || !bytes.Equal(got.Bytes(), want) {
		t.Errorf("rebuilt %d bytes, %v; equal %v", n, err, bytes.Equal(got.Bytes(), want))
	}
	src.lie = "b"
	if _, err := RebuildLayer(io.Discard, s.Layers, d, src); err == nil {
		t.Error("a layer rebuilt from the wrong contents passed")
	}
}
