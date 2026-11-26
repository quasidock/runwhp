//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	dcontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/vmm"
)

func (s *apiServer) execRoutes() {
	s.handle("POST /containers/{id}/exec", s.postExecCreate)
	s.handle("POST /exec/{id}/start", s.postExecStart)
	s.handle("POST /exec/{id}/resize", s.postExecResize)
	s.handle("GET /exec/{id}/json", s.getExec)
}

// execRecord is an exec the server was asked to make: Docker makes an
// exec first, then starts it, and is asked afterwards how it ended. An
// exec lives as long as the server, which holds its session.
type execRecord struct {
	id, containerID string
	ex              guestspec.Exec
	attachStdout    bool
	attachStderr    bool
	detachKeys      string

	mu       sync.Mutex
	started  bool
	running  bool
	pid      int
	exitCode *int
	ended    time.Time
	sess     *vmm.ExecSession
}

// execTable holds the server's execs, dropping each some minutes after it
// ended, or with its container.
type execTable struct {
	mu sync.Mutex
	m  map[string]*execRecord
}

func newExecTable() *execTable { return &execTable{m: map[string]*execRecord{}} }

const execKeep = 5 * time.Minute

func (t *execTable) add(e *execRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, old := range t.m {
		old.mu.Lock()
		stale := !old.ended.IsZero() && now.Sub(old.ended) > execKeep
		old.mu.Unlock()
		if stale {
			delete(t.m, id)
		}
	}
	t.m[e.id] = e
}

func (t *execTable) get(id string) (*execRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[id]
	return e, ok
}

func (t *execTable) ofContainer(id string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for _, e := range t.m {
		if e.containerID == id {
			ids = append(ids, e.id)
		}
	}
	return ids
}

func (t *execTable) dropContainer(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for eid, e := range t.m {
		if e.containerID == id {
			delete(t.m, eid)
		}
	}
}

// closeAll ends every exec still running, as the server stops.
func (t *execTable) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.m {
		e.mu.Lock()
		if e.sess != nil {
			e.sess.Close()
		}
		e.mu.Unlock()
	}
}

func (s *apiServer) execIDsOf(containerID string) []string {
	if s.execs == nil {
		return nil
	}
	return s.execs.ofContainer(containerID)
}

