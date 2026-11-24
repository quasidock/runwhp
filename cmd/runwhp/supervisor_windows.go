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
	"github.com/quasidock/runwhp/internal/whp"
)

// killGrace is how long the guest has to end a container after SIGKILL
// before runwhp stops the VM itself.
const killGrace = 5 * time.Second

// supervisor is the runwhp process that runs a container: it holds the
// container's lock for as long as the container has a VM, records its
// state as it changes, and serves other runwhps' requests on the
// container's pipe. The container's process may run more than once, each
// time in a VM of its own, a boot; what belongs to one is in vmBoot, what
// lasts from one to the next here.
type supervisor struct {
	root  string
	store *container.Store
	rec   *container.Container
	lock  *container.Lock
	ln    net.Listener
	serve sync.Once

	mu    sync.Mutex
	state container.State
	cur   *vmBoot // the boot under way, or the last
	// changed is closed, and replaced, as the status changes.
	changed chan struct{}
	// The restart policy, and how far along it the container is: armed
	// once a boot's process has started, after which it applies, and
	// stopping once someone has stopped the container, after which no
	// boot follows; stopCh is closed then too.
	policy   container.RestartPolicy
	backoff  container.Backoff
	restarts int
	armed    bool
	stopping bool
	stopCh   chan struct{}
	// wmu orders the writes of state: each writes it as it is then, so the
	// last write is always of the latest. pending counts writes in flight.
	wmu     sync.Mutex
	pending sync.WaitGroup
	done    chan struct{} // closed once the container has stopped for good, and that is recorded
	conns   sync.WaitGroup
	// files are closed before the container is removed: a file open in
	// its directory would keep that from being renamed away.
	files []io.Closer
	// hub passes the container's output on to attached runwhps, and stdin,
	// if set, is its input, open to them.
	hub   *outputHub
	stdin *attachStdin
}

// vmBoot is one VM of the container's, from booting it until the process
// ends.
type vmBoot struct {
	proxy   *signalProxy  // its control port, and stopping it
	ready   chan struct{} // closed once a container booted to wait is ready
	running chan struct{} // closed once the process has started
	exited  chan struct{} // closed once the process has ended and that is recorded

	// Guarded by the supervisor's mu.
	hardStopped string     // why the supervisor stopped the VM itself, if it did
	console     []string   // the last kernel console lines, if the VM failed
	vm          *vmHandles // what reaches into the VM, once it has one
	exit        container.Reply
	startedAt   time.Time // when the process started
	// killed says a kill or stop reached this boot: its process ended by
	// hand, and is not started again, as Docker counts any kill.
	killed bool
}

// streams has the container's output pass through the hub to attached
// runwhps, and, with openStdin, makes its input theirs: stdinOnce ends it
// with the first of them to end its own. Each boot then reads that input
// through a view of its own (see attachStdin.view).
func (s *supervisor) streams(bio *bootIO, openStdin, stdinOnce bool) {
	bio.stdout = s.hub.wrap(container.Stdout, bio.stdout)
	bio.stderr = s.hub.wrap(container.Stderr, bio.stderr)
	if openStdin {
		s.mu.Lock()
		s.stdin = newAttachStdin(stdinOnce)
		s.mu.Unlock()
	}
}

// supervise takes a container's lock, records that this process is
// booting it, with its output going where output says, and makes its
// pipe, which it serves from the first boot.
func supervise(root string, store *container.Store, rec *container.Container, output string) (*supervisor, error) {
	lock, err := store.Lock(rec)
	if err != nil {
		return nil, err
	}
	s := &supervisor{root: root, store: store, rec: rec, lock: lock, state: rec.State,
		done: make(chan struct{}), hub: newOutputHub(), changed: make(chan struct{}), stopCh: make(chan struct{})}
	if s.ln, err = container.Listen(root, rec.ID); err != nil {
		lock.Close()
		return nil, fmt.Errorf("serving container %s's pipe: %w", rec.Short(), err)
	}
	// A container this process made already records it. One started again
	// keeps how it last ended, which also tells the store it is not a
	// record that was never finished.
	if rec.State.Pid != os.Getpid() || rec.State.Output != output {
		s.update(func(st *container.State) {
			*st = container.State{Status: container.Creating, Pid: os.Getpid(), Output: output,
				FinishedAt: st.FinishedAt, ExitCode: st.ExitCode}
		})
	}
	return s, nil
}

