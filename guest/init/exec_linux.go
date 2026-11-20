//go:build linux

package main

import (
	"errors"
	"fmt"
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
func runProcess(spec *guestspec.Spec, user passwd.User, ports map[string]*os.File, ctl *controller) (guestspec.Status, error) {
	if len(spec.Args) == 0 {
		return guestspec.Status{}, fail(guestspec.ExitRuntime, "no command specified")
	}

	env := append([]string(nil), spec.Env...)
	if lookupEnv(env, "PATH") == "" {
		env = append(env, "PATH="+defaultPath)
	}
	if lookupEnv(env, "HOSTNAME") == "" && spec.Hostname != "" {
		env = append(env, "HOSTNAME="+spec.Hostname)
	}
	if lookupEnv(env, "HOME") == "" {
		env = append(env, "HOME="+user.Home)
	}
	if spec.TTY && lookupEnv(env, "TERM") == "" {
		env = append(env, "TERM=xterm")
	}

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
	debugf("exec %q as %d:%d in %s (stdin %v, tty %v)", spec.Args, user.UID, user.GID, cwd, spec.Stdin, spec.TTY)
	pid, err := syscall.ForkExec(exe, spec.Args, attr)
	streams.started()
	if err != nil {
		return guestspec.Status{}, execFailure(spec.Args[0], err)
	}
	ctl.start(pid)

	status := reap(pid)
	ctl.exited()
	cleanUp()
	if !streams.wait(2 * time.Second) {
		logf("output still pending 2 s after the processes ended; reporting without it")
	}
	return status, nil
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

// reap waits for the container process, reaping any other children that exit
// first (init inherits orphans).
func reap(pid int) guestspec.Status {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			logf("wait: %v", err)
			return guestspec.Status{Exit: guestspec.ExitRuntime, Error: fmt.Sprintf("waiting for the process: %v", err)}
		}
		if got != pid {
			continue
		}
		switch {
		case ws.Exited():
			debugf("process exited with %d", ws.ExitStatus())
			return guestspec.Status{Exit: ws.ExitStatus()}
		case ws.Signaled():
			debugf("process killed by %v", ws.Signal())
			return guestspec.Status{Exit: guestspec.ExitSignalBase + int(ws.Signal())}
		}
	}
}

// cleanUp kills every remaining process and reaps them, so nothing is still
// writing to the console ports when the status goes out.
func cleanUp() {
	syscall.Kill(-1, syscall.SIGKILL)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.ECHILD {
			return
		}
		if pid == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	logf("some processes did not exit after SIGKILL")
}
