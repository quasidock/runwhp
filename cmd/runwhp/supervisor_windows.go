//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/virtio"
)

// killGrace is how long the guest has to end a container after SIGKILL
// before runwhp stops the VM itself.
const killGrace = 5 * time.Second

// supervisor is the runwhp process that runs a container: it holds the
// container's lock for as long as the container has a VM, records its
// state as it changes, and serves other runwhps' requests on the
// container's pipe.
type supervisor struct {
	root  string
	store *container.Store
	rec   *container.Container
	lock  *container.Lock
	proxy *signalProxy // the control port, and stopping the VM
	ln    net.Listener

	mu    sync.Mutex
	state container.State
	// wmu orders the writes of state: each writes it as it is then, so the
	// last write is always of the latest. pending counts writes in flight.
	wmu     sync.Mutex
	pending sync.WaitGroup
	ready   chan struct{} // closed once a container booted to wait is ready
	running chan struct{} // closed once the process has started
	stopped chan struct{} // closed once the container has stopped and that is recorded
	// hardStopped says why the supervisor stopped the VM itself, if it did.
	hardStopped string
	console     []string // the last kernel console lines, if the VM failed
	conns       sync.WaitGroup
	// files are closed before the container is removed: a file open in
	// its directory would keep that from being renamed away.
	files []io.Closer
	vsock *virtio.Vsock // the VM's vsock device, once it has one
}

// supervise takes a container's lock, records that this process is
// booting it, with its output going where output says, and serves its
// pipe.
func supervise(root string, store *container.Store, rec *container.Container, output string, proxy *signalProxy) (*supervisor, error) {
	lock, err := store.Lock(rec)
	if err != nil {
		return nil, err
	}
	s := &supervisor{root: root, store: store, rec: rec, lock: lock, proxy: proxy, state: rec.State,
		ready: make(chan struct{}), running: make(chan struct{}), stopped: make(chan struct{})}
	if s.ln, err = container.Listen(root, rec.ID); err != nil {
		lock.Close()
		return nil, fmt.Errorf("serving container %s's pipe: %w", rec.Short(), err)
	}
	// A container this process made already records it.
	if rec.State.Pid != os.Getpid() || rec.State.Output != output {
		s.update(func(st *container.State) {
			*st = container.State{Status: container.Creating, Pid: os.Getpid(), Output: output}
		})
	}
	go s.serve()
	return s, nil
}

// update changes the container's state and records it.
func (s *supervisor) update(f func(*container.State)) {
	s.change(f)
	s.persist()
}

// updateLater changes the container's state at once, and records it in
// the background.
func (s *supervisor) updateLater(f func(*container.State)) {
	s.change(f)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.persist()
	}()
}

func (s *supervisor) change(f func(*container.State)) {
	s.mu.Lock()
	f(&s.state)
	s.mu.Unlock()
}

// persist records the state as it is now.
func (s *supervisor) persist() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	if err := s.lock.WriteState(st); err != nil {
		errorf("recording the state of container %s: %v", s.rec.Short(), err)
	}
}

func (s *supervisor) status() container.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Status
}

// hooks are what bootContainer tells the supervisor.
func (s *supervisor) hooks(waitStart bool) bootHooks {
	return bootHooks{waitStart: waitStart, ready: s.isReady, started: s.started, failed: s.failed,
		vsock: func(v *virtio.Vsock) {
			s.mu.Lock()
			s.vsock = v
			s.mu.Unlock()
		}}
}

// isReady records that a container booted to wait for its start is ready.
func (s *supervisor) isReady() {
	s.update(func(st *container.State) { st.Status = container.Created })
	close(s.ready)
}

// started records that the container process runs, as pid in the guest:
// in the background, since the guest goes on meanwhile, and whoever needs
// it recorded first, a start request say, persists it itself.
func (s *supervisor) started(pid int) {
	s.updateLater(func(st *container.State) {
		st.Status, st.GuestPid, st.StartedAt = container.Running, pid, time.Now().UTC()
	})
	close(s.running)
}

func (s *supervisor) failed(console []string) {
	s.mu.Lock()
	s.console = append([]string(nil), console...)
	s.mu.Unlock()
}

// finish records how the container ended, answers whoever waits for that,
// gives up the container's lock and pipe, and removes the container if it
// was to be removed when it stopped.
func (s *supervisor) finish(code int, err error) {
	s.change(func(st *container.State) {
		st.Status, st.Pid, st.GuestPid = container.Stopped, 0, 0
		st.FinishedAt, st.ExitCode, st.Error = time.Now().UTC(), code, ""
		switch {
		case s.hardStopped != "":
			st.ExitCode, st.Error = guestspec.ExitSignalBase+guestspec.SIGKILL, s.hardStopped
		case errors.Is(err, context.Canceled):
			st.Error = "stopped by runwhp after a second interrupt"
		case err != nil:
			st.Error = err.Error()
		case code == guestspec.ExitRuntime:
			st.Error = "runwhp failed"
		}
	})
	autoRemove := s.rec.Config.AutoRemove
	s.pending.Wait()
	if !autoRemove {
		s.persist()
	}
	close(s.stopped)
	s.ln.Close()
	// Let the answers to stop and wait requests go out.
	done := make(chan struct{})
	go func() { s.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if !autoRemove {
		s.lock.Close()
		return
	}
	// A container to be removed is, with no last state recorded, unless
	// it cannot be.
	s.lock.Close()
	for _, f := range s.files {
		f.Close()
	}
	if err := s.store.Remove(s.rec); err != nil {
		errorf("removing container %s: %v", s.rec.Short(), err)
		if s.lock, err = s.store.Lock(s.rec); err == nil {
			s.persist()
			s.lock.Close()
		}
	}
}

// exitCode is the container's exit code, once it has stopped.
func (s *supervisor) reply() container.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := container.Reply{OK: true, Status: s.state.Status, ExitCode: s.state.ExitCode}
	if s.state.Status == container.Stopped && s.state.Error != "" {
		r.Error = s.state.Error
	}
	return r
}