// beginBoot starts a boot of the container, with proxy its control port
// and the way to stop its VM; or returns nil if the container has been
// stopped meanwhile, and is not to be booted again.
func (s *supervisor) beginBoot(proxy *signalProxy) *vmBoot {
	b := &vmBoot{proxy: proxy, ready: make(chan struct{}), running: make(chan struct{}), exited: make(chan struct{})}
	// The console's reader of the control port ends with the boot, rather
	// than wait for messages meant for the next.
	proxy.queue.done = b.exited
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	restarting := s.state.Status == container.Restarting
	s.cur = b
	s.mu.Unlock()
	if restarting {
		// Booting again: creating, as each boot is, with how the last run
		// ended kept.
		s.update(func(st *container.State) { st.Status = container.Creating })
	}
	s.serve.Do(func() { go s.serveConns() })
	go s.clockLoop(b, clockPeriod(proxy.logf))
	return b
}

// current is the boot under way, or the last; nil before the first.
func (s *supervisor) current() *vmBoot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// clockLoop keeps the guest's wall clock with the host's, for as long as
// the boot's process runs: the guest boots with the CMOS stub's whole
// seconds, and nothing moves its clock on while the host sleeps. The time
// goes to init as it opens the control port, every period, and as Windows
// resumes. The first is queued at once: the queue stamps it as the console
// takes it for init, and init waits for it before it starts the process
// (Spec.ClockSync).
func (s *supervisor) clockLoop(b *vmBoot, period time.Duration) {
	if period <= 0 {
		return
	}
	b.proxy.queue.sendTime()
	wake := make(chan struct{}, 1)
	stopResume, err := onResume(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		b.proxy.logf("%v; the guest's clock follows the host's every %v only", err, period)
	}
	defer stopResume()
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-b.exited:
			return
		case <-tick.C:
		case <-wake:
			b.proxy.logf("Windows resumed: setting the guest's clock")
		}
		b.proxy.queue.sendTime()
	}
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
	was := s.state.Status
	f(&s.state)
	if s.state.Status != was {
		close(s.changed)
		s.changed = make(chan struct{})
	}
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

// hooks are what bootContainer tells the supervisor of boot b.
func (s *supervisor) hooks(b *vmBoot, waitStart bool) bootHooks {
	return bootHooks{waitStart: waitStart,
		ready:   func() { s.isReady(b) },
		started: func(pid int) { s.started(b, pid) },
		failed: func(console []string) {
			s.mu.Lock()
			b.console = append([]string(nil), console...)
			s.mu.Unlock()
		},
		attached: func(h *vmHandles) {
			s.mu.Lock()
			b.vm = h
			s.mu.Unlock()
		}}
}

// isReady records that a container booted to wait for its start is ready.
func (s *supervisor) isReady(b *vmBoot) {
	s.update(func(st *container.State) { st.Status = container.Created })
	close(b.ready)
}

// started records that the container process runs, as pid in the guest:
// in the background, since the guest goes on meanwhile, and whoever needs
// it recorded first, a start request say, persists it itself.
func (s *supervisor) started(b *vmBoot, pid int) {
	now := time.Now()
	s.updateLater(func(st *container.State) {
		st.Status, st.GuestPid, st.StartedAt = container.Running, pid, now.UTC()
		b.startedAt, s.armed = now, true
	})
	close(b.running)
}

