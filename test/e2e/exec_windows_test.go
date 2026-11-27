//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// running starts a container in the background that runs until the test
// ends, and returns its name.
func running(t *testing.T, args ...string) string {
	t.Helper()
	name := newName(t)
	detached(t, append([]string{"--name", name}, append(args, alpine, "sleep", "300")...)...)
	return name
}

func execIn(t *testing.T, args ...string) result {
	t.Helper()
	return cli(t, append([]string{"exec"}, args...)...)
}

func TestExec(t *testing.T) {
	parallel(t)
	name := running(t, "-e", "FROM_RUN=yes", "-w", "/srv")
	begin := time.Now()
	r := execIn(t, name, "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.stdout != "out\n" || r.stderr != "err\n" || r.code != 3 {
		t.Errorf("exec: %+v", r)
	}
	t.Logf("exec took %v", time.Since(begin))
	expect(t, execIn(t, name, "sh", "-c", "echo $FROM_RUN $(pwd) $(id -u) $HOSTNAME"), "yes /srv 0 "+stateOf(t, name).ID[:12]+"\n", 0)
	expect(t, execIn(t, "-e", "FOO=bar", "-w", "/tmp", "-u", "nobody", name, "sh", "-c", "echo $FOO $(pwd) $(id -u) $HOME"), "bar /tmp 65534 /\n", 0)
	if r := execIn(t, name, "nosuchcommand"); r.code != 127 || !strings.Contains(r.stderr, "nosuchcommand") {
		t.Errorf("a missing command: %+v", r)
	}
	if r := execIn(t, name, "/etc/passwd"); r.code != 126 {
		t.Errorf("a command that is not executable: %+v", r)
	}
	if r := execIn(t, "-w", "/nope", name, "true"); r.code != 126 || !strings.Contains(r.stderr, "/nope") {
		t.Errorf("a missing working directory: %+v", r)
	}
	if r := execIn(t, "-u", "nosuchuser", name, "true"); r.code != 126 {
		t.Errorf("a missing user: %+v", r)
	}
	// A process exec started is the container's: it sees its files.
	expect(t, execIn(t, name, "sh", "-c", "echo hi > /from-exec"), "", 0)
	expect(t, execIn(t, name, "cat", "/from-exec"), "hi\n", 0)
	if r := execIn(t, "e2e-no-such-container", "true"); r.code != 125 {
		t.Errorf("exec in a missing container: %+v", r)
	}
}

func TestExecNotRunning(t *testing.T) {
	parallel(t)
	name := newName(t)
	created(t, "--name", name, alpine, "true")
	if r := execIn(t, name, "true"); r.code != 125 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("exec in a created container: %+v", r)
	}
	cli(t, "start", name)
	waitStatus(t, name, "stopped", 10*time.Second)
	if r := execIn(t, name, "true"); r.code != 125 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("exec in a stopped container: %+v", r)
	}
}

func TestExecStdin(t *testing.T) {
	parallel(t)
	name := running(t)
	data := make([]byte, 10<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	cmd := command(t, "exec", "-i", name, "sh", "-c", "sha256sum | cut -d' ' -f1")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != hex.EncodeToString(sum[:]) {
		t.Errorf("checksum %q, %v", out, err)
	}
	// Without -i, the command reads nothing.
	expect(t, execIn(t, name, "wc", "-c"), "0\n", 0)
}

func TestExecDetach(t *testing.T) {
	name := running(t)
	begin := time.Now()
	expect(t, execIn(t, "-d", name, "sh", "-c", "sleep 1; touch /detached"), "", 0)
	if d := time.Since(begin); d > 900*time.Millisecond {
		t.Errorf("exec -d took %v", d)
	}
	expect(t, execIn(t, name, "sh", "-c", "test -f /detached && echo early || echo not yet"), "not yet\n", 0)
	for deadline := time.Now().Add(10 * time.Second); execIn(t, name, "test", "-f", "/detached").code != 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the detached command never ran")
		}
	}
}

// TestExecConcurrent runs four commands at once, each streaming 20 MB.
func TestExecConcurrent(t *testing.T) {
	parallel(t)
	name := running(t)
	h := sha256.New()
	h.Write(make([]byte, 20<<20))
	want := hex.EncodeToString(h.Sum(nil))
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := command(t, "exec", name, "head", "-c", fmt.Sprint(20<<20), "/dev/zero")
			out, err := cmd.Output()
			sum := sha256.Sum256(out)
			if err != nil || hex.EncodeToString(sum[:]) != want {
				t.Errorf("exec %d: %d bytes, %v", i, len(out), err)
			}
		}()
	}
	wg.Wait()
}

// TestExecEndsWithContainer checks that a command exec started is killed
// when the container's process exits.
func TestExecEndsWithContainer(t *testing.T) {
	parallel(t)
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "echo ready; sleep 1")
	waitLog(t, name, "ready")
	begin := time.Now()
	r := execIn(t, name, "sleep", "60")
	if r.code != 137 {
		t.Errorf("exec when the container ended: %+v", r)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("exec returned %v after", d)
	}
}

// TestExecForeground runs a command in a container a foreground run runs.
func TestExecForeground(t *testing.T) {
	parallel(t)
	name := newName(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd := command(t, keepArgs("--name", name, alpine, "sh", "-c", "echo ready; while [ ! -f /stop ]; do sleep 0.1; done; echo stopped")...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(stdout.String(), "ready"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("container never got ready")
		}
	}
	expect(t, execIn(t, name, "touch", "/stop"), "", 0)
	if code := exitCode(cmd.Wait()); code != 0 || stdout.String() != "ready\nstopped\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestExecInterrupt sends Ctrl+Break to runwhp exec, which passes it on
// to the command as SIGINT.
func TestExecInterrupt(t *testing.T) {
	parallel(t)
	name := running(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptibleArgv(t, &stdout, &stderr, []string{"exec", name, "sh", "-c", "trap 'echo got INT; exit 9' INT; echo ready; while :; do sleep 0.1; done"})
	interrupt()
	if code := exitCode(cmd.Wait()); code != 9 || !strings.Contains(stdout.String(), "got INT") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestExecTTY(t *testing.T) {
	name := running(t)
	p := startPTYArgv(t, 30, 100, []string{"exec", "-it", name, "sh"})
	p.typeKeys("tty; stty size; echo $TERM\r")
	p.waitFor("/dev/pts/")
	p.waitFor("30 100")
	p.waitFor("xterm")
	if err := resizePTY(p, 40, 120); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	p.typeKeys("stty size\r")
	p.waitFor("40 120")
	p.typeKeys("exit 4\r")
	if code := p.wait(); code != 4 {
		t.Errorf("exit %d; the terminal shows %q", code, p.text())
	}
}

func resizePTY(p *pty, rows, cols int16) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: cols, Y: rows})
}
