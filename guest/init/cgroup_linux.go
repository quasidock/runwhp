//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// containerCgroup is the cgroup the container's processes run in, those
// of runwhp exec included, below the root one init stays in: freezing it
// pauses the container while init goes on serving the host.
type containerCgroup struct {
	dir    *os.File // the cgroup's directory, for CLONE_INTO_CGROUP
	freeze *os.File // cgroup.freeze
	events *os.File // cgroup.events
	mu     sync.Mutex
}

// cgroup is the container's cgroup, or nil if init could not make it; the
// container then runs in the root cgroup, and cannot be paused.
var cgroup *containerCgroup

// containerCgroupDir is the container's cgroup, below cgroupDir.
const containerCgroupDir = cgroupDir + "/container"

// makeContainerCgroup makes the container's cgroup, once the hierarchy is
// mounted at cgroupDir, and opens what init needs of it before the
// container's root hides it.
func makeContainerCgroup() {
	g, err := openCgroup(containerCgroupDir)
	if err != nil {
		logf("making the container's cgroup: %v", err)
		return
	}
	cgroup = g
}

// openCgroup makes the cgroup dir, if missing, and opens it.
func openCgroup(dir string) (*containerCgroup, error) {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	g := &containerCgroup{}
	var err error
	if g.dir, err = os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0); err == nil {
		if g.freeze, err = os.OpenFile(dir+"/cgroup.freeze", os.O_WRONLY, 0); err == nil {
			g.events, err = os.Open(dir + "/cgroup.events")
		}
	}
	if err != nil {
		for _, f := range []*os.File{g.dir, g.freeze, g.events} {
			if f != nil {
				f.Close()
			}
		}
		return nil, err
	}
	return g, nil
}

// procAttr puts a process to be started in the container's cgroup.
func (g *containerCgroup) procAttr(sys *syscall.SysProcAttr) {
	if g != nil {
		sys.UseCgroupFD, sys.CgroupFD = true, int(g.dir.Fd())
	}
}

// freezeTimeout bounds how long init waits for the kernel to freeze or
// thaw every process in the cgroup.
const freezeTimeout = 10 * time.Second

// setFrozen freezes or thaws the container's processes, and returns once
// the kernel says they are.
func (g *containerCgroup) setFrozen(frozen bool) error {
	if g == nil {
		return errors.New("the container has no cgroup of its own, so it cannot be paused")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	value := []byte("0")
	if frozen {
		value = []byte("1")
	}
	if _, err := g.freeze.WriteAt(value, 0); err != nil {
		return fmt.Errorf("writing cgroup.freeze: %w", err)
	}
	want := append([]byte("frozen "), value...)
	deadline := time.Now().Add(freezeTimeout)
	for {
		data, err := read(g.events)
		if err != nil {
			return fmt.Errorf("reading cgroup.events: %w", err)
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.Equal(line, want) {
				return nil
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			what := "thawed"
			if frozen {
				what = "frozen"
			}
			return fmt.Errorf("the container's processes were not %s after %v", what, freezeTimeout)
		}
		// cgroup.events wakes pollers with POLLPRI when it changes.
		fds := []unix.PollFd{{Fd: int32(g.events.Fd()), Events: unix.POLLPRI}}
		if _, err := unix.Poll(fds, int(min(left, 100*time.Millisecond)/time.Millisecond)); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("polling cgroup.events: %w", err)
		}
	}
}