// hardStop stops the VM at once, without the guest's help.
func (s *supervisor) hardStop(why string) {
	s.mu.Lock()
	if s.hardStopped == "" {
		s.hardStopped = why
	}
	s.mu.Unlock()
	s.proxy.stop()
}

var errNotRunning = errors.New("container is not running")

// kill sends sig to the container's process, or with all to every process
// in it. Before init listens there is nothing to send it to, so the VM is
// stopped instead. After SIGKILL the guest has killGrace to end the
// container before the VM is stopped.
func (s *supervisor) kill(sig int, all bool) error {
	switch s.status() {
	case container.Stopped:
		return errNotRunning
	case container.Creating:
		if !s.proxy.listening() {
			s.hardStop(fmt.Sprintf("killed by %s while it was being created", guestspec.SignalName(sig)))
			return nil
		}
	}
	if !s.proxy.listening() {
		s.hardStop(fmt.Sprintf("killed by %s", guestspec.SignalName(sig)))
		return nil
	}
	s.proxy.queue.send(guestspec.Control{Signal: sig, All: all})
	if sig == guestspec.SIGKILL {
		time.AfterFunc(killGrace, func() {
			select {
			case <-s.stopped:
			default:
				s.hardStop(fmt.Sprintf("stopped by runwhp: the guest had not ended it %v after SIGKILL", killGrace))
			}
		})
	}
	return nil
}

// stop sends the container's stop signal, then SIGKILL after timeout, and
// waits for it to stop.
func (s *supervisor) stop(timeout time.Duration) {
	sig := guestspec.SIGTERM
	if n, err := guestspec.ParseSignal(s.rec.Config.StopSignal); err == nil && s.rec.Config.StopSignal != "" {
		sig = n
	}
	if s.kill(sig, false) != nil {
		return
	}
	t := time.AfterFunc(timeout, func() { s.kill(guestspec.SIGKILL, false) })
	<-s.stopped
	t.Stop()
}

// start starts the process of a container booted to wait for its start,
// and waits until it runs or has failed to.
func (s *supervisor) start() container.Reply {
	switch s.status() {
	case container.Creating:
		return container.Reply{Error: "the container is still being created"}
	case container.Stopped:
		return container.Reply{Error: errNotRunning.Error()}
	case container.Created:
		s.proxy.queue.send(guestspec.Control{Start: true})
	}
	select {
	case <-s.running:
		s.persist() // so that whoever asked sees it running
		return s.reply()
	case <-s.stopped:
		r := s.reply()
		r.OK = false
		if r.Error == "" {
			r.Error = fmt.Sprintf("the container exited with %d as it started", r.ExitCode)
		}
		s.mu.Lock()
		r.Console = s.console
		s.mu.Unlock()
		return r
	}
}

// serve answers requests on the container's pipe until it closes.
func (s *supervisor) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)
		go func() {
			defer s.conns.Done()
			s.handle(c)
		}()
	}
}

func (s *supervisor) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req container.Request
	rep := container.Reply{OK: true}
	if err := json.Unmarshal(line, &req); err != nil {
		rep = container.Reply{Error: "bad request: " + err.Error()}
	} else {
		switch req.Op {
		case container.OpStart:
			rep = s.start()
		case container.OpKill:
			if err := s.kill(req.Signal, req.All); err != nil {
				rep = container.Reply{Error: err.Error()}
			}
		case container.OpStop:
			s.stop(time.Duration(req.Timeout * float64(time.Second)))
			rep = s.reply()
		case container.OpWait:
			<-s.stopped
			rep = s.reply()
		case container.OpConnect:
			s.connect(c, r, req)
			return
		default:
			rep = container.Reply{Error: fmt.Sprintf("this runwhp does not know the request %q", req.Op)}
		}
	}
	data, _ := json.Marshal(rep)
	c.Write(append(data, '\n'))
}

// connect connects c to a vsock port in the guest, and carries bytes both
// ways until the guest closes the connection. What c sent beyond its
// request waits in r. The guest's end of file closes c; c's end of file
// shuts down the guest's receiving side, so a stream can end in one
// direction while the other goes on.
func (s *supervisor) connect(c net.Conn, r *bufio.Reader, req container.Request) {
	reply := func(rep container.Reply) {
		data, _ := json.Marshal(rep)
		c.Write(append(data, '\n'))
	}
	if req.Port != guestspec.ExecPort {
		reply(container.Reply{Error: fmt.Sprintf("vsock port %d is not runwhp exec's", req.Port)})
		return
	}
	s.mu.Lock()
	vs, status := s.vsock, s.state.Status
	s.mu.Unlock()
	if status != container.Running || vs == nil {
		reply(container.Reply{Error: errNotRunning.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	vc, err := vs.Dial(ctx, req.Port)
	cancel()
	if err != nil {
		reply(container.Reply{Error: fmt.Sprintf("connecting to the container's init: %v", err)})
		return
	}
	defer vc.Close()
	reply(container.Reply{OK: true})
	go func() {
		io.Copy(vc, r)
		vc.CloseWrite()
	}()
	io.Copy(c, vc)
}
