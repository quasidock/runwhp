//go:build windows

package vmm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// ExecDialer opens a connection to init's exec server: through the pipe
// of the runwhp running the container, or, in that runwhp, its vsock.
type ExecDialer func(ctx context.Context) (io.ReadWriteCloser, error)

// ExecStreams are where an exec's streams go on the host. Stdin is read
// only if the exec takes input; Stderr is unused with a terminal.
type ExecStreams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// ExecSession is a process run in a container by init's exec server (see
// guestspec.ExecPort).
type ExecSession struct {
	ctl    io.ReadWriteCloser
	events *json.Decoder
	ctlMu  sync.Mutex

	conns   []io.Closer
	outputs sync.WaitGroup
	detach  bool

	started chan struct{} // closed once the process has started, or the session ended
	done    chan struct{} // closed once the session has ended
	pid     int
	result  guestspec.ExecEvent
}

// StartExec asks init to run ex, attaches its streams, and returns the
// session, whose process init starts once they are attached. A command
// init cannot run returns a session already ended, whose Wait says why.
func StartExec(ctx context.Context, dial ExecDialer, ex *guestspec.Exec, streams ExecStreams) (*ExecSession, error) {
	open := func(hello guestspec.ExecHello) (io.ReadWriteCloser, *bufio.Reader, error) {
		c, err := dial(ctx)
		if err != nil {
			return nil, nil, err
		}
		line, _ := json.Marshal(hello)
		if _, err := c.Write(append(line, '\n')); err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, bufio.NewReader(c), nil
	}
	ctl, r, err := open(guestspec.ExecHello{Exec: ex})
	if err != nil {
		return nil, err
	}
	s := &ExecSession{ctl: ctl, events: json.NewDecoder(r), detach: ex.Detach,
		started: make(chan struct{}), done: make(chan struct{})}
	var ev guestspec.ExecEvent
	if err := s.events.Decode(&ev); err != nil {
		ctl.Close()
		return nil, fmt.Errorf("init did not answer runwhp exec: %w", err)
	}
	if ev.Exited {
		s.end(ev)
		return s, nil
	}

	// The streams, one connection each.
	for _, name := range ev.Streams {
		sc, sr, err := open(guestspec.ExecHello{Attach: ev.ID, Stream: name})
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("attaching %s: %w", name, err)
		}
		s.conns = append(s.conns, sc)
		switch name {
		case guestspec.PortStdout, guestspec.PortStderr:
			out := streams.Stdout
			if name == guestspec.PortStderr {
				out = streams.Stderr
			}
			if out == nil {
				out = io.Discard
			}
			s.outputs.Add(1)
			go func() {
				defer s.outputs.Done()
				io.Copy(out, sr)
			}()
		case guestspec.PortStdin:
			in := streams.Stdin
			if in == nil {
				sc.Close()
				continue
			}
			go func() {
				io.Copy(sc, in)
				sc.Close() // the end of input
			}()
		}
	}
	go s.read()
	return s, nil
}

// read follows the control connection's events until the session ends.
func (s *ExecSession) read() {
	for {
		var ev guestspec.ExecEvent
		if err := s.events.Decode(&ev); err != nil {
			s.end(guestspec.ExecEvent{Exited: true, Exit: guestspec.ExitSignalBase + guestspec.SIGKILL,
				Error: "the container stopped while the command ran"})
			return
		}
		if ev.Started {
			// A detached process's session goes on too, until init says
			// how it ended, for whoever keeps it open to learn that.
			s.pid = ev.Pid
			s.markStarted()
		}
		if ev.Exited {
			s.end(ev)
			return
		}
	}
}

func (s *ExecSession) markStarted() {
	select {
	case <-s.started:
	default:
		close(s.started)
	}
}

func (s *ExecSession) end(ev guestspec.ExecEvent) {
	s.result = ev
	s.markStarted()
	close(s.done)
}

// Send sends init a Control for the process: a signal, or its terminal's
// size.
func (s *ExecSession) Send(m guestspec.Control) error {
	line, _ := json.Marshal(m)
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	_, err := s.ctl.Write(append(line, '\n'))
	return err
}

// Started waits until the process has started, and returns its pid in the
// guest; or the error that kept it from starting.
func (s *ExecSession) Started() (int, error) {
	<-s.started
	select {
	case <-s.done:
		if s.pid == 0 {
			_, err := s.Wait()
			if err == nil {
				err = errors.New("the command ended before it started")
			}
			return 0, err
		}
	default:
	}
	return s.pid, nil
}

// Done is closed once the session has ended.
func (s *ExecSession) Done() <-chan struct{} { return s.done }

// Wait waits for the process to exit, then for the last of its output,
// at most 2 s more, and returns its exit code.
func (s *ExecSession) Wait() (int, error) {
	<-s.done
	outputs := make(chan struct{})
	go func() { s.outputs.Wait(); close(outputs) }()
	select {
	case <-outputs:
	case <-time.After(2 * time.Second):
	}
	if s.result.Error != "" {
		return s.result.Exit, errors.New(s.result.Error)
	}
	return s.result.Exit, nil
}

// Close ends the session's connections; init kills a process whose
// control connection is gone.
func (s *ExecSession) Close() {
	s.ctl.Close()
	for _, c := range s.conns {
		c.Close()
	}
}

// DialGuest connects to init's server on vsock port: ExecPort, while the
// container runs and is not paused, or RootPort, once its root is ready.
func (s *Supervisor) DialGuest(ctx context.Context, port uint32) (GuestConn, error) {
	var h *vmHandles
	switch port {
	case guestspec.ExecPort:
		h = s.running()
	case guestspec.RootPort:
		// The container's files, from the moment its root is ready.
		h = s.withRoot()
	case guestspec.BuildKitPort:
		// BuildKit's daemon, in runwhp's builder alone: no other
		// container's processes are the host's to connect to.
		if _, ok := s.rec.Config.Labels[guestspec.BuilderLabel]; !ok {
			return nil, fmt.Errorf("vsock port %d is not runwhp's", port)
		}
		h = s.running()
	default:
		return nil, fmt.Errorf("vsock port %d is not runwhp's", port)
	}
	if h == nil {
		return nil, ErrNotRunning
	}
	s.mu.Lock()
	paused := s.state.Paused
	s.mu.Unlock()
	if paused && port == guestspec.ExecPort {
		// An exec's process would start frozen.
		id := s.rec.ID
		if t := s.rec.Config.Containerd; t != nil {
			id = t.ID // as containerd's client knows it
		}
		return nil, fmt.Errorf("Container %s is paused, unpause the container before exec", id)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vc, err := h.vsock.Dial(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("connecting to the container's init: %w", err)
	}
	return vc, nil
}

// GuestConn is a connection to one of init's servers.
type GuestConn interface {
	io.ReadWriteCloser
	// CloseWrite ends what the host sends, keeping what init sends.
	CloseWrite() error
}
