//go:build linux

package main

import (
	"sync"
	"syscall"
	"testing"
	"time"
)

func startSh(t *testing.T, script string) (int, <-chan syscall.WaitStatus) {
	t.Helper()
	pid, done, err := children.start(func() (int, error) {
		return syscall.ForkExec("/bin/sh", []string{"sh", "-c", script}, &syscall.ProcAttr{})
	})
	if err != nil {
		t.Fatal(err)
	}
	return pid, done
}

func waitExit(t *testing.T, done <-chan syscall.WaitStatus) int {
	t.Helper()
	select {
	case ws := <-done:
		return exitCode(ws)
	case <-time.After(10 * time.Second):
		t.Fatal("no exit status")
		return -1
	}
}

func TestReaper(t *testing.T) {
	// A process that exits at once is not reaped before it is registered.
	for range 20 {
		_, done := startSh(t, "exit 3")
		if code := waitExit(t, done); code != 3 {
			t.Fatalf("exit %d, want 3", code)
		}
	}
	pid, done := startSh(t, "sleep 30")
	syscall.Kill(pid, syscall.SIGKILL)
	if code := waitExit(t, done); code != 137 {
		t.Errorf("killed: %d, want 137", code)
	}
	if _, _, err := children.start(func() (int, error) { return 0, syscall.ENOENT }); err == nil {
		t.Error("a failed fork reported success")
	}
}

func TestReaperConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, done := startSh(t, "exit "+string(rune('0'+i%10)))
			if code := waitExit(t, done); code != i%10 {
				t.Errorf("process %d: exit %d", i, code)
			}
		}()
	}
	wg.Wait()
	if !children.waitIdle(time.Now().Add(5 * time.Second)) {
		t.Error("children left")
	}
}
