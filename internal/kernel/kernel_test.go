package kernel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fakeBzImage returns a minimal image that boot.ParseKernel accepts.
func fakeBzImage(fill byte) []byte {
	img := bytes.Repeat([]byte{fill}, 3*512+4096)
	img[0x1f1] = 2 // setup_sects
	binary.LittleEndian.PutUint16(img[0x1fe:], 0xaa55)
	img[0x201] = 0x66
	copy(img[0x202:], "HdrS")
	binary.LittleEndian.PutUint16(img[0x206:], 0x020f)
	binary.LittleEndian.PutUint16(img[0x236:], 1) // XLF_KERNEL_64
	return img
}

func testArtifact() *Artifact {
	return &Artifact{
		BzImage:     fakeBzImage(0x90),
		Config:      []byte("CONFIG_64BIT=y\n"),
		Build:       []byte("build files"),
		Source:      []byte("source tarball"),
		SourceName:  "linux-6.18.64.tar.xz",
		Annotations: map[string]string{"org.opencontainers.image.version": "6.18.64-1"},
	}
}

// testRegistry serves an in-process registry. wrap, if not nil, sits in
// front of it.
func testRegistry(t *testing.T, wrap func(http.Handler) http.Handler) (host string, requests *atomic.Int64) {
	requests = &atomic.Int64{}
	var h http.Handler = registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// go-containerregistry speaks plain HTTP to localhost.
	return strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1), requests
}

func publish(t *testing.T, host string, a *Artifact) Pin {
	t.Helper()
	tag, err := name.NewTag(host + "/runwhp-kernel:6.18.64-1")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Push(tag, a)
	if err != nil {
		t.Fatal(err)
	}
	return Pin{Repository: host + "/runwhp-kernel", Tag: "6.18.64-1", Digest: d.String()}
}

func TestManifest(t *testing.T) {
	a := testArtifact()
	raw, blobs, err := a.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := testArtifact().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Errorf("the same artifact gave different manifests:\n%s\n%s", raw, again)
	}
	var m v1.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ArtifactType != ArtifactType || m.MediaType != types.OCIManifestSchema1 {
		t.Errorf("artifactType %q, mediaType %q", m.ArtifactType, m.MediaType)
	}
	// The OCI empty descriptor has a well-known digest.
	if m.Config.MediaType != types.OCIEmptyJSON || m.Config.Digest.String() != "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Errorf("config descriptor %+v", m.Config)
	}
	want := []types.MediaType{MediaBzImage, MediaConfig, MediaBuild, MediaSource}
	if len(m.Layers) != len(want) || len(blobs) != len(want)+1 {
		t.Fatalf("%d layers and %d blobs", len(m.Layers), len(blobs))
	}
	for i, l := range m.Layers {
		if l.MediaType != want[i] || l.Annotations[titleAnnotation] == "" {
			t.Errorf("layer %d: %+v", i, l)
		}
	}
	if m.Layers[3].Annotations[titleAnnotation] != "linux-6.18.64.tar.xz" {
		t.Errorf("source layer title %q", m.Layers[3].Annotations[titleAnnotation])
	}
	if d, err := a.Digest(); err != nil || d.String() == "" {
		t.Errorf("Digest: %v, %v", d, err)
	}

	a.Source = nil
	if _, _, err := a.Manifest(); err == nil {
		t.Error("accepted an artifact without its source")
	}
}

func TestEnsure(t *testing.T) {
	host, requests := testRegistry(t, nil)
	a := testArtifact()
	pin := publish(t, host, a)
	if d, _ := a.Digest(); d.String() != pin.Digest {
		t.Errorf("Digest %s, pushed as %s", d, pin.Digest)
	}

	s := Open(t.TempDir())
	var progress bytes.Buffer
	s.Progress = &progress
	if _, err := s.Ensure(context.Background(), pin, false); !errors.Is(err, ErrNotPresent) {
		t.Errorf("Ensure without fetching on an empty store: %v", err)
	}
	before := requests.Load()
	path, err := s.Ensure(context.Background(), pin, true)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := s.Path(pin); path != want {
		t.Errorf("path %s, want %s", path, want)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, a.BzImage) {
		t.Error("stored bzImage differs from the published one")
	}
	if requests.Load() == before {
		t.Error("nothing was fetched")
	}
	if !strings.HasPrefix(progress.String(), "pulling kernel 6.18.64-1") {
		t.Errorf("progress: %q", progress.String())
	}

	before = requests.Load()
	for _, fetch := range []bool{false, true} {
		if again, err := s.Ensure(context.Background(), pin, fetch); err != nil || again != path {
			t.Errorf("second Ensure(%v): %s, %v", fetch, again, err)
		}
	}
	if n := requests.Load() - before; n != 0 {
		t.Errorf("a stored kernel took %d more requests", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "tmp")); len(entries) != 0 {
		t.Errorf("left %d files in tmp", len(entries))
	}
}

