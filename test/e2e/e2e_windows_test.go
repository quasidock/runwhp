//go:build windows

// Package e2e runs the built runwhp.exe against real images. It needs the
// Windows Hypervisor Platform, network access to Docker Hub and to ghcr.io,
// where the first run fetches runwhp's kernel from, and a build in out\ (see
// build.ps1). It is skipped unless RUNWHP_E2E=1.
//
// RUNWHP_KERNEL, which runwhp itself reads, boots a locally built kernel
// instead of the pinned one. RUNWHP_E2E_RUN_FLAGS adds flags, separated by
// semicolons, to every runwhp run; for example
//
//	--kernel=C:\Program Files\WSL\tools\kernel;--module=C:\wsl\virtio_blk.ko
//
// runs the suite on the WSL kernel. Tests of runwhp's own kernel are then
// skipped.
package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// runArgs is the argument list of runwhp run --rm, with
// RUNWHP_E2E_RUN_FLAGS before args. --rm keeps the tests from leaving
// containers behind; keepArgs leaves it out.
func runArgs(args ...string) []string {
	return append([]string{"run", "--rm"}, withFlags(args)...)
}

// keepArgs is the argument list of runwhp run, which keeps the container,
// with RUNWHP_E2E_RUN_FLAGS before args.
func keepArgs(args ...string) []string {
	return append([]string{"run"}, withFlags(args)...)
}

func withFlags(args []string) []string {
	var out []string
	for _, f := range filepath.SplitList(os.Getenv("RUNWHP_E2E_RUN_FLAGS")) {
		if f != "" {
			out = append(out, f)
		}
	}
	return append(out, args...)
}

// cli runs runwhp with exactly args.
func cli(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(runwhp(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	return runWithInput(t, nil, args...)
}

// runWithInput runs with stdin, or with no stdin (the null device) if nil.
func runWithInput(t *testing.T, stdin io.Reader, args ...string) result {
	t.Helper()
	cmd := exec.Command(runwhp(t), runArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
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
	// The registry goes with the test, and so does the image.
	t.Cleanup(func() { cli(t, "rmi", "-f", ref) })
	expect(t, run(t, "--pull", "always", ref, "sh", "-c", "ls /data /opq"), "/data:\nkeep\n\n/opq:\nnew\n", 0)
}

func TestStdin(t *testing.T) {
	expect(t, runWithInput(t, strings.NewReader("hello\n"), "-i", alpine, "cat"), "hello\n", 0)
	expect(t, runWithInput(t, strings.NewReader(""), "-i", alpine, "wc", "-c"), "0\n", 0)
	// Without -i the process reads the null device, even with input there.
	expect(t, runWithInput(t, strings.NewReader("ignored"), alpine, "cat"), "", 0)
}

func TestStdinLarge(t *testing.T) {
	data := make([]byte, 10<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	r := runWithInput(t, bytes.NewReader(data), "-i", alpine, "sha256sum")
	expect(t, r, hex.EncodeToString(sum[:])+"  -\n", 0)
}

// TestStdinDelayed writes input only once the guest has been idle a while,
// so the vCPU is halted in the hypervisor when the data arrives.
func TestStdinDelayed(t *testing.T) {
	cmd := exec.Command(runwhp(t), runArgs("-i", alpine, "sh", "-c", "read x; echo got $x")...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	sent := time.Now()
	io.WriteString(in, "late\n")
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v; stderr %q", err, stderr.String())
	}
	if stdout.String() != "got late\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if d := time.Since(sent); d > time.Second {
		t.Errorf("took %v after the input arrived", d)
	}
}

// TestSlowReader leaves stdout unread until stderr has shown progress. When
// output was written on the vCPU thread, the full stdout pipe stopped the
// whole guest and this deadlocked.
func TestSlowReader(t *testing.T) {
	const n = 4000000
	cmd := exec.Command(runwhp(t), runArgs(alpine, "sh", "-c", fmt.Sprintf("head -c %d /dev/zero & sleep 1; echo tick >&2; wait", n))...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ticked := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if sc.Text() == "tick" {
				ticked <- true
				io.Copy(io.Discard, stderr)
				return
			}
		}
		ticked <- false
	}()
	select {
	case ok := <-ticked:
		if !ok {
			t.Fatal("stderr ended without tick")
		}
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("stderr stalled behind unread stdout")
	}
	got, _ := io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil || got != n {
		t.Errorf("read %d bytes of stdout, exit %v", got, err)
	}
}

// TestReopenStdio opens the standard streams by name. They used to be the
// console ports themselves, which can be open only once.
func TestReopenStdio(t *testing.T) {
	r := runWithInput(t, strings.NewReader("in\n"), "-i", alpine, "sh", "-c", "cat /dev/stdin; echo a > /dev/stdout; echo b | tee /dev/stderr >/dev/null")
	expect(t, r, "in\na\n", 0)
	if r.stderr != "b\n" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

// interruptible starts runwhp in a process group of its own, waits for ready
// on stdout, and returns a function that sends it Ctrl+Break, which Go
// delivers as os.Interrupt like Ctrl+C.
func interruptible(t *testing.T, stdout *syncBuffer, stderr *bytes.Buffer, args ...string) (*exec.Cmd, func()) {
	t.Helper()
	return interruptibleArgv(t, stdout, stderr, runArgs(args...))
}

// interruptibleArgv is interruptible for runwhp with exactly argv.
func interruptibleArgv(t *testing.T, stdout *syncBuffer, stderr *bytes.Buffer, argv []string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(runwhp(t), argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(stdout.String(), "ready"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("container never got ready; stderr %q", stderr.String())
		}
	}
	return cmd, func() {
		const ctrlBreak = 1
		r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent").Call(ctrlBreak, uintptr(cmd.Process.Pid))
		if r == 0 {
			cmd.Process.Kill()
			t.Skipf("cannot send Ctrl+Break here (no shared console?): %v", err)
		}
	}
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// TestInterrupt checks that Ctrl+Break reaches a container that does not
// handle it, which then dies of SIGINT.
func TestInterrupt(t *testing.T) {
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, alpine, "sh", "-c", "echo ready; sleep 30")
	interrupt()
	start := time.Now()
	if code := exitCode(cmd.Wait()); code != 130 {
		t.Errorf("exit = %d, want 130; stderr %q", code, stderr.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

// TestInterruptTrapped checks that the container can handle the interrupt.
func TestInterruptTrapped(t *testing.T) {
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, alpine, "sh", "-c", `trap "echo bye; exit 5" INT; echo ready; sleep 30 & wait`)
	interrupt()
	if code := exitCode(cmd.Wait()); code != 5 || stdout.String() != "ready\nbye\n" {
		t.Errorf("exit %d, stdout %q, want 5 and bye; stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestSecondInterrupt checks that a second interrupt stops a container that
// ignores the first.
func TestSecondInterrupt(t *testing.T) {
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, alpine, "sh", "-c", `trap "" INT; echo ready; sleep 30`)
	exited := make(chan error, 1)
	interrupt()
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("stopped after the first interrupt: %v", err)
	case <-time.After(time.Second):
	}
	interrupt()
	start := time.Now()
	if code := exitCode(<-exited); code != 130 {
		t.Errorf("exit = %d, want 130; stderr %q", code, stderr.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

// syncBuffer is a bytes.Buffer that can be read while a process writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