// endBoot records how boot b's process ended, and answers whoever waits
// for that. It reports whether the restart policy starts the container
// again, and after how long: not if the process never started, nor if it
// ended by hand, by a stop or a kill, or by a signal to a foreground run.
func (s *supervisor) endBoot(b *vmBoot, code int, err error) (restart bool, delay time.Duration) {
	now := time.Now()
	s.change(func(st *container.State) {
		st.GuestPid = 0
		st.FinishedAt, st.ExitCode, st.Error = now.UTC(), code, ""
		switch {
		case b.hardStopped != "":
			st.ExitCode, st.Error = guestspec.ExitSignalBase+guestspec.SIGKILL, b.hardStopped
		case errors.Is(err, context.Canceled):
			st.Error = "stopped by runwhp after a second interrupt"
		case err != nil:
			st.Error = err.Error()
		case code == guestspec.ExitRuntime:
			st.Error = "runwhp failed"
		}
		b.vm = nil
		byHand := s.stopping || b.killed || b.proxy.signaled.Load()
		restart = s.armed && !byHand && s.policy.ShouldRestart(st.ExitCode, s.restarts)
		if !restart {
			st.Status, st.Pid = container.Stopped, 0
			return
		}
		s.restarts++
		st.Status, st.RestartCount = container.Restarting, s.restarts
		var ran time.Duration
		if !b.startedAt.IsZero() {
			ran = now.Sub(b.startedAt)
		}
		delay = s.backoff.Next(ran)
	})
	s.pending.Wait()
	if !s.rec.Config.AutoRemove {
		s.persist()
	}
	rep := s.reply()
	s.mu.Lock()
	b.exit = rep
	s.mu.Unlock()
	close(b.exited)
	return restart, delay
}

// waitRestart waits delay before the next boot, and reports false if the
// container was stopped meanwhile, which it then records.
func (s *supervisor) waitRestart(delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stopCh:
		s.stoppedRestarting()
		return false
	}
}

// stoppedRestarting records that a container waiting to start again was
// stopped instead, with how its last run ended.
func (s *supervisor) stoppedRestarting() {
	s.update(func(st *container.State) {
		if st.Status == container.Restarting {
			st.Status, st.Pid = container.Stopped, 0
		}
	})
}

// stopRestarts says no boot is to follow; mu must be held.
func (s *supervisor) stopRestarts() {
	if !s.stopping {
		s.stopping = true
		close(s.stopCh)
	}
}

// finish ends the supervisor once the container has stopped for good: it
// gives up the container's lock and pipe, and removes the container if it
// was to be removed when it stopped.
func (s *supervisor) finish() {
	autoRemove := s.rec.Config.AutoRemove
	close(s.done)
	if s.stdin != nil {
		s.stdin.close() // for attached runwhps still sending input
	}
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

// reply describes the container as it is now.
func (s *supervisor) reply() container.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := container.Reply{OK: true, Status: s.state.Status}
	if s.state.Status == container.Stopped || s.state.Status == container.Restarting {
		// Until then, the state holds how it ended the time before.
		r.ExitCode = s.state.ExitCode
		if s.state.Error != "" {
			r.Error = s.state.Error
		}
	}
	return r
}

// hardStop stops boot b's VM at once, without the guest's help.
func (s *supervisor) hardStop(b *vmBoot, why string) {
	s.mu.Lock()
	if b.hardStopped == "" {
		b.hardStopped = why
	}
	s.mu.Unlock()
	b.proxy.stop()
}

var errNotRunning = errors.New("container is not running")

