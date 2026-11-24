//go:build linux

package main

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/passwd"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// resolveUser resolves the container's user against the image's
// /etc/passwd and /etc/group.
func resolveUser(spec *guestspec.Spec) (passwd.User, error) {
	pw, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	gr, _ := os.ReadFile(filepath.Join(root, "etc/group"))
	user, err := passwd.Resolve(spec.User, pw, gr)
	if err != nil {
		return user, fail(guestspec.ExitRuntime, "%v", err)
	}
	return user, nil
}

// runProcess starts the container process, once enterRoot has made the
// overlay the root, waits for it, kills whatever it left behind, and waits
// for the last of its output to reach the ports.
func runProcess(spec *guestspec.Spec, user passwd.User, ports map[string]*os.File, ctl *controller, mem *memFiles) (guestspec.Status, error) {
	if len(spec.Args) == 0 {
		return guestspec.Status{}, fail(guestspec.ExitRuntime, "no command specified")
	}
	if spec.WaitStart {
		ctl.send(guestspec.Event{Ready: true})
		debugf("ready; waiting for the host to start the process")
		if sig := ctl.waitStart(); sig != 0 {
			debugf("ended by signal %d before it started", sig)
			return guestspec.Status{Exit: guestspec.ExitSignalBase + sig}, nil
		}
	}
	if spec.ClockSync {
		// The time is on its way as the control port opens, but on its own
		// goroutine: the process must not read the clock before it is set.
		if took, ok := ctl.waitClock(clockWait); ok {
			debugf("clock set from the host's after waiting %v", took)
		} else {
			logf("no time from the host within %v; starting with the boot clock", clockWait)
		}
	}

	env := processEnv(spec.Env, spec.Hostname, user, spec.TTY)

	cwd := spec.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if !path.IsAbs(cwd) {
		return guestspec.Status{}, fail(guestspec.ExitRuntime, "working directory %q is not absolute", cwd)
	}
	if err := os.MkdirAll(filepath.Join(root, cwd), 0o755); err != nil {
		return guestspec.Status{}, fail(guestspec.ExitRuntime, "creating working directory %s: %v", cwd, err)
	}

	exe, err := lookPath(spec.Args[0], cwd, lookupEnv(env, "PATH"))
	if err != nil {
		return guestspec.Status{}, err
	}

	streams, err := newStdio(spec, ports, user.UID)
	if err != nil {
		return guestspec.Status{}, err
	}
	ctl.setTerminal(streams.master)
	attr := &syscall.ProcAttr{
		Dir:   cwd,
		Env:   env,
		Files: streams.fds(),
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: user.UID, Gid: user.GID, Groups: user.Groups},
			Setsid:     true,
			Setctty:    spec.TTY,
			Ctty:       0,
		},
	}
	cgroup.procAttr(attr.Sys)
	debugf("exec %q as %d:%d in %s (stdin %v, tty %v)", spec.Args, user.UID, user.GID, cwd, spec.Stdin, spec.TTY)
	pid, done, err := children.start(func() (int, error) { return syscall.ForkExec(exe, spec.Args, attr) })
	streams.started()
	if err != nil {
		return guestspec.Status{}, execFailure(spec.Args[0], err)
	}
	ctl.start(pid)
	ctl.send(guestspec.Event{Started: true, Pid: pid})
	execs := startExecServer(spec)
	stopReclaim := make(chan struct{})
	reclaimed := make(chan struct{})
	go func() {
		defer close(reclaimed)
		mem.reclaimLoop(spec.Reclaim, stopReclaim)
	}()

	status := guestspec.Status{Exit: exitCode(<-done)}
	close(stopReclaim)
	<-reclaimed
	debugf("process exited with %d", status.Exit)
	ctl.exited()
	cleanUp()
	execs.wait(2 * time.Second)
	if !streams.wait(2 * time.Second) {
		logf("output still pending 2 s after the processes ended; reporting without it")
	}
	return status, nil
}

// processEnv is a process's environment: env, with PATH, HOSTNAME, HOME
// and, with a terminal, TERM set where env does not set them.
func processEnv(env []string, hostname string, user passwd.User, tty bool) []string {
	env = append([]string(nil), env...)
	if lookupEnv(env, "PATH") == "" {
		env = append(env, "PATH="+defaultPath)
	}
	if lookupEnv(env, "HOSTNAME") == "" && hostname != "" {
		env = append(env, "HOSTNAME="+hostname)
	}
	if lookupEnv(env, "HOME") == "" {
		env = append(env, "HOME="+user.Home)
	}
	if tty && lookupEnv(env, "TERM") == "" {
		env = append(env, "TERM=xterm")
	}
	return env
}

func lookupEnv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// lookPath finds the command the way a shell inside the container would,
// returning its path inside the container. exec.LookPath cannot be used: it
// would search init's PATH from init's working directory, not the
// container's.
func lookPath(name, cwd, pathEnv string) (string, error) {
	check := func(p string) error {
		host := filepath.Join(root, p)
		// Resolve symlinks within the container: an absolute link target
		// is relative to the container root, not ours.
		fi, err := statIn(host)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return syscall.EACCES
		}
		if fi.Mode()&0o111 == 0 {
			return syscall.EACCES
		}
		return nil
	}
	if strings.Contains(name, "/") {
		p := name
		if !path.IsAbs(p) {
			p = path.Join(cwd, p)
		}
		if err := check(p); err != nil {
			return "", execFailure(name, err)
		}
		return p, nil
	}
	var denied error
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		if !path.IsAbs(dir) {
			dir = path.Join(cwd, dir)
		}
		p := path.Join(dir, name)
		err := check(p)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, syscall.EACCES) && denied == nil {
			denied = err
		}
	}
	if denied != nil {
		return "", execFailure(name, denied)
	}
	return "", fail(guestspec.ExitNotFound, "exec: %q: executable file not found in $PATH", name)
}

// statIn stats a path below root, following symlinks as the container would
// see them.
func statIn(p string) (os.FileInfo, error) {
	for hops := 0; hops < 40; hops++ {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return os.Stat(p)
		}
		target, err := os.Readlink(p)
		if err != nil {
			return nil, err
		}
		if path.IsAbs(target) {
			p = filepath.Join(root, target)
		} else {
			p = filepath.Join(filepath.Dir(p), target)
		}
		if !within(p) {
			p = root // a link climbing out of the root stays at the root, as chroot would
		}
	}
	return nil, syscall.ELOOP
}

func execFailure(name string, err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
		return fail(guestspec.ExitNotFound, "exec: %q: no such file or directory", name)
	default:
		return fail(guestspec.ExitCannotInvoke, "exec: %q: %v", name, err)
	}
}

// cleanUp kills every remaining process and reaps them, so nothing is still
// writing to the console ports when the status goes out.
func cleanUp() {
	syscall.Kill(-1, syscall.SIGKILL)
	if !children.waitIdle(time.Now().Add(time.Second)) {
		logf("some processes did not exit after SIGKILL")
	}
}
