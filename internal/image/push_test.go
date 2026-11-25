package image

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/go-containerregistry/pkg/v1/validate"

	"github.com/quasidock/runwhp/internal/layer"
)

// TestPush pushes an image as docker push does: v2 schema 2, gzip layers,
// the configuration as it is; and again, uploading nothing.
func TestPush(t *testing.T) {
	var uploads atomic.Int32
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/blobs/uploads") {
			uploads.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	s := Open(t.TempDir())
	l1 := tarLayer(t, entry{name: "dir/", dir: true}, entry{name: "dir/a", body: "alpha"})
	l2 := tarLayer(t, entry{name: "b", body: strings.Repeat("bravo", 1000)})
	var diffIDs []string
	src := &mapSource{files: map[string]map[string][]byte{}}
	for _, l := range []v1.Layer{l1, l2} {
		rc, _ := l.Uncompressed()
		data, _ := io.ReadAll(rc)
		d, _, err := s.ImportDiff(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		diffIDs = append(diffIDs, d)
	}
	src.files[diffIDs[0]] = map[string][]byte{"dir/a": []byte("alpha")}
	src.files[diffIDs[1]] = map[string][]byte{"b": []byte(strings.Repeat("bravo", 1000))}
	cfg := map[string]any{
		"architecture": "amd64", "os": "linux", "config": map[string]any{"Cmd": []string{"true"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": append(diffIDs, layer.EmptyDiffID)},
	}
	raw, _ := json.Marshal(cfg)
	id, err := s.PutConfig(raw)
	if err != nil {
		t.Fatal(err)
	}

	tag, _ := name.NewTag(host + "/pushed/img:1")
	tmp := t.TempDir()
	var mu sync.Mutex
	statuses := map[string]string{}
	status := func(d, st string) {
		mu.Lock()
		statuses[d] = st
		mu.Unlock()
	}
	first, err := s.Push(context.Background(), tag, raw, src, tmp, status)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 3 || statuses[diffIDs[0]] != "Pushed" || uploads.Load() != 4 {
		t.Errorf("statuses %v, %d uploads", statuses, uploads.Load())
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("spools left: %v", left)
	}

	img, err := remote.Image(tag)
	if err != nil {
		t.Fatal(err)
	}
	if mt, _ := img.MediaType(); mt != types.DockerManifestSchema2 {
		t.Errorf("manifest media type %s", mt)
	}
	if got, _ := img.ConfigName(); got.String() != id {
		t.Errorf("pushed with ID %s, want %s", got, id)
	}
	if err := validate.Image(img); err != nil {
		t.Errorf("validate: %v", err)
	}
	ls, _ := img.Layers()
	for i, l := range ls {
		if mt, _ := l.MediaType(); mt != types.DockerLayer {
			t.Errorf("layer %d: %s", i, mt)
		}
	}
	m, _ := img.RawManifest()
	if d, _ := img.Digest(); d.String() != first.Digest || len(m) != first.Size || !bytes.Contains(m, []byte("\n   \"schemaVersion\"")) {
		t.Errorf("manifest %s, %d bytes:\n%s", d, len(m), m)
	}

	// Again: the same manifest, and nothing uploaded.
	uploads.Store(0)
	second, err := s.Push(context.Background(), tag, raw, src, tmp, status)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest != first.Digest || uploads.Load() != 0 || statuses[diffIDs[1]] != "Layer already exists" {
		t.Errorf("second push: %s, %d uploads, statuses %v", second.Digest, uploads.Load(), statuses)
	}

	// A layer rebuilt wrong fails the push, which puts no manifest.
	src.lie = "b"
	other, _ := name.NewTag(host + "/pushed/other:1")
	if _, err := s.Push(context.Background(), other, raw, src, tmp, nil); err == nil {
		t.Error("a layer rebuilt from the wrong contents was pushed")
	}
	if _, err := remote.Head(other); err == nil {
		t.Error("a failed push left a manifest")
	}
}
