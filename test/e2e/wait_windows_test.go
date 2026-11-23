//go:build windows

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestWait checks runwhp wait: it prints the exit code of a container once
// it stops, at once for one that has stopped, one line per container.
func TestWait(t *testing.T) {
	a, b := newName(t), newName(t)
	detached(t, "--name", a, alpine, "sh", "-c", "sleep 1; exit 7")
	detached(t, "--name", b, alpine, "sh", "-c", "exit 3")
	begin := time.Now()
	expect(t, cli(t, "wait", a, b), "7\n3\n", 0)
	if d := time.Since(begin); d < 500*time.Millisecond {
		t.Errorf("wait returned after %v, before the container stopped", d)
	}
	// Stopped already.
	expect(t, cli(t, "wait", a), "7\n", 0)
	if r := cli(t, "wait", "e2e-nosuchcontainer"); r.code != 1 || r.stdout != "" {
		t.Errorf("wait for a missing container: %+v", r)
	}
}

// TestWaitRemoved checks that a container run with --rm answers wait before
// it is removed.
func TestWaitRemoved(t *testing.T) {
	name := newName(t)
	detached(t, "--rm", "--name", name, alpine, "sh", "-c", "sleep 1; exit 4")
	expect(t, cli(t, "wait", name), "4\n", 0)
}

// TestWaitSupervisorKilled checks that wait answers with what the store
// records when the runwhp running the container is killed meanwhile.
func TestWaitSupervisorKilled(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "echo ready; sleep 300")
	waitLog(t, name, "ready")
	cmd := exec.Command(runwhp(t), "wait", name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(stateOf(t, name).Pid))
	if err != nil {
		t.Fatal(err)
	}
	windows.TerminateProcess(h, 1)
	windows.CloseHandle(h)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || strings.TrimSpace(stdout.String()) != "255" {
			t.Errorf("wait: %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("wait still waiting 20 s after the runwhp was killed; stderr %q", stderr.String())
	}
}
