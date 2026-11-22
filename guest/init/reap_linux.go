//go:build linux

package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// reaper waits for every child of init, which as PID 1 adopts orphans too,
// and hands each exit status to whoever started that process: the
// container's process, or one runwhp exec started.
//
// It reaps only while holding its lock, which start holds while it forks
// and registers a process, so a process that exits at once cannot be
// reaped before anyone waits for it.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
}

var children = newReaper()

func newReaper() *reaper {
	r := &reaper{waiters: map[int]chan syscall.WaitStatus{}}
	wake := make(chan os.Signal, 1)
	signal.Notify(wake, syscall.SIGCHLD)
	go func() {
		// SIGCHLD wakes it; the ticker covers a signal that came while it
		// was reaping, in case.
		t := time.NewTicker(100 * time.Millisecond)
		for {
			select {
			case <-wake:
			case <-t.C:
			}
			r.collect()
		}
	}()
	return r
}

// collect reaps every child that has exited, and reports whether init has
// no children left.
func (r *reaper) collect() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.ECHILD:
			return true
		case err != nil || pid <= 0:
			return false
		}
		if ch, ok := r.waiters[pid]; ok {
			delete(r.waiters, pid)
			ch <- ws
		}
	}
}

// start runs fork, which starts a process and returns its pid, and returns
// a channel that will get its exit status.
func (r *reaper) start(fork func() (int, error)) (int, <-chan syscall.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := fork()
	if err != nil {
		return 0, nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = ch
	return pid, ch, nil
}

// waitIdle waits until init has no children left, or until the deadline.
func (r *reaper) waitIdle(deadline time.Time) bool {
	for {
		if r.collect() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// exitCode is how a process ended, as Docker reports it: its exit status,
// or 128 plus the signal that killed it.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return guestspec.ExitSignalBase + int(ws.Signal())
	}
	return ws.ExitStatus()
}
