package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type entry struct {
	name, body string
	dir        bool
}

func tarLayer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.dir {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// testRegistry serves an in-process registry and counts requests.
func testRegistry(t *testing.T) (host string, requests *atomic.Int64) {
	requests = &atomic.Int64{}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// go-containerregistry speaks plain HTTP to localhost.
	return strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1), requests
}

func push(t *testing.T, ref string, arch string, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	cf.Config.Entrypoint = []string{"/bin/sh"}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPullAndCache(t *testing.T) {
	host, requests := testRegistry(t)
	ref := host + "/test/app:1"
	base := tarLayer(t, entry{name: "etc/", dir: true}, entry{name: "etc/os-release", body: "NAME=test\n"})
	top := tarLayer(t, entry{name: "etc/.wh.os-release"}, entry{name: "data/", dir: true}, entry{name: "data/.wh..wh..opq"})
	pushed := push(t, ref, "amd64", base, top, tarLayer(t))

	s := Open(t.TempDir())
	var progress bytes.Buffer
	s.Progress = &progress
	ctx := context.Background()

	if _, err := s.Ensure(ctx, ref, PullNever); err == nil {
		t.Fatal("--pull=never succeeded with an empty store")
	}
	img, err := s.Ensure(ctx, ref, PullMissing)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := pushed.Digest(); img.Digest != d.String() || img.Ref != ref {
		t.Errorf("record %+v, want digest %s", img, d)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("layers = %v; the empty third layer should be skipped", layers)
	}
	for _, d := range layers {
		p, _ := s.Layers.Path(d)
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tar2ext4.ReadExt4SuperBlockReadSeeker(f); err != nil {
			t.Errorf("layer %s is not ext4: %v", d, err)
		}
		f.Close()
	}
	// The pull kept what save needs: each layer's tar metadata, and the
	// configuration blob as the registry served it.
	for _, d := range layers {
		if !s.Layers.HasSplit(d) {
			t.Errorf("layer %s has no tar metadata", d)
		}
	}
	raw, _ := pushed.RawConfigFile()
	if blob, err := s.RawConfig(img); err != nil || !bytes.Equal(blob, raw) {
		t.Errorf("configuration blob: %v; equal %v", err, bytes.Equal(blob, raw))
	}
	if !strings.Contains(progress.String(), "converting to ext4") {
		t.Errorf("no progress reported: %q", progress.String())
	}

	// A second Ensure is served from the store without the registry.
	before := requests.Load()
	if _, err := s.Ensure(ctx, ref, PullMissing); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Error("--pull=missing contacted the registry for a cached image")
	}
	if _, err := s.Ensure(ctx, ref, PullNever); err != nil {
		t.Error(err)
	}
	if _, err := s.Ensure(ctx, ref, PullAlways); err != nil || requests.Load() == before {
		t.Errorf("--pull=always did not contact the registry (err %v)", err)
	}

	// Losing a layer file makes the image absent again.
	p, _ := s.Layers.Path(layers[0])
	os.Remove(p)
	if _, err := s.Get(ref); !errors.Is(err, ErrNotPresent) {
		t.Errorf("Get with a missing layer: %v", err)
	}
}

func TestPullRejectsOtherArchitectures(t *testing.T) {
	host, _ := testRegistry(t)
	ref := host + "/test/arm:1"
	push(t, ref, "arm64", tarLayer(t, entry{name: "a", body: "x"}))
	if _, err := Open(t.TempDir()).Pull(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("pull of an arm64 image: %v", err)
	}
}

func TestLayerDigestMismatch(t *testing.T) {
	s := Open(t.TempDir())
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	wrong := "sha256:" + strings.Repeat("ab", 32)
	if _, err := s.Layers.Put(wrong, &buf); err == nil {
		t.Fatal("stored a layer whose digest does not match")
	}
	if s.Layers.Has(wrong) {
		t.Error("mismatched layer left in the store")
	}
	if _, err := s.Layers.Put("md5:abc", &buf); err == nil {
		t.Error("accepted a non-sha256 digest")
	}
}