func newExecID() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *apiServer) postExecCreate(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("id")
	c, err := s.resolve(ref)
	if err != nil {
		return err
	}
	var req dcontainer.ExecCreateRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if len(req.Cmd) == 0 {
		return badRequest("No exec command specified")
	}
	if req.Privileged {
		return badRequest("--privileged is not supported")
	}
	switch {
	case c.State.Status == container.Restarting:
		return conflict("Container %s is restarting, wait until the container is running", c.ID)
	case c.State.Status != container.Running:
		return conflict("container %s is not running", c.ID)
	case c.State.Paused:
		return conflict("Container %s is paused, unpause the container before exec", c.ID)
	}
	if req.DetachKeys != "" {
		if _, err := parseDetachKeys(req.DetachKeys); err != nil {
			return badRequest("%v", err)
		}
	}
	e := &execRecord{
		id: newExecID(), containerID: c.ID,
		ex:           guestspec.Exec{Args: req.Cmd, Cwd: req.WorkingDir, User: req.User, Stdin: req.AttachStdin, TTY: req.Tty},
		attachStdout: req.AttachStdout, attachStderr: req.AttachStderr, detachKeys: req.DetachKeys,
	}
	for _, kv := range req.Env {
		// A bare KEY is the client's to resolve.
		if containsByte(kv, '=') {
			e.ex.Env = append(e.ex.Env, kv)
		}
	}
	if cs := req.ConsoleSize; cs != nil && cs[0] > 0 && cs[1] > 0 {
		e.ex.Rows, e.ex.Cols = uint16(cs[0]), uint16(cs[1])
	}
	s.execs.add(e)
	s.emitContainer(c, events.ActionExecCreate, map[string]string{"execID": e.id, "exec_command": joinArgs(req.Cmd)})
	writeJSONStatus(w, http.StatusCreated, dcontainer.ExecCreateResponse{ID: e.id})
	return nil
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func (s *apiServer) execByID(id string) (*execRecord, error) {
	e, ok := s.execs.get(id)
	if !ok {
		return nil, notFound("No such exec instance: %s", id)
	}
	return e, nil
}

func (s *apiServer) postExecStart(w http.ResponseWriter, r *http.Request) error {
	e, err := s.execByID(r.PathValue("id"))
	if err != nil {
		return err
	}
	var req dcontainer.ExecStartRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return conflict("Error: Exec command %s has already run", e.id)
	}
	e.started = true
	e.mu.Unlock()
	c, err := s.resolve(e.containerID)
	if err != nil {
		return err
	}
	ex := e.ex
	ex.Detach = req.Detach
	if req.Detach {
		ex.Stdin = false
	}
	if cs := req.ConsoleSize; cs != nil && cs[0] > 0 && cs[1] > 0 {
		ex.Rows, ex.Cols = uint16(cs[0]), uint16(cs[1])
	}
	if ex.TTY && ex.Rows == 0 {
		ex.Rows, ex.Cols = vmm.DetachedRows, vmm.DetachedCols
	}

	if req.Detach {
		sess, err := vmm.StartExec(context.Background(), dialExec(s.root, c.ID), &ex, vmm.ExecStreams{})
		if err != nil {
			return s.execFailed(e, err)
		}
		pid, _ := sess.Started()
		e.mu.Lock()
		e.sess, e.running, e.pid = sess, true, pid
		e.mu.Unlock()
		s.emitContainer(c, events.ActionExecStart, map[string]string{"execID": e.id, "exec_command": joinArgs(ex.Args)})
		// The session stays open to learn how the process ends, which its
		// inspect then says; closing it, as the server stops, leaves the
		// process running.
		go func() {
			select {
			case <-sess.Done():
			case <-s.ctx.Done():
				sess.Close()
				return
			}
			code, _ := sess.Wait()
			sess.Close()
			s.execEnded(c, e, code)
		}()
		w.WriteHeader(http.StatusOK)
		return nil
	}

	ctype := dockerapiMultiplexed
	if ex.TTY {
		ctype = dockerapiRaw
	}
	conn, brw, err := hijack(w, r, ctype)
	if err != nil {
		return nil
	}
	defer conn.Close()
	out := &lockedWriter{w: conn}
	streams := vmm.ExecStreams{Stdout: io.Discard, Stderr: io.Discard}
	if e.attachStdout {
		streams.Stdout = streamWriter(out, container.Stdout, ex.TTY)
	}
	if e.attachStderr {
		streams.Stderr = streamWriter(out, container.Stderr, ex.TTY)
	}
	detached := make(chan struct{})
	if ex.Stdin {
		var in io.Reader = brw.Reader
		if ex.TTY {
			keys, _ := parseDetachKeys(defaultDetachKeys)
			if e.detachKeys != "" {
				keys, _ = parseDetachKeys(e.detachKeys)
			}
			f := &detachFilter{r: in, keys: keys}
			in = readerFunc(func(p []byte) (int, error) {
				n, err := f.Read(p)
				if errors.Is(err, errDetached) {
					close(detached)
					return n, io.EOF
				}
				return n, err
			})
		}
		streams.Stdin = in
	}
	sess, err := vmm.StartExec(s.ctx, dialExec(s.root, c.ID), &ex, streams)
	if err != nil {
		// The client already has the stream: what failed goes there.
		msg := []byte("runwhp: " + err.Error() + "\n")
		if ex.TTY {
			conn.Write(msg)
		} else {
			conn.Write(container.AppendFrame(nil, container.Stderr, msg))
		}
		s.execFailed(e, err)
		return nil
	}
	pid, _ := sess.Started()
	e.mu.Lock()
	e.sess, e.running, e.pid = sess, true, pid
	e.mu.Unlock()
	s.emitContainer(c, events.ActionExecStart, map[string]string{"execID": e.id, "exec_command": joinArgs(ex.Args)})
	select {
	case <-sess.Done():
	case <-detached:
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
		return nil
	case <-s.ctx.Done():
	}
	code, werr := sess.Wait()
	sess.Close()
	if werr != nil {
		msg := []byte("runwhp: " + werr.Error() + "\n")
		if ex.TTY {
			conn.Write(msg)
		} else {
			conn.Write(container.AppendFrame(nil, container.Stderr, msg))
		}
	}
	s.execEnded(c, e, code)
	return nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func (s *apiServer) execFailed(e *execRecord, err error) error {
	code := guestspec.ExitRuntime
	if errors.Is(err, container.ErrNotRunning) {
		code = guestspec.ExitRuntime
	}
	e.mu.Lock()
	e.exitCode, e.running, e.ended = &code, false, time.Now()
	e.mu.Unlock()
	return err
}

func (s *apiServer) execEnded(c *container.Container, e *execRecord, code int) {
	e.mu.Lock()
	e.exitCode, e.running, e.ended, e.sess = &code, false, time.Now(), nil
	e.mu.Unlock()
	s.emitContainer(c, events.ActionExecDie, map[string]string{"execID": e.id, "exitCode": itoa(code)})
}

func (s *apiServer) postExecResize(w http.ResponseWriter, r *http.Request) error {
	e, err := s.execByID(r.PathValue("id"))
	if err != nil {
		return err
	}
	h, err1 := intParam(r, "h", 0)
	wd, err2 := intParam(r, "w", 0)
	if err1 != nil || err2 != nil || h <= 0 || wd <= 0 || h > 0xffff || wd > 0xffff {
		return badRequest("invalid terminal size")
	}
	e.mu.Lock()
	sess := e.sess
	e.mu.Unlock()
	if sess == nil {
		return conflict("exec %s is not running", e.id)
	}
	if err := sess.Send(guestspec.Control{Rows: uint16(h), Cols: uint16(wd)}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *apiServer) getExec(w http.ResponseWriter, r *http.Request) error {
	e, err := s.execByID(r.PathValue("id"))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	resp := dcontainer.ExecInspectResponse{
		ID: e.id, Running: e.running, ExitCode: e.exitCode, ContainerID: e.containerID, Pid: e.pid,
		OpenStdin: e.ex.Stdin, OpenStdout: e.attachStdout, OpenStderr: e.attachStderr, CanRemove: !e.running && e.started,
		ProcessConfig: &dcontainer.ExecProcessConfig{Tty: e.ex.TTY, Entrypoint: e.ex.Args[0], Arguments: e.ex.Args[1:], User: e.ex.User},
	}
	if resp.ProcessConfig.Arguments == nil {
		resp.ProcessConfig.Arguments = []string{}
	}
	if e.detachKeys != "" {
		resp.DetachKeys, _ = parseDetachKeys(e.detachKeys)
	}
	writeJSON(w, resp)
	return nil
}
