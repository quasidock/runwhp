//go:build windows

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/go-containerregistry/pkg/v1/validate"
)

// countingRegistry is a registry of the test's own that counts blob
// uploads.
func countingRegistry(t *testing.T) (host string, uploads *atomic.Int32) {
	t.Helper()
	uploads = &atomic.Int32{}
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/blobs/uploads") {
			uploads.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// localhost, so runwhp speaks plain HTTP to it.
	return strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1), uploads
}

// TestPush pushes a built image, pulls it back into another store with
// its ID, and pushes it again, uploading nothing.
func TestPush(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	host, uploads := countingRegistry(t)
	dir := buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nRUN echo pushed > /p\nCMD [\"cat\", \"/p\"]\n"})
	ref := host + "/built/img:1"
	s.ok("build", "-t", ref, dir)
	id := s.imageID(ref)

	r := s.ok("push", ref)
	if !strings.Contains(r.stdout, "The push refers to repository ["+host+"/built/img]") ||
		strings.Count(r.stdout, ": Pushed") != 2 || !strings.Contains(r.stdout, "1: digest: sha256:") {
		t.Errorf("push:\n%s", r.stdout)
	}
	tag, _ := name.NewTag(ref)
	img, err := remote.Image(tag)
	if err != nil {
		t.Fatal(err)
	}
	if mt, _ := img.MediaType(); mt != types.DockerManifestSchema2 {
		t.Errorf("manifest media type %s", mt)
	}
	if cfg, _ := img.ConfigName(); cfg.String() != id {
		t.Errorf("pushed with ID %s, want %s", cfg, id)
	}
	if err := validate.Image(img); err != nil {
		t.Errorf("validate: %v", err)
	}
	digest, _ := img.Digest()
	if out := s.ok("image", "inspect", ref).stdout; !strings.Contains(out, host+"/built/img@"+digest.String()) {
		t.Errorf("inspect lacks the pushed digest:\n%s", out)
	}

	// Pulled into another store, with its ID, and run.
	other := newOwnStore(t)
	other.ok("pull", ref)
	if other.imageID(ref) != id {
		t.Errorf("pulled back as %s, want %s", other.imageID(ref), id)
	}
	if got := other.ok("run", "--rm", ref).stdout; got != "pushed\n" {
		t.Errorf("run of the pulled image: %q", got)
	}

	// Again: nothing uploaded.
	uploads.Store(0)
	r = s.ok("push", ref)
	if strings.Count(r.stdout, ": Layer already exists") != 2 || uploads.Load() != 0 {
		t.Errorf("a second push, %d uploads:\n%s", uploads.Load(), r.stdout)
	}

	// A pulled image pushed elsewhere, with all its tags.
	s.ok("tag", base, host+"/copied/alpine:a")
	s.ok("tag", base, host+"/copied/alpine:b")
	r = s.ok("push", "--all-tags", host+"/copied/alpine")
	if !strings.Contains(r.stdout, "a: digest:") || !strings.Contains(r.stdout, "b: digest:") {
		t.Errorf("push --all-tags:\n%s", r.stdout)
	}
	if got := other.ok("run", "--rm", host+"/copied/alpine:b", "cat", "/etc/alpine-release").stdout; !strings.HasPrefix(got, "3.20") {
		t.Errorf("the pushed alpine: %q", got)
	}
	if q := strings.TrimSpace(s.ok("push", "-q", host+"/copied/alpine:a").stdout); q != host+"/copied/alpine:a" {
		t.Errorf("push -q printed %q", q)
	}

	// What cannot be pushed.
	for _, c := range []struct{ args, want string }{
		{host + "/nothing:1", "An image does not exist locally with the tag: " + host + "/nothing:1"},
		{id, "An image does not exist locally with the tag"},
		{"localhost:1/unreachable/img:1", "An image does not exist locally"},
	} {
		if r := s.cli("", "push", c.args); r.code != 1 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("push %s: %d %s", c.args, r.code, r.stderr)
		}
	}
	s.ok("tag", ref, "localhost:1/unreachable/img:1")
	if r := s.cli("", "push", "localhost:1/unreachable/img:1"); r.code != 1 || r.stderr == "" {
		t.Errorf("push to an unreachable registry: %d %s", r.code, fmt.Sprint(r.stderr))
	}
}