func TestEnsureConcurrent(t *testing.T) {
	host, _ := testRegistry(t, nil)
	pin := publish(t, host, testArtifact())
	s := Open(t.TempDir())
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Ensure(context.Background(), pin, true)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Ensure %d: %v", i, err)
		}
	}
}

func TestEnsureRejects(t *testing.T) {
	host, _ := testRegistry(t, nil)

	put := func(t *testing.T, tag string, raw []byte, blobs []v1.Layer) Pin {
		t.Helper()
		ref, err := name.NewTag(host + "/runwhp-kernel:" + tag)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blobs {
			if err := remote.WriteLayer(ref.Context(), b); err != nil {
				t.Fatal(err)
			}
		}
		if err := remote.Put(ref, rawManifest(raw)); err != nil {
			t.Fatal(err)
		}
		d, _, _ := v1.SHA256(bytes.NewReader(raw))
		return Pin{Repository: host + "/runwhp-kernel", Tag: tag, Digest: d.String()}
	}
	edit := func(a *Artifact, change func(*v1.Manifest)) ([]byte, []v1.Layer) {
		raw, blobs, err := a.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		var m v1.Manifest
		json.Unmarshal(raw, &m)
		change(&m)
		raw, _ = json.Marshal(m)
		return raw, blobs
	}

	type rejection struct {
		name, reason string
		raw          []byte
		blobs        []v1.Layer
	}
	var cases []rejection
	raw, blobs := edit(testArtifact(), func(m *v1.Manifest) { m.ArtifactType = "application/vnd.example" })
	cases = append(cases, rejection{"wrong-type", "not a runwhp kernel", raw, blobs})
	raw, blobs = edit(testArtifact(), func(m *v1.Manifest) { m.Layers = m.Layers[1:] })
	cases = append(cases, rejection{"no-bzimage", "0 bzImage layers", raw, blobs})
	raw, blobs = edit(testArtifact(), func(m *v1.Manifest) { m.Layers[0].Size = maxBzImage + 1 })
	cases = append(cases, rejection{"too-big", "unacceptable bzImage", raw, blobs})
	notKernel := testArtifact()
	notKernel.BzImage = []byte("not a kernel")
	raw, blobs, _ = notKernel.Manifest()
	cases = append(cases, rejection{"not-a-kernel", "bzImage: not a bzImage", raw, blobs})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin := put(t, c.name, c.raw, c.blobs)
			s := Open(t.TempDir())
			_, err := s.Ensure(context.Background(), pin, true)
			if err == nil || !strings.Contains(err.Error(), c.reason) {
				t.Errorf("got %v, want an error containing %q", err, c.reason)
			}
			if p, _ := s.Path(pin); fileExists(p) {
				t.Error("stored a rejected kernel")
			}
		})
	}

	t.Run("wrong-digest", func(t *testing.T) {
		pin := publish(t, host, testArtifact())
		other, _ := (&Artifact{BzImage: fakeBzImage(1), Config: []byte("x"), Build: []byte("x"), Source: []byte("x"), SourceName: "x"}).Digest()
		pin.Digest = other.String()
		if _, err := Open(t.TempDir()).Ensure(context.Background(), pin, true); err == nil {
			t.Error("fetched a manifest the registry does not have")
		}
	})
	t.Run("no-digest", func(t *testing.T) {
		if _, err := Open(t.TempDir()).Ensure(context.Background(), Pin{Repository: host + "/x", Tag: "1"}, true); err == nil {
			t.Error("accepted a pin without a digest")
		}
	})
}

// TestEnsureTampered serves a bzImage whose bytes do not match its digest.
func TestEnsureTampered(t *testing.T) {
	a := testArtifact()
	bz, _, _ := v1.SHA256(bytes.NewReader(a.BzImage))
	host, _ := testRegistry(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blobs/"+bz.String()) {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			body[len(body)-1] ^= 0xff
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(body)
		})
	})
	pin := publish(t, host, a)
	s := Open(t.TempDir())
	if _, err := s.Ensure(context.Background(), pin, true); err == nil {
		t.Fatal("accepted a tampered bzImage")
	}
	if p, _ := s.Path(pin); fileExists(p) {
		t.Error("stored a tampered bzImage")
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "tmp")); len(entries) != 0 {
		t.Errorf("left %d files in tmp", len(entries))
	}
}

func TestDefaultPin(t *testing.T) {
	if _, err := name.NewRepository(Default.Repository); err != nil {
		t.Error(err)
	}
	if Default.Tag == "" {
		t.Error("Default has no tag")
	}
	if Default.Digest != "" {
		if _, err := Open(t.TempDir()).Path(Default); err != nil {
			t.Error(err)
		}
		if _, err := Default.Reference(); err != nil {
			t.Error(err)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
