//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFreeze starts a process in a cgroup of its own, as init starts the
// container's, freezes and thaws it. It needs a writable cgroup v2
// hierarchy: run it as root in a privileged container.
func TestFreeze(t *testing.T) {
	base := "/sys/fs/cgroup"
	if _, err := os.ReadFile(base + "/cgroup.controllers"); err != nil || os.Getuid() != 0 {
		t.Skipf("needs root and cgroup v2 at %s (%v): run it in a privileged container", base, err)
	}
	dir := filepath.Join(base, "runwhp-test-freeze")
	g, err := openCgroup(dir)
	if err != nil {
		t.Skipf("cannot make a cgroup: %v", err)
	}
	t.Cleanup(func() { syscall.Rmdir(dir) })

	sys := &syscall.SysProcAttr{}
	g.procAttr(sys)
	pid, err := syscall.ForkExec("/bin/sh", []string{"sh", "-c", "while :; do sleep 0.01; done"}, &syscall.ProcAttr{Sys: sys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		syscall.Wait4(pid, &ws, 0, nil)
	})
	procs, _ := os.ReadFile(dir + "/cgroup.procs")
	if !strings.Contains(string(procs), strconv.Itoa(pid)) {
		t.Fatalf("the process is not in the cgroup: %q", procs)
	}

	start := time.Now()
	if err := g.setFrozen(true); err != nil {
		t.Fatal(err)
	}
	t.Logf("froze in %v", time.Since(start))
	events, _ := os.ReadFile(dir + "/cgroup.events")
	if !strings.Contains(string(events), "frozen 1") {
		t.Errorf("cgroup.events: %q", events)
	}
	if err := g.setFrozen(false); err != nil {
		t.Fatal(err)
	}
	events, _ = os.ReadFile(dir + "/cgroup.events")
	if !strings.Contains(string(events), "frozen 0") {
		t.Errorf("cgroup.events: %q", events)
	}

	// SIGKILL ends a frozen process, which then leaves the cgroup.
	if err := g.setFrozen(true); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil || ws.Signal() != syscall.SIGKILL {
		t.Errorf("wait: %v %v", err, ws)
	}
	g.setFrozen(false)
}
