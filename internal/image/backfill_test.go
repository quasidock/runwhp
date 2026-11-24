package image

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// TestBackfill fetches what a store made before iteration 14 lacks: its
// layers' tar metadata and the configuration blob, and for a record made
// before iteration 12, the configuration's digest.
func TestBackfill(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1)
	ref := host + "/test/old:1"
	pushed := push(t, ref, "amd64", tarLayer(t, entry{name: "a", body: "a"}), tarLayer(t, entry{name: "b", body: "b"}))
	id, _ := pushed.ConfigName()

	s := Open(t.TempDir())
	ctx := context.Background()
	if _, err := s.Pull(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// As iteration 11 left it: no metadata, no blob, no configuration digest.
	recs, _ := s.Find(ref)
	img := *recs[0].Image
	img.ConfigDigest = ""
	s.writeRecordAt(recs[0].Path, &img)
	splits, _ := filepath.Glob(filepath.Join(s.Root, "layers", "sha256", "*.tar-split.gz"))
	for _, p := range splits {
		os.Remove(p)
	}
	os.RemoveAll(filepath.Join(s.Root, "blobs"))
	recs, _ = s.Find(ref)
	if layers, config, err := s.Missing(recs[0].Image); err != nil || len(layers) != 2 || !config {
		t.Fatalf("Missing = %v, %v, %v", layers, config, err)
	}
	var progress strings.Builder
	s.Progress = &progress
	if err := s.Backfill(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "Fetching tar metadata for 2 layers of "+ref) {
		t.Errorf("progress: %q", progress.String())
	}
	recs, _ = s.Find(ref)
	if layers, config, err := s.Missing(recs[0].Image); err != nil || len(layers) != 0 || config {
		t.Errorf("Missing after Backfill = %v, %v, %v", layers, config, err)
	}
	if recs[0].ConfigDigest != id.String() {
		t.Errorf("the record's configuration digest: %q, want %s", recs[0].ConfigDigest, id)
	}
	// Nothing missing: no registry needed.
	srv.Close()
	if err := s.Backfill(ctx, recs); err != nil {
		t.Errorf("Backfill of an image that lacks nothing: %v", err)
	}
	// Missing again, with the registry gone.
	os.RemoveAll(filepath.Join(s.Root, "blobs"))
	if err := s.Backfill(ctx, recs); err == nil {
		t.Error("Backfill with the registry gone")
	}
	// No registry to ask: a record with no digest.
	img = *recs[0].Image
	img.Digest = ""
	if err := s.Backfill(ctx, []*Record{{Image: &img}}); err == nil || !strings.Contains(err.Error(), "pull it again") {
		t.Errorf("Backfill without a registry digest: %v", err)
	}
}

// TestBackfillFromOthers saves an image committed from a pulled one, which
// has no registry of its own: its parent's layers' metadata comes through
// the parent's record.
func TestBackfillFromOthers(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1)
	ref := host + "/test/base:1"
	push(t, ref, "amd64", tarLayer(t, entry{name: "a", body: "a"}))

	s := Open(t.TempDir())
	ctx := context.Background()
	base, err := s.Pull(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	splits, _ := filepath.Glob(filepath.Join(s.Root, "layers", "sha256", "*.tar-split.gz"))
	for _, p := range splits {
		os.Remove(p)
	}
	parent, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	diff, _ := tarLayer(t, entry{name: "added", body: "x"}).Uncompressed()
	img, err := s.Commit(Commit{Parent: parent, Config: parent.Config, Container: "c", Now: time.Now()}, diff, "mine:1")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Find("mine:1")
	if layers, _, _ := s.Missing(recs[0].Image); len(layers) != 1 {
		t.Fatalf("missing %v, want the base layer alone", layers)
	}
	if err := s.Backfill(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if layers, config, err := s.Missing(img); err != nil || len(layers) != 0 || config {
		t.Errorf("Missing after Backfill = %v, %v, %v", layers, config, err)
	}
}
