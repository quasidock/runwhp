//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/passwd"
)

// execServer serves runwhp exec on guestspec.ExecPort: it runs further
// processes in the container, each with streams of its own over vsock
// connections from the host. Nothing in the container can reach it: the
// kernel has no vsock loopback, so only the host can connect.
type execServer struct {
	spec *guestspec.Spec // the container's

	mu       sync.Mutex
	sessions map[string]*execSession
	next     int
	active   sync.WaitGroup // sessions not yet done reporting
}

// execSession is one runwhp exec.
type execSession struct {
	id      string
	exec    *guestspec.Exec
	ctl     *os.File
	want    []string
	streams map[string]*os.File
	ready   chan struct{} // closed once every stream is attached

	sendMu sync.Mutex
}

// startExecServer listens for runwhp exec, or returns nil if the kernel has
// no vsock.
func startExecServer(spec *guestspec.Spec) *execServer {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guestspec.ExecPort})
		if err == nil {
			err = unix.Listen(fd, 16)
		}
		if err != nil {
			unix.Close(fd)
		}
	}
	if err != nil {
		logf("runwhp exec is not available: vsock: %v", err)
		return nil
	}
	s := &execServer{spec: spec, sessions: map[string]*execSession{}}
	go func() {
		for {
			nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			if err != nil {
				logf("runwhp exec: accept: %v", err)
				return
			}
			go s.handle(os.NewFile(uintptr(nfd), "vsock"))
		}
	}()
	return s
}

// wait waits up to limit for every session to report how its process
// ended. Init calls it once the container's process has ended and
// everything has been killed.
func (s *execServer) wait(limit time.Duration) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.active.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		logf("runwhp exec sessions still reporting %v after the container ended", limit)
	}
}

// readLine reads one line from f a byte at a time, so that nothing after
// it is consumed: on a stream's connection, the stream follows.
func readLine(f *os.File) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64<<10 {
		if _, err := f.Read(b); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
	return nil, errors.New("line too long")
}

func (s *execServer) handle(conn *os.File) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := readLine(conn)
	conn.SetReadDeadline(time.Time{})
	var h guestspec.ExecHello
	if err == nil {
		err = json.Unmarshal(line, &h)
	}
	switch {
	case err != nil:
		debugf("runwhp exec: bad hello: %v", err)
		conn.Close()
	case h.Exec != nil:
		s.run(conn, h.Exec)
	case h.Attach != "":
		s.attach(conn, h.Attach, h.Stream)
	default:
		conn.Close()
	}
}

func (e *execSession) send(ev guestspec.ExecEvent) {
	line, _ := json.Marshal(ev)
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	if _, err := e.ctl.Write(append(line, '\n')); err != nil {
		debugf("runwhp exec %s: %v", e.id, err)
	}
}

func (s *execServer) attach(conn *os.File, id, stream string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.sessions[id]
	wanted := false
	if e != nil {
		for _, w := range e.want {
			wanted = wanted || w == stream
		}
	}
	if !wanted || e.streams[stream] != nil {
		debugf("runwhp exec: unexpected stream %q for session %q", stream, id)
		conn.Close()
		return
	}
	e.streams[stream] = conn
	if len(e.streams) == len(e.want) {
		close(e.ready)
	}
}

