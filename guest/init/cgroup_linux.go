//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
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

// delegatedControllers are those a privileged container gets, for the
// cgroups it makes below its own, as docker:dind and BuildKit make them.
var delegatedControllers = []string{"cpu", "cpuset", "io", "memory", "pids"}

// initCgroupNS is init's own cgroup namespace, which a thread that has
// entered the container's to start an exec goes back to.
var initCgroupNS *os.File

// mainPid is the container process's pid, once it has started: execs enter
// its cgroup namespace.
var mainPid atomic.Int32

// bindCgroup shows the container its cgroup at /sys/fs/cgroup, as the root
// of a cgroup namespace of its own, as Docker does: read-only, unless the
// container is privileged, which also gets the controllers enabled for it
// to delegate. It is called before enterRoot, while the hierarchy init
// mounted for itself is still in reach.
func bindCgroup(spec *guestspec.Spec) {
	if cgroup == nil {
		return
	}
	ns, err := os.Open("/proc/self/ns/cgroup")
	if err != nil {
		logf("opening init's cgroup namespace: %v", err)
		return
	}
	initCgroupNS = ns
	if spec.Privileged {
		enableControllers(cgroupDir + "/cgroup.subtree_control")
	}
	target := filepath.Join(root, "sys/fs/cgroup")
	if err := syscall.Mount(containerCgroupDir, target, "", syscall.MS_BIND, ""); err != nil {
		logf("binding the container's cgroup at /sys/fs/cgroup: %v", err)
		return
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if !spec.Privileged {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("", target, "", flags, ""); err != nil {
		logf("remounting /sys/fs/cgroup: %v", err)
	}
}

// enableControllers enables delegatedControllers in a subtree_control
// file, each that the kernel has.
func enableControllers(file string) {
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		logf("opening %s: %v", file, err)
		return
	}
	defer f.Close()
	for _, c := range delegatedControllers {
		if _, err := f.Write([]byte("+" + c)); err != nil {
			debugf("enabling the %s controller: %v", c, err)
		}
	}
}

// unshareFlags are those the container process starts with: a cgroup
// namespace of its own, rooted at the container's cgroup, which it is
// cloned into before it unshares.
func (g *containerCgroup) unshareFlags() uintptr {
	if g == nil || initCgroupNS == nil {
		return 0
	}
	return syscall.CLONE_NEWCGROUP
}

// forkExec starts an exec's process in the container: in its cgroup, or,
// once the container has cgroups of its own below it, as docker:dind makes
// them, in its main process's, as runc does, since a cgroup that delegates
// controllers takes no processes; and in its cgroup namespace.
func (g *containerCgroup) forkExec(exe string, args []string, attr *syscall.ProcAttr) (int, error) {
	g.procAttr(attr.Sys)
	main := mainPid.Load()
	if g == nil || initCgroupNS == nil || main == 0 {
		return syscall.ForkExec(exe, args, attr)
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/cgroup", main))
	if err != nil {
		return syscall.ForkExec(exe, args, attr)
	}
	defer ns.Close()
	// The namespace is the thread's: it goes back to init's before the
	// thread is let go, or dies with the goroutine.
	runtime.LockOSThread()
	if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWCGROUP); err != nil {
		runtime.UnlockOSThread()
		logf("entering the container's cgroup namespace: %v", err)
		return syscall.ForkExec(exe, args, attr)
	}
	pid, err := syscall.ForkExec(exe, args, attr)
	if errors.Is(err, syscall.EBUSY) {
		if dir, derr := g.processDir(main); derr == nil {
			attr.Sys.CgroupFD = int(dir.Fd())
			pid, err = syscall.ForkExec(exe, args, attr)
			dir.Close()
		} else {
			logf("finding the container process's cgroup: %v", derr)
		}
	}
	if serr := unix.Setns(int(initCgroupNS.Fd()), unix.CLONE_NEWCGROUP); serr == nil {
		runtime.UnlockOSThread()
	}
	return pid, err
}

// processDir opens the cgroup of process pid, which is in the container's,
// from the calling thread, which is in the container's cgroup namespace:
// /proc gives its path from the container's cgroup.
func (g *containerCgroup) processDir(pid int32) (*os.File, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return nil, err
	}
	rel, ok := unifiedPath(data)
	if !ok {
		return nil, fmt.Errorf("no cgroup v2 path in %q", data)
	}
	if rel == "" {
		return nil, errors.New("the container process is in the container's cgroup, which takes no processes")
	}
	fd, err := unix.Openat(int(g.dir.Fd()), rel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// unifiedPath is the cgroup v2 path in /proc/PID/cgroup, without its
// leading slash: empty for the namespace's root.
func unifiedPath(data []byte) (string, bool) {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if p, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			return string(bytes.TrimPrefix(p, []byte("/"))), true
		}
	}
	return "", false
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