// kill sends sig to the container's process, or with all to every process
// in it. Before init listens there is nothing to send it to, so the VM is
// stopped instead. After SIGKILL the guest has killGrace to end the
// container before the VM is stopped.
func (s *supervisor) kill(sig int, all bool) error {
	s.mu.Lock()
	b, status := s.cur, s.state.Status
	switch {
	case status == container.Restarting:
		// Between boots, a kill ends the restarts.
		s.stopRestarts()
		s.mu.Unlock()
		return nil
	case status == container.Stopped || b == nil:
		s.mu.Unlock()
		return errNotRunning
	}
	b.killed = true
	s.mu.Unlock()
	if !b.proxy.listening() {
		why := fmt.Sprintf("killed by %s", guestspec.SignalName(sig))
		if status == container.Creating {
			why += " while it was being created"
		}
		s.hardStop(b, why)
		return nil
	}
	b.proxy.queue.send(guestspec.Control{Signal: sig, All: all})
	if sig == guestspec.SIGKILL {
		time.AfterFunc(killGrace, func() {
			select {
			case <-b.exited:
			default:
				s.hardStop(b, fmt.Sprintf("stopped by runwhp: the guest had not ended it %v after SIGKILL", killGrace))
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
	s.mu.Lock()
	s.stopRestarts()
	s.mu.Unlock()
	if s.kill(sig, false) != nil {
		return
	}
	t := time.AfterFunc(timeout, func() { s.kill(guestspec.SIGKILL, false) })
	<-s.done
	t.Stop()
}

// start starts the process of a container booted to wait for its start,
// and waits until it runs or has failed to.
func (s *supervisor) start() container.Reply {
	b := s.current()
	switch s.status() {
	case container.Creating:
		return container.Reply{Error: "the container is still being created"}
	case container.Stopped:
		return container.Reply{Error: errNotRunning.Error()}
	case container.Restarting:
		return s.reply() // it starts again by itself, as docker start leaves it
	case container.Created:
		b.proxy.queue.send(guestspec.Control{Start: true})
	}
	select {
	case <-b.running:
		s.persist() // so that whoever asked sees it running
		return s.reply()
	case <-b.exited:
		s.mu.Lock()
		r := b.exit
		r.Console = b.console
		s.mu.Unlock()
		r.OK = false
		if r.Error == "" {
			r.Error = fmt.Sprintf("the container exited with %d as it started", r.ExitCode)
		}
		return r
	}
}

// wait waits for the container's process to end, and says how it did. A
// container waiting to start again is waited for through its next run, as
// docker wait does.
func (s *supervisor) wait() container.Reply {
	for {
		s.mu.Lock()
		b, status, changed := s.cur, s.state.Status, s.changed
		s.mu.Unlock()
		if status == container.Restarting {
			select {
			case <-changed:
				continue
			case <-s.done:
				return s.reply()
			}
		}
		if b == nil {
			return s.reply()
		}
		<-b.exited
		s.mu.Lock()
		defer s.mu.Unlock()
		return b.exit
	}
}

// serveConns answers requests on the container's pipe until it closes.
func (s *supervisor) serveConns() {
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
			rep = s.wait()
		case container.OpConnect:
			s.connect(c, r, req)
			return
		case container.OpStats:
			rep = s.stats()
		case container.OpAttach:
			s.attach(c, r, req)
			return
		default:
			rep = container.Reply{Error: fmt.Sprintf("this runwhp does not know the request %q", req.Op)}
		}
	}
	data, _ := json.Marshal(rep)
	c.Write(append(data, '\n'))
}

// running returns the VM of the boot under way, if the container's process
// runs.
func (s *supervisor) running() *vmHandles {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Status != container.Running || s.cur == nil {
		return nil
	}
	return s.cur.vm
}

// stats reports what the running container uses: its VM's processor time
// and memory, this process's memory, which holds the guest's, the bytes
// its devices moved, and what init says of the guest.
func (s *supervisor) stats() container.Reply {
	h := s.running()
	if h == nil {
		return container.Reply{Error: errNotRunning.Error()}
	}
	st := &container.Stats{Time: time.Now(), CPUs: s.rec.Config.CPUs, MemLimit: s.rec.Config.Memory}
	if u, ok := h.probe.Usage(); ok {
		st.CPUTime, st.Committed = u.CPUTime, u.Committed
	}
	if pm, err := whp.ProcessMemory(whp.CurrentProcess); err == nil {
		st.HostCommit, st.HostWorkingSet = pm.Commit, pm.PrivateWorkingSet
		if st.HostWorkingSet == 0 {
			st.HostWorkingSet = pm.WorkingSet
		}
	}
	if h.nic != nil {
		st.NetRx, st.NetTx = h.nic.RxBytes.Load(), h.nic.TxBytes.Load()
	}
	for _, b := range h.blks {
		st.BlockRead += b.BytesRead.Load()
		st.BlockWrite += b.BytesWritten.Load()
	}
	st.Guest = h.guest.get(time.Second)
	return container.Reply{OK: true, Stats: st}
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
	h := s.running()
	if h == nil {
		reply(container.Reply{Error: errNotRunning.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	vc, err := h.vsock.Dial(ctx, req.Port)
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