// run runs one session: prepares the process, waits for its streams,
// starts it, passes on signals and terminal sizes, and reports how it
// ended.
func (s *execServer) run(ctl *os.File, ex *guestspec.Exec) {
	s.active.Add(1)
	defer s.active.Done()
	defer ctl.Close()
	s.mu.Lock()
	s.next++
	e := &execSession{id: strconv.Itoa(s.next), exec: ex, ctl: ctl, streams: map[string]*os.File{}, ready: make(chan struct{})}
	switch {
	case ex.Detach:
	case ex.TTY:
		e.want = []string{guestspec.PortStdout}
	default:
		e.want = []string{guestspec.PortStdout, guestspec.PortStderr}
	}
	if ex.Stdin && !ex.Detach {
		e.want = append(e.want, guestspec.PortStdin)
	}
	s.sessions[e.id] = e
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, e.id)
		s.mu.Unlock()
		for _, f := range e.streams {
			f.Close()
		}
	}()
	failed := func(err error) {
		code := guestspec.ExitRuntime
		var f *failure
		if errors.As(err, &f) {
			code = f.code
		}
		e.send(guestspec.ExecEvent{Exited: true, Exit: code, Error: err.Error()})
	}

	p, err := s.prepare(ex)
	if err != nil {
		failed(err)
		return
	}
	e.send(guestspec.ExecEvent{ID: e.id, Streams: e.want})
	if len(e.want) > 0 {
		select {
		case <-e.ready:
		case <-time.After(5 * time.Second):
			failed(errors.New("the streams of runwhp exec never all arrived"))
			return
		}
	}

	var streams *stdio
	if ex.Detach {
		streams, err = nullStdio()
	} else {
		streams, err = newStdio(&guestspec.Spec{TTY: ex.TTY, Rows: ex.Rows, Cols: ex.Cols}, e.streams, p.user.UID)
	}
	if err != nil {
		failed(err)
		return
	}
	attr := &syscall.ProcAttr{
		Dir:   p.cwd,
		Env:   p.env,
		Files: streams.fds(),
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: p.user.UID, Gid: p.user.GID, Groups: p.user.Groups},
			Setsid:     true,
			Setctty:    ex.TTY,
			Ctty:       0,
		},
	}
	debugf("runwhp exec %s: %q as %d:%d in %s", e.id, ex.Args, p.user.UID, p.user.GID, p.cwd)
	pid, done, err := children.start(func() (int, error) { return cgroup.forkExec(p.exe, ex.Args, attr) })
	streams.started()
	if err != nil {
		failed(execFailure(ex.Args[0], err))
		return
	}
	e.send(guestspec.ExecEvent{Started: true, Pid: pid})
	if ex.Detach {
		// Whoever started it may still be listening, as the Engine API
		// does to report how it ended; runwhp exec -d has gone already,
		// and the process goes on without it.
		e.send(guestspec.ExecEvent{Exited: true, Exit: exitCode(<-done)})
		return
	}
	go e.control(pid, streams.master)
	code := exitCode(<-done)
	if !streams.wait(2 * time.Second) {
		debugf("runwhp exec %s: output still pending 2 s after the process ended", e.id)
	}
	for _, f := range e.streams {
		f.Close()
	}
	e.send(guestspec.ExecEvent{Exited: true, Exit: code})
}

// control applies the host's Control messages to the session's process
// until the control connection closes.
func (e *execSession) control(pid int, master *os.File) {
	sc := bufio.NewScanner(e.ctl)
	for sc.Scan() {
		var m guestspec.Control
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Signal != 0 {
			target := pid
			if m.Group {
				target = -pid
			}
			syscall.Kill(target, syscall.Signal(m.Signal))
		}
		if m.Rows > 0 && m.Cols > 0 && master != nil {
			setWinsize(master, m.Rows, m.Cols)
		}
	}
}

// execProcess is what a runwhp exec runs, resolved in the container.
type execProcess struct {
	exe, cwd string
	env      []string
	user     passwd.User
}

// prepare resolves an exec's user, environment, working directory and
// command in the container, as it is now.
func (s *execServer) prepare(ex *guestspec.Exec) (*execProcess, error) {
	if len(ex.Args) == 0 {
		return nil, fail(guestspec.ExitRuntime, "no command given")
	}
	who := s.spec.User
	if ex.User != "" {
		who = ex.User
	}
	user, err := resolveUser(&guestspec.Spec{User: who})
	if err != nil {
		return nil, fail(guestspec.ExitCannotInvoke, "%v", err)
	}
	env := processEnv(s.spec.Env, s.spec.Hostname, user, ex.TTY)
	env = append(env, ex.Env...)
	cwd := ex.Cwd
	if cwd == "" {
		cwd = s.spec.Cwd
	}
	if cwd == "" {
		cwd = "/"
	}
	if !path.IsAbs(cwd) {
		return nil, fail(guestspec.ExitCannotInvoke, "working directory %q is not absolute", cwd)
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return nil, fail(guestspec.ExitCannotInvoke, "working directory %s does not exist", cwd)
	}
	exe, err := lookPath(ex.Args[0], cwd, lookupEnv(env, "PATH"))
	if err != nil {
		return nil, err
	}
	return &execProcess{exe: exe, cwd: cwd, env: env, user: user}, nil
}

// nullStdio gives a process /dev/null for all three streams.
func nullStdio() (*stdio, error) {
	devnull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening /dev/null: %w", err)
	}
	return &stdio{child: []*os.File{devnull, devnull, devnull}, closeAfterStart: []*os.File{devnull}}, nil
}