func TestBuildProcess(t *testing.T) {
	cfg := v1.Config{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		Env:        []string{"PATH=/bin", "A=1"},
		WorkingDir: "/app",
		User:       "app",
	}
	lookup := func(k string) (string, bool) {
		if k == "HOSTVAR" {
			return "fromhost", true
		}
		return "", false
	}
	for _, tc := range []struct {
		name string
		o    Overrides
		want Process
	}{
		{"image defaults", Overrides{},
			Process{[]string{"/entry", "default"}, cfg.Env, "/app", "app", []string{"/entry"}, []string{"default"}}},
		{"command replaces cmd", Overrides{Cmd: []string{"x", "y"}},
			Process{[]string{"/entry", "x", "y"}, cfg.Env, "/app", "app", []string{"/entry"}, []string{"x", "y"}}},
		{"entrypoint drops cmd", Overrides{Entrypoint: "/other", EntrypointSet: true},
			Process{[]string{"/other"}, cfg.Env, "/app", "app", []string{"/other"}, nil}},
		{"empty entrypoint", Overrides{EntrypointSet: true, Cmd: []string{"sh"}},
			Process{[]string{"sh"}, cfg.Env, "/app", "app", nil, []string{"sh"}}},
		{"env, workdir, user", Overrides{Env: []string{"A=2", "B=3", "HOSTVAR", "UNSET"}, Workdir: "/tmp", User: "0:0", LookupEnv: lookup},
			Process{[]string{"/entry", "default"}, []string{"PATH=/bin", "A=2", "B=3", "HOSTVAR=fromhost"}, "/tmp", "0:0", []string{"/entry"}, []string{"default"}}},
	} {
		got, err := BuildProcess(cfg, tc.o)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
	if _, err := BuildProcess(v1.Config{}, Overrides{}); !errors.Is(err, ErrNoCommand) {
		t.Errorf("no command: %v", err)
	}
	if p, _ := BuildProcess(v1.Config{Cmd: []string{"sh"}}, Overrides{}); p.Cwd != "/" {
		t.Errorf("default cwd = %q", p.Cwd)
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "get" {
		server, _ := io.ReadAll(os.Stdin)
		switch strings.TrimSpace(string(server)) {
		case authn.DefaultAuthKey:
			io.WriteString(os.Stdout, `{"Username":"hubuser","Secret":"hubpass"}`)
			os.Exit(0)
		case "helper.example.io":
			io.WriteString(os.Stdout, `{"Username":"<token>","Secret":"idtok"}`)
			os.Exit(0)
		}
		io.WriteString(os.Stdout, "credentials not found in native keychain\n")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestPullWithMissingCredentialHelper(t *testing.T) {
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var requireAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth.Load() {
			u, p, ok := r.BasicAuth()
			if !ok || u != "alice" || p != "secret" {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1)
	ref := host + "/test/cred:1"
	push(t, ref, "amd64", tarLayer(t, entry{name: "a", body: "x"}))

	cfgDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", cfgDir)
	t.Setenv("PATH", t.TempDir())

	// A missing credsStore binary (such as docker-credential-wincred on a
	// machine without Docker) must not break anonymous pulls.
	writeDockerConfig(t, cfgDir, `{"credsStore":"wincred"}`)
	if _, err := Open(t.TempDir()).Pull(context.Background(), ref); err != nil {
		t.Fatalf("anonymous pull with missing credsStore failed: %v", err)
	}

	// When the helper is absent or has no entry, inline auths in config.json
	// still authenticate the pull.
	requireAuth.Store(true)
	auth := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	writeDockerConfig(t, cfgDir, fmt.Sprintf(`{"credsStore":"wincred","auths":{%q:{"auth":%q}}}`, host, auth))
	if _, err := Open(t.TempDir()).Pull(context.Background(), ref); err != nil {
		t.Fatalf("authenticated pull with missing credsStore failed: %v", err)
	}
}

func TestKeychain(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", cfgDir)
	t.Setenv("REGISTRY_AUTH_FILE", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	helperPath := filepath.Join(binDir, "docker-credential-testwhp")
	if runtime.GOOS == "windows" {
		helperPath += ".exe"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	b64 := func(u, p string) string {
		return base64.StdEncoding.EncodeToString([]byte(u + ":" + p))
	}
	resolve := func(t *testing.T, ref string) (authn.Authenticator, *authn.AuthConfig) {
		t.Helper()
		r, err := name.ParseReference(ref)
		if err != nil {
			t.Fatal(err)
		}
		auth, err := Keychain.Resolve(r.Context())
		if err != nil {
			t.Fatalf("Resolve(%s): %v", ref, err)
		}
		ac, err := auth.Authorization()
		if err != nil {
			t.Fatalf("Authorization(%s): %v", ref, err)
		}
		return auth, ac
	}

	// No config.json -> anonymous.
	if auth, _ := resolve(t, "alpine:3.20"); auth != authn.Anonymous {
		t.Errorf("without config.json: got %v, want Anonymous", auth)
	}

	// Working credsStore and credHelpers, plus fallback to inline auths when
	// the helper has no credentials for a registry.
	writeDockerConfig(t, cfgDir, fmt.Sprintf(`{
		"credsStore": "testwhp",
		"credHelpers": {
			"https://helper.example.io/v2/": "testwhp",
			"missing.example.io": "no-such-helper"
		},
		"auths": {
			"test.io/repo-a": {"auth": %q},
			"http://test.io/v2/": {"auth": %q},
			"missing.example.io": {"auth": %q}
		}
	}`, b64("repo-user", "repo-pass"), b64("reg-user", "reg-pass"), b64("fb-user", "fb-pass")))

	if _, ac := resolve(t, "alpine:3.20"); ac.Username != "hubuser" || ac.Password != "hubpass" {
		t.Errorf("Docker Hub from credsStore: got %+v", ac)
	}
	if _, ac := resolve(t, "helper.example.io/app:1"); ac.Username != "<token>" || ac.IdentityToken != "idtok" {
		t.Errorf("credHelpers identity token: got %+v", ac)
	}
	if _, ac := resolve(t, "missing.example.io/app:1"); ac.Username != "fb-user" || ac.Password != "fb-pass" {
		t.Errorf("fallback when credHelper binary is missing: got %+v", ac)
	}
	if _, ac := resolve(t, "test.io/repo-a:1"); ac.Username != "repo-user" || ac.Password != "repo-pass" {
		t.Errorf("repo-specific auth: got %+v", ac)
	}
	if _, ac := resolve(t, "test.io/repo-b:1"); ac.Username != "reg-user" || ac.Password != "reg-pass" {
		t.Errorf("registry URL auth fallback: got %+v", ac)
	}
	if auth, _ := resolve(t, "unknown.example.io/app:1"); auth != authn.Anonymous {
		t.Errorf("unconfigured registry: got %v, want Anonymous", auth)
	}
}

func writeDockerConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
