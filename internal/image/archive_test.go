package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
)

// testImage is an image of two layers for arch, built in memory.
func testImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image,
		tarLayer(t, entry{name: "etc/", dir: true}, entry{name: "etc/os-release", body: "NAME=archive\n"}),
		tarLayer(t, entry{name: "app", body: strings.Repeat("a", 100000)}))
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	cf.Config.Cmd = []string{"/app"}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// archiveFile writes files into a tar, in order, and opens it.
func archiveFile(t *testing.T, files [][2]string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive.tar")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, nf := range files {
		tw.WriteHeader(&tar.Header{Name: nf[0], Mode: 0o644, Size: int64(len(nf[1]))})
		io.WriteString(tw, nf[1])
	}
	tw.Close()
	f.Close()
	f, err = os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// dockerArchive lays img out as Docker 29's save does: an OCI layout, with
// layers as plain tars named by diffID, and manifest.json. compress, if
// not nil, compresses the layers. withManifest leaves out manifest.json.
func dockerArchive(t *testing.T, img v1.Image, tags []string, compress func([]byte) []byte, withManifest bool) *os.File {
	t.Helper()
	raw, _ := img.RawConfigFile()
	cfgDigest := digestOf(raw)
	files := [][2]string{{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`}}
	blob := func(data []byte) string {
		d := digestOf(data)
		files = append(files, [2]string{"blobs/sha256/" + strings.TrimPrefix(d, "sha256:"), string(data)})
		return d
	}
	blob(raw)
	ls, _ := img.Layers()
	var layerPaths []string
	var descs []map[string]any
	for _, l := range ls {
		rc, _ := l.Uncompressed()
		data, _ := io.ReadAll(rc)
		if compress != nil {
			data = compress(data)
		}
		d := blob(data)
		layerPaths = append(layerPaths, "blobs/sha256/"+strings.TrimPrefix(d, "sha256:"))
		descs = append(descs, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": d, "size": len(data)})
	}
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": len(raw)},
		"layers": descs})
	md := blob(manifest)
	var entries []map[string]any
	for _, tag := range tags {
		entries = append(entries, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md,
			"annotations": map[string]string{"io.containerd.image.name": tag, "org.opencontainers.image.ref.name": "x"}})
	}
	if len(tags) == 0 {
		entries = append(entries, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md})
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": entries})
	files = append(files, [2]string{"index.json", string(index)})
	if withManifest {
		m, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + strings.TrimPrefix(cfgDigest, "sha256:"), "RepoTags": tags, "Layers": layerPaths}})
		files = append(files, [2]string{"manifest.json", string(m)})
	}
	return archiveFile(t, files)
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func zstded(b []byte) []byte {
	zw, _ := zstd.NewWriter(nil)
	return zw.EncodeAll(b, nil)
}

// checkLoaded checks that s holds img under each of refs, with its ID, and
// every layer with its tar metadata.
func checkLoaded(t *testing.T, s *Store, img v1.Image, refs ...string) {
	t.Helper()
	id, _ := img.ConfigName()
	for _, ref := range refs {
		got, err := s.Get(ref)
		if err != nil {
			t.Fatalf("Get(%s): %v", ref, err)
		}
		if got.ID() != id.String() || got.Digest != "" {
			t.Errorf("%s: ID %s, digest %q; want ID %s and no digest", ref, got.ID(), got.Digest, id)
		}
		raw, _ := img.RawConfigFile()
		if blob, err := s.RawConfig(got); err != nil || !bytes.Equal(blob, raw) {
			t.Errorf("%s: configuration blob: %v", ref, err)
		}
		layers, _ := got.Layers()
		for _, d := range layers {
			if !s.Layers.HasSplit(d) {
				t.Errorf("%s: layer %s has no tar metadata", ref, d)
			}
		}
	}
}

func TestLoadGGCRArchive(t *testing.T) {
	img := testImage(t, "amd64")
	p := filepath.Join(t.TempDir(), "ggcr.tar")
	tags := map[name.Reference]v1.Image{}
	for _, tag := range []string{"localhost:5000/app:1", "app:latest"} {
		r, _ := name.ParseReference(tag)
		tags[r] = img
	}
	if err := tarball.MultiRefWriteToFile(p, tags); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	a, err := OpenArchive(f)
	if err != nil {
		t.Fatal(err)
	}
	s := Open(t.TempDir())
	loaded, err := s.Load(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || len(loaded[0].Refs) != 2 {
		t.Fatalf("Load = %+v", loaded)
	}
	checkLoaded(t, s, img, "localhost:5000/app:1", "app:latest")
}

func TestLoadDockerArchive(t *testing.T) {
	img := testImage(t, "amd64")
	for _, c := range []struct {
		name         string
		compress     func([]byte) []byte
		withManifest bool
	}{
		{"docker29", nil, true},
		{"oci-gzip", gzipped, false},
		{"oci-zstd", zstded, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := dockerArchive(t, img, []string{"docker.io/library/app:1", "app:2"}, c.compress, c.withManifest)
			a, err := OpenArchive(f)
			if err != nil {
				t.Fatal(err)
			}
			s := Open(t.TempDir())
			loaded, err := s.Load(a)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 || strings.Join(loaded[0].Refs, " ") != "index.docker.io/library/app:1 index.docker.io/library/app:2" {
				t.Fatalf("Load = %+v", loaded)
			}
			checkLoaded(t, s, img, "app:1", "app:2")
			// Loading again reads no layer again.
			s.Progress = &bytes.Buffer{}
			if _, err := s.Load(a); err != nil || strings.Contains(s.Progress.(*bytes.Buffer).String(), "loading") {
				t.Errorf("second Load: %v; %q", err, s.Progress)
			}
		})
	}
}

func TestLoadUntagged(t *testing.T) {
	img := testImage(t, "amd64")
	id, _ := img.ConfigName()
	s := Open(t.TempDir())
	a, _ := OpenArchive(dockerArchive(t, img, nil, nil, true))
	loaded, err := s.Load(a)
	if err != nil || len(loaded) != 1 || loaded[0].ID != id.String() || len(loaded[0].Refs) != 0 {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
	checkLoaded(t, s, img, id.String())
	recs, _ := s.List()
	if len(recs) != 1 || !Untagged(recs[0].Ref) {
		t.Fatalf("records: %v", recs)
	}
	// Loaded again, and once tagged, it gets no second untagged record.
	s.Load(a)
	s.Put("app:1", &Image{Ref: "index.docker.io/library/app:1", ConfigDigest: id.String(), Config: recs[0].Config})
	os.Remove(recs[0].Path)
	s.Load(a)
	if recs, _ := s.List(); len(recs) != 1 || recs[0].Ref != "index.docker.io/library/app:1" {
		t.Errorf("records after loading a tagged image's ID again: %v", recs)
	}
}

func TestLoadRefused(t *testing.T) {
	s := Open(t.TempDir())
	a, _ := OpenArchive(dockerArchive(t, testImage(t, "arm64"), []string{"app:1"}, nil, true))
	if _, err := s.Load(a); err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("Load of an arm64 image: %v", err)
	}
	// A configuration whose name is not its digest.
	img := testImage(t, "amd64")
	raw, _ := img.RawConfigFile()
	bad := archiveFile(t, [][2]string{
		{"blobs/sha256/" + strings.Repeat("0", 64), string(raw)},
		{"manifest.json", `[{"Config":"blobs/sha256/` + strings.Repeat("0", 64) + `","RepoTags":["app:1"],"Layers":[]}]`},
	})
	a, _ = OpenArchive(bad)
	if _, err := a.Images(); err == nil {
		t.Error("read a configuration whose digest is not its name")
	}
	// A layer whose content is not its diffID.
	f := dockerArchive(t, img, []string{"app:1"}, nil, true)
	a, _ = OpenArchive(f)
	imgs, _ := a.Images()
	imgs[0].Layers[0].DiffID = imgs[0].Layers[1].DiffID
	if err := s.loadLayer(imgs[0].Layers[0]); err == nil {
		t.Error("stored a layer under another's diffID")
	}
	a, _ = OpenArchive(archiveFile(t, [][2]string{{"hello", "world"}}))
	if _, err := a.Images(); err == nil {
		t.Error("an archive with no index")
	}
}

func TestImport(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "bin/hello", Mode: 0o755, Size: 5})
	tw.Write([]byte("hello"))
	tw.Close()
	plain := buf.Bytes()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		data []byte
	}{{"plain", plain}, {"gzip", gzipped(plain)}, {"zstd", zstded(plain)}} {
		t.Run(c.name, func(t *testing.T) {
			s := Open(t.TempDir())
			r, err := Decompress(bytes.NewReader(c.data))
			if err != nil {
				t.Fatal(err)
			}
			img, err := s.Import(r, "imported:1", []string{`CMD ["/bin/hello"]`, "ENV A=b"}, "", now)
			if err != nil {
				t.Fatal(err)
			}
			cf, _ := img.ConfigFile()
			if cf.Architecture != "amd64" || cf.OS != "linux" || !cf.Created.Time.Equal(now) ||
				strings.Join(cf.Config.Cmd, " ") != "/bin/hello" || strings.Join(cf.Config.Env, " ") != "A=b" ||
				len(cf.History) != 1 || cf.History[0].Comment != "Imported from -" || len(cf.RootFS.DiffIDs) != 1 ||
				cf.RootFS.DiffIDs[0].String() != digestOf(plain) {
				t.Errorf("configuration: %s", img.Config)
			}
			got, err := s.Get("imported:1")
			if err != nil || got.ID() != digestOf(img.Config) {
				t.Errorf("Get = %v, %v", got, err)
			}
			if blob, err := s.RawConfig(got); err != nil || !bytes.Equal(blob, img.Config) {
				t.Errorf("configuration blob: %v", err)
			}
		})
	}
	s := Open(t.TempDir())
	img, err := s.Import(bytes.NewReader(plain), "", nil, "my message", now)
	if err != nil || !Untagged(img.Ref) || img.Ref != img.ID() {
		t.Fatalf("untagged import: %+v, %v", img, err)
	}
	if cf, _ := img.ConfigFile(); cf.History[0].Comment != "my message" {
		t.Errorf("message: %s", img.Config)
	}
	if _, err := s.Import(bytes.NewReader(plain), "", []string{"RUN true"}, "", now); err == nil {
		t.Error("imported with RUN")
	}
	if _, err := Decompress(bytes.NewReader([]byte{0xfd, '7', 'z', 'X', 'Z', 0, 1, 2})); err == nil || !strings.Contains(err.Error(), "xz") {
		t.Errorf("xz: %v", err)
	}
	if _, err := s.Import(strings.NewReader("not a tar, not at all, and long enough to tell............................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................"), "x:1", nil, "", now); err == nil {
		t.Error("imported junk")
	}
	_ = errors.New
}
