//go:build windows

// Package e2e runs the built runwhp.exe against real images. It needs the
// Windows Hypervisor Platform, network access to Docker Hub, the virtio_blk
// module in the store (see hack/wsl-modules.sh), and a build in out\ (see
// build.ps1). It is skipped unless RUNWHP_E2E=1.
package e2e

import (
	"archive/tar"
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const alpine = "alpine:3.20"

func runwhp(t *testing.T) string {
	t.Helper()
	if os.Getenv("RUNWHP_E2E") != "1" {
		t.Skip("set RUNWHP_E2E=1 to run end-to-end tests")
	}
	exe, err := filepath.Abs(filepath.Join("..", "..", "out", "runwhp.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("%v; run build.ps1 first", err)
	}
	return exe
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(runwhp(t), append([]string{"run"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{stdout.String(), stderr.String(), code}
}

func expect(t *testing.T, r result, stdout string, code int) {
	t.Helper()
	if r.stdout != stdout || r.code != code {
		t.Errorf("got stdout %q exit %d, want %q exit %d\nstderr: %s", r.stdout, r.code, stdout, code, r.stderr)
	}
}

func TestEcho(t *testing.T) {
	r := run(t, alpine, "echo", "hello")
	expect(t, r, "hello\n", 0)
	if r.stderr != "" && !strings.HasPrefix(r.stderr, "pulling") {
		t.Errorf("unexpected stderr %q", r.stderr)
	}
}

func TestStreamsAndExitCode(t *testing.T) {
	r := run(t, alpine, "sh", "-c", "echo out; echo err >&2; exit 3")
	expect(t, r, "out\n", 3)
	if r.stderr != "err\n" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestCommandErrors(t *testing.T) {
	r := run(t, alpine, "nosuchcmd")
	expect(t, r, "", 127)
	if !strings.Contains(r.stderr, "executable file not found") {
		t.Errorf("stderr = %q", r.stderr)
	}
	expect(t, run(t, alpine, "/etc/passwd"), "", 126)
	expect(t, run(t, alpine, "sh", "-c", "kill -9 $$"), "", 137)
}

func TestUserEnvWorkdir(t *testing.T) {
	expect(t, run(t, "-u", "nobody", alpine, "id"), "uid=65534(nobody) gid=65534(nobody)\n", 0)
	expect(t, run(t, "-e", "FOO=bar", "-w", "/tmp/x", alpine, "sh", "-c", "echo $FOO; pwd"), "bar\n/tmp/x\n", 0)
	expect(t, run(t, "--entrypoint", "/bin/echo", alpine, "a", "b"), "a b\n", 0)
	expect(t, run(t, "--hostname", "box", alpine, "hostname"), "box\n", 0)
}

func TestMultiLayerImage(t *testing.T) {
	expect(t, run(t, "python:3.12-slim", "python3", "-c", "print(1)"), "1\n", 0)
}

func TestParallel(t *testing.T) {
	runwhp(t)
	var wg sync.WaitGroup
	results := make([]result, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = run(t, alpine, "sh", "-c", "sleep 1; echo ok")
		}()
	}
	wg.Wait()
	for _, r := range results {
		expect(t, r, "ok\n", 0)
	}
}

// TestWhiteouts builds an image on alpine whose top layer deletes a file and
// makes a directory opaque, serves it from a localhost registry, and checks
// the container sees the merged result.
func TestWhiteouts(t *testing.T) {
	runwhp(t)
	base, err := remote.Image(name.MustParseReference(alpine), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	lower := tarLayer(t, map[string]string{"data/keep": "k", "data/gone": "g", "opq/old": "o"})
	upper := tarLayer(t, map[string]string{"data/.wh.gone": "", "opq/.wh..wh..opq": "", "opq/new": "n"})
	img, err := mutate.AppendLayers(base, lower, upper)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	ref := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1) + "/e2e/whiteouts:1"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	expect(t, run(t, "--pull", "always", ref, "sh", "-c", "ls /data /opq"), "/data:\nkeep\n\n/opq:\nnew\n", 0)
}

// TestInterrupt sends Ctrl+Break, which Go delivers as os.Interrupt like
// Ctrl+C, and expects the VM to stop with exit code 130.
func TestInterrupt(t *testing.T) {
	cmd := exec.Command(runwhp(t), "run", alpine, "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	const ctrlBreak = 1
	r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent").Call(ctrlBreak, uintptr(cmd.Process.Pid))
	if r == 0 {
		cmd.Process.Kill()
		t.Skipf("cannot send Ctrl+Break here (no shared console?): %v", err)
	}
	start := time.Now()
	err = cmd.Wait()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 130 {
		t.Errorf("exit = %v, want 130; stderr %q", err, stderr.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

func tarLayer(t *testing.T, files map[string]string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for p, body := range files {
		if d := filepath.ToSlash(filepath.Dir(p)); !dirs[d] {
			dirs[d] = true
			tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		io.WriteString(tw, body)
	}
	tw.Close()
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}
