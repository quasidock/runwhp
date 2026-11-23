//go:build windows

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// attachCmd starts runwhp attach in the background, with stdin as given
// (the null device if empty), and returns it with its output.
func attachCmd(t *testing.T, stdin string, args ...string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
	t.Helper()
	cmd := exec.Command(runwhp(t), append([]string{"attach"}, args...)...)
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, &stdout, &stderr
}

// waitOutput waits for b to hold want.
func waitOutput(t *testing.T, b *syncBuffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(b.String(), want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the output: %q", want, b.String())
		}
	}
}

// TestAttachOutput attaches two runwhps to a container in the background:
// both see its output from then on, and exit with its exit code.
func TestAttachOutput(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "for i in $(seq 1 30); do echo line$i; sleep 0.1; done; echo err >&2; exit 6")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := cli(t, "attach", name)
			if r.code != 6 || !strings.Contains(r.stdout, "line30\n") || r.stderr != "err\n" {
				t.Errorf("attach: %+v", r)
			}
		}()
	}
	wg.Wait()
	if r := cli(t, "attach", name); r.code != 1 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("attach to a stopped container: %+v", r)
	}
}

// TestAttachForeground attaches to a container run in the foreground by
// another runwhp: its output, but not its input, which is that runwhp's.
func TestAttachForeground(t *testing.T) {
	name := newName(t)
	cmd := exec.Command(runwhp(t), keepArgs("-i", "--name", name, alpine, "sh", "-c", "echo ready; sleep 2; echo late; exit 4")...)
	var stdout syncBuffer
	cmd.Stdout = &stdout
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	waitOutput(t, &stdout, "ready")
	r := cli(t, "attach", name)
	if r.code != 4 || r.stdout != "late\n" {
		t.Errorf("attach: %+v", r)
	}
}

// TestAttachStdin attaches input to a container run in the background with
// -i: its input stays open for the next runwhp to attach.
func TestAttachStdin(t *testing.T) {
	name := newName(t)
	detached(t, "-i", "--name", name, alpine, "sh", "-c", "echo ready; while read l; do echo got $l; done; echo input ended")
	waitLog(t, name, "ready")
	for _, line := range []string{"a", "b"} {
		cmd, stdout, _ := attachCmd(t, line+"\n", name)
		waitOutput(t, stdout, "got "+line+"\n")
		cmd.Process.Kill()
		cmd.Wait()
	}
	if s := stateOf(t, name); s.Status != "running" {
		t.Errorf("the container is %s after two attaches ended their input", s.Status)
	}
	if r := cli(t, "logs", name); !strings.Contains(r.stdout, "got a\ngot b\n") || strings.Contains(r.stdout, "input ended") {
		t.Errorf("logs: %q", r.stdout)
	}
}

// TestStartAttach checks start -a: a created container's output from the
// first byte, again once it has stopped, and with -i input that ends the
// container's once it ends, as create -i takes input once.
func TestStartAttach(t *testing.T) {
	name := newName(t)
	created(t, "--name", name, alpine, "sh", "-c", "echo hi; echo there >&2; exit 2")
	for range 2 {
		r := cli(t, "start", "-a", name)
		if r.stdout != "hi\n" || r.stderr != "there\n" || r.code != 2 {
			t.Errorf("start -a: %+v", r)
		}
	}
	// Output redirected to files comes once, through the attach, not also
	// from the runwhp that start -a starts, which once inherited them.
	dir := t.TempDir()
	stdout, _ := os.Create(filepath.Join(dir, "stdout"))
	stderr, _ := os.Create(filepath.Join(dir, "stderr"))
	cmd := exec.Command(runwhp(t), "start", "-a", name)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.Close()
	stderr.Close()
	gotOut, _ := os.ReadFile(filepath.Join(dir, "stdout"))
	gotErr, _ := os.ReadFile(filepath.Join(dir, "stderr"))
	if exitCode(err) != 2 || string(gotOut) != "hi\n" || string(gotErr) != "there\n" {
		t.Errorf("start -a to files: exit %d, stdout %q, stderr %q", exitCode(err), gotOut, gotErr)
	}

	cat := newName(t)
	created(t, "-i", "--name", cat, alpine, "sh", "-c", "cat; echo input ended")
	cmd = exec.Command(runwhp(t), "start", "-a", "-i", cat)
	cmd.Stdin = strings.NewReader("hello\n")
	out, err := cmd.Output()
	if err != nil || string(out) != "hello\ninput ended\n" {
		t.Errorf("start -ai: %v, %q", err, out)
	}

	if r := cli(t, "start", "-a", name, cat); r.code != 2 || !strings.Contains(r.stderr, "one container") {
		t.Errorf("start -a with two containers: %+v", r)
	}
}

// TestAttachTTY attaches a pseudo console to a container in the background
// with a terminal: keys go to its shell, the size follows the console's,
// Ctrl+P Ctrl+Q detaches and leaves it running, and the next attach gets
// its exit code.
func TestAttachTTY(t *testing.T) {
	name := newName(t)
	detached(t, "-it", "--name", name, alpine, "sh")
	p := startPTYArgv(t, 30, 100, []string{"attach", name})
	time.Sleep(500 * time.Millisecond)
	p.typeKeys("echo $((6*7))\r")
	p.waitFor("42")
	p.typeKeys("stty size\r")
	p.waitFor("30 100")
	if err := resizePTY(p, 40, 120); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	p.typeKeys("stty size\r")
	p.waitFor("40 120")
	p.typeKeys("\x10\x11")
	if code := p.wait(); code != 0 {
		t.Errorf("detaching exited %d; the terminal shows %q", code, p.text())
	}
	if s := stateOf(t, name); s.Status != "running" {
		t.Fatalf("the container is %s after detaching", s.Status)
	}

	p = startPTYArgv(t, 30, 100, []string{"attach", name})
	time.Sleep(500 * time.Millisecond)
	p.typeKeys("exit 3\r")
	if code := p.wait(); code != 3 {
		t.Errorf("exit %d; the terminal shows %q", code, p.text())
	}
}
