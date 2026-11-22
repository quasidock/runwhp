//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// supervisor is the runwhp process that runs a container: it holds the
// container's lock for as long as the container has a VM, and records its
// state as it changes.
type supervisor struct {
	store *container.Store
	rec   *container.Container
	lock  *container.Lock

	mu    sync.Mutex
	state container.State
}

// supervise takes a container's lock and records that this process is
// booting it, with its output going where output says.
func supervise(store *container.Store, rec *container.Container, output string) (*supervisor, error) {
	lock, err := store.Lock(rec)
	if err != nil {
		return nil, err
	}
	s := &supervisor{store: store, rec: rec, lock: lock, state: rec.State}
	s.update(func(st *container.State) {
		*st = container.State{Status: container.Creating, Pid: os.Getpid(), Output: output}
	})
	return s, nil
}

// update changes the container's state and records it.
func (s *supervisor) update(f func(*container.State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.state)
	if err := s.lock.WriteState(s.state); err != nil {
		errorf("recording the state of container %s: %v", s.rec.Short(), err)
	}
}

// started records that the container process runs, as pid in the guest.
func (s *supervisor) started(pid int) {
	s.update(func(st *container.State) {
		st.Status, st.GuestPid, st.StartedAt = container.Running, pid, time.Now().UTC()
	})
}

// finish records how the container ended, gives up its lock, and removes
// it if it was to be removed when it stopped.
func (s *supervisor) finish(code int, err error) {
	s.update(func(st *container.State) {
		st.Status, st.Pid, st.GuestPid = container.Stopped, 0, 0
		st.FinishedAt, st.ExitCode, st.Error = time.Now().UTC(), code, ""
		if err != nil {
			st.Error = err.Error()
			if errors.Is(err, context.Canceled) {
				st.Error = "stopped by runwhp after a second interrupt"
			}
		}
		if code == guestspec.ExitRuntime && st.Error == "" {
			st.Error = "runwhp failed"
		}
	})
	s.lock.Close()
	if s.rec.Config.AutoRemove {
		if err := s.store.Remove(s.rec); err != nil {
			errorf("removing container %s: %v", s.rec.Short(), err)
		}
	}
}
