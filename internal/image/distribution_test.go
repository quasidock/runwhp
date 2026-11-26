package image

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestDistribution looks up an image and an index in a registry, as
// docker's distribution inspect does, without pulling either.
func TestDistribution(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1)

	one := push(t, host+"/test/one:1", "amd64", tarLayer(t, entry{name: "a", body: "a"}))
	arm := push(t, host+"/test/arm:1", "arm64", tarLayer(t, entry{name: "b", body: "b"}))
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: one, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}})
	ref, err := name.ParseReference(host + "/test/multi:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}

	s := Open(t.TempDir())
	ctx := context.Background()
	desc, platforms, err := s.Distribution(ctx, host+"/test/one:1")
	wantDigest, _ := one.Digest()
	if err != nil || desc.Digest != wantDigest || desc.Size == 0 || len(platforms) != 1 ||
		platforms[0].OS != "linux" || platforms[0].Architecture != "amd64" {
		t.Errorf("image: %+v, %+v, %v", desc, platforms, err)
	}
	desc, platforms, err = s.Distribution(ctx, host+"/test/multi:1")
	wantDigest, _ = idx.Digest()
	if err != nil || desc.Digest != wantDigest || !desc.MediaType.IsIndex() || len(platforms) != 2 ||
		platforms[1].Architecture != "arm64" || platforms[1].Variant != "v8" {
		t.Errorf("index: %+v, %+v, %v", desc, platforms, err)
	}
	if _, _, err := s.Distribution(ctx, host+"/test/missing:1"); err == nil {
		t.Error("a missing image was found")
	}
	if images, _ := s.List(); len(images) != 0 {
		t.Errorf("looked-up images were pulled: %v", images)
	}
}
