//go:build windows

package vmm

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

// KillGrace is how long the guest has to end a container after SIGKILL
// before runwhp stops the VM itself.
const KillGrace = 5 * time.Second

// Supervisor is the runwhp process that runs a container: it holds the
// container's lock for as long as the container has a VM, records its
// state as it changes, and serves other runwhps' requests on the
// container's pipe. The container's process may run more than once, each
// time in a VM of its own, a boot; what belongs to one is in vmBoot, what
// lasts from one to the next here.
type Supervisor struct {
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
	// pauseMu orders pauses, unpauses and the thaws after kills.
	pauseMu sync.Mutex
}

// vmBoot is one VM of the container's, from booting it until the process
// ends.
type vmBoot struct {
	proxy   *SignalProxy  // its control port, and stopping it
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
	// reprepare says the boot, of a created container, is stopped to boot
	// again with a configuration changed meanwhile, as network connect
	// changes it: its process never ran, and the next boot waits to start
	// in its place. Once it is set, b.exited means that, not an end.
	reprepare bool
}

// streams has the container's output pass through the hub to attached
// runwhps, and, with openStdin, makes its input theirs: stdinOnce ends it
// with the first of them to end its own. Each boot then reads that input
// through a view of its own (see attachStdin.view).
func (s *Supervisor) streams(bio *BootIO, openStdin, stdinOnce bool) {
	bio.Stdout = s.hub.wrap(container.Stdout, bio.Stdout)
	bio.Stderr = s.hub.wrap(container.Stderr, bio.Stderr)
	if openStdin {
		s.mu.Lock()
		s.stdin = newAttachStdin(stdinOnce)
		s.mu.Unlock()
	}
}

// Supervise takes a container's lock, records that this process is
// booting it, with its output going where output says, and makes its
// pipe, which it serves from the first boot.
func Supervise(root string, store *container.Store, rec *container.Container, output string) (*Supervisor, error) {
	lock, err := store.Lock(rec)
	if err != nil {
		return nil, err
	}
	return superviseLocked(root, store, rec, output, lock)
}

// Adopt takes over a container from the runwhp that started this one,
// with the lock it handed over, to start it again: it records that this
// process holds it, waiting to, and serves its pipe at once.
func Adopt(root string, store *container.Store, rec *container.Container, lock *container.Lock, restarts int) (*Supervisor, error) {
	s, err := superviseLocked(root, store, rec, container.OutputLog, lock)
	if err != nil {
		return nil, err
	}
	s.restarts, s.armed = restarts, true
	s.update(func(st *container.State) {
		st.Status, st.Pid, st.GuestPid, st.Output, st.RestartCount = container.Restarting, os.Getpid(), 0, container.OutputLog, restarts
	})
	s.serve.Do(func() { go s.serveConns() })
	return s, nil
}

func superviseLocked(root string, store *container.Store, rec *container.Container, output string, lock *container.Lock) (*Supervisor, error) {
	var err error
	s := &Supervisor{root: root, store: store, rec: rec, lock: lock, state: rec.State,
		done: make(chan struct{}), hub: newOutputHub(), changed: make(chan struct{}), stopCh: make(chan struct{})}
	if s.ln, err = container.Listen(root, rec.ID); err != nil {
		lock.Close()
		return nil, fmt.Errorf("serving container %s's pipe: %w", rec.Short(), err)
	}
	// A container this process made already records it, and one handed
	// over is recorded by adopt. One started again keeps how it last
	// ended, which also tells the store it is not a record that was never
	// finished.
	if rec.State.Status != container.Restarting && (rec.State.Pid != os.Getpid() || rec.State.Output != output) {
		s.update(func(st *container.State) {
			health := st.Health
			if health != nil {
				// Starting again, with the last results kept, as Docker's.
				health = container.StartHealth(health)
			}
			*st = container.State{Status: container.Creating, Pid: os.Getpid(), Output: output,
				FinishedAt: st.FinishedAt, ExitCode: st.ExitCode, Health: health}
		})
	}
	return s, nil
}

// beginBoot starts a boot of the container, with proxy its control port
// and the way to stop its VM; or returns nil if the container has been
// stopped meanwhile, and is not to be booted again.
func (s *Supervisor) beginBoot(proxy *SignalProxy) *vmBoot {
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
	s.state.Paused = false
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
func (s *Supervisor) current() *vmBoot {
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
func (s *Supervisor) clockLoop(b *vmBoot, period time.Duration) {
	keepClock(b.proxy.queue, b.exited, period, b.proxy.logf)
}

// keepClock sends the guest the host's time through queue at once, every
// period and as Windows resumes, until done is closed.
func keepClock(queue *controlQueue, done <-chan struct{}, period time.Duration, logf func(string, ...any)) {
	if period <= 0 {
		return
	}
	queue.sendTime()
	wake := make(chan struct{}, 1)
	stopResume, err := onResume(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		logf("%v; the guest's clock follows the host's every %v only", err, period)
	}
	defer stopResume()
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		case <-wake:
			logf("Windows resumed: setting the guest's clock")
		}
		queue.sendTime()
	}
}

// update changes the container's state and records it.
func (s *Supervisor) update(f func(*container.State)) {
	s.change(f)
	s.persist()
}

// updateLater changes the container's state at once, and records it in
// the background.
func (s *Supervisor) updateLater(f func(*container.State)) {
	s.change(f)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.persist()
	}()
}

func (s *Supervisor) change(f func(*container.State)) {
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
func (s *Supervisor) persist() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	if err := s.lock.WriteState(st); err != nil {
		errorf("recording the state of container %s: %v", s.rec.Short(), err)
	}
}

func (s *Supervisor) status() container.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Status
}

// hooks are what bootContainer tells the supervisor of boot b.
func (s *Supervisor) hooks(b *vmBoot, waitStart bool) bootHooks {
	return bootHooks{waitStart: waitStart,
		ready:   func() { s.isReady(b) },
		started: func(pid int) { s.started(b, pid) },
		failed: func(console []string) {
			s.mu.Lock()
			b.console = append([]string(nil), console...)
			s.mu.Unlock()
		},
		attached: func(h *vmHandles) {
			// The ports as bound go in the state, which is recorded as the
			// guest becomes ready or starts its process.
			ports := h.network.published()
			s.change(func(st *container.State) { st.Ports, st.DirectVolumes = ports, h.directVolumes })
			s.mu.Lock()
			b.vm = h
			s.mu.Unlock()
			s.hub.hold()
		},
		// Once the guest has stopped, its last output is not to wait for
		// attached runwhps: the VM drains it, and goes, at once.
		stopped: s.hub.release,
	}
}

// isReady records that a container booted to wait for its start is ready.
func (s *Supervisor) isReady(b *vmBoot) {
	s.update(func(st *container.State) { st.Status = container.Created })
	close(b.ready)
}

// started records that the container process runs, as pid in the guest:
// in the background, since the guest goes on meanwhile, and whoever needs
// it recorded first, a start request say, persists it itself.
func (s *Supervisor) started(b *vmBoot, pid int) {
	now := time.Now()
	s.updateLater(func(st *container.State) {
		st.Status, st.GuestPid, st.StartedAt = container.Running, pid, now.UTC()
		b.startedAt, s.armed = now, true
	})
	close(b.running)
	if hc := s.rec.Config.Healthcheck; hc.Enabled() {
		go s.healthLoop(b, hc, s.execProbe)
	}
}

// endBoot records how boot b's process ended, and answers whoever waits
// for that. It reports whether the restart policy starts the container
// again, and after how long: not if the process never started, nor if it
// ended by hand, by a stop or a kill, or by a signal to a foreground run.
func (s *Supervisor) endBoot(b *vmBoot, code int, err error) (restart bool, delay time.Duration) {
	now := time.Now()
	s.change(func(st *container.State) {
		st.GuestPid = 0
		st.Paused = false
		st.Ports, st.DirectVolumes = nil, nil
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
func (s *Supervisor) waitRestart(delay time.Duration) bool {
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
func (s *Supervisor) stoppedRestarting() {
	s.update(func(st *container.State) {
		if st.Status == container.Restarting {
			st.Status, st.Pid = container.Stopped, 0
		}
	})
}

// HandOffFunc starts a runwhp in the background to take container rec over
// from this one, with lock, and start it again after delay, the restarts'th
// time; it returns once that runwhp holds the container.
type HandOffFunc func(ctx context.Context, root string, rec *container.Container, lock *container.Lock, delay time.Duration, restarts int) (container.Reply, error)

// handOff hands the container, waiting to start again after delay, to a
// runwhp in the background that spawn starts, with its lock, so that it
// outlives this one, as a foreground run's container does under Docker's
// daemon. Between the two, no runwhp serves its pipe, and clients wait
// while its lock is held.
func (s *Supervisor) handOff(ctx context.Context, delay time.Duration, spawn HandOffFunc) error {
	s.ln.Close()
	done := make(chan struct{})
	go func() { s.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	stopped, restarts := s.stopping, s.restarts
	s.mu.Unlock()
	if stopped {
		return errors.New("it was stopped")
	}
	rep, err := spawn(ctx, s.root, s.rec, s.lock, delay, restarts)
	if err == nil && !rep.OK {
		err = errors.New(rep.Error)
	}
	if err != nil {
		return err
	}
	s.lock.Close()
	return nil
}

// stopRestarts says no boot is to follow; mu must be held.
func (s *Supervisor) stopRestarts() {
	if !s.stopping {
		s.stopping = true
		close(s.stopCh)
	}
}

// finish ends the supervisor once the container has stopped for good: it
// gives up the container's lock and pipe, and removes the container if it
// was to be removed when it stopped.
func (s *Supervisor) finish() {
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
func (s *Supervisor) reply() container.Reply {
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
func (s *Supervisor) hardStop(b *vmBoot, why string) {
	s.mu.Lock()
	if b.hardStopped == "" {
		b.hardStopped = why
	}
	s.mu.Unlock()
	b.proxy.stop()
}

var ErrNotRunning = errors.New("container is not running")

// Kill sends sig to the container's process, or with all to every process
// in it. Before init listens there is nothing to send it to, so the VM is
// stopped instead. After SIGKILL the guest has killGrace to end the
// container before the VM is stopped.
func (s *Supervisor) Kill(sig int, all bool) error {
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
		return ErrNotRunning
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
	s.thawAfterSignal(b)
	if sig == guestspec.SIGKILL {
		time.AfterFunc(KillGrace, func() {
			select {
			case <-b.exited:
			default:
				s.hardStop(b, fmt.Sprintf("stopped by runwhp: the guest had not ended it %v after SIGKILL", KillGrace))
			}
		})
	}
	return nil
}

// stop sends signal sig, or the container's stop signal if sig is 0, then
// SIGKILL after timeout, and waits for it to stop.
func (s *Supervisor) stop(sig int, timeout time.Duration) {
	if sig <= 0 || sig > 64 {
		sig = guestspec.SIGTERM
		if n, err := guestspec.ParseSignal(s.rec.Config.StopSignal); err == nil && s.rec.Config.StopSignal != "" {
			sig = n
		}
	}
	s.mu.Lock()
	s.stopRestarts()
	s.mu.Unlock()
	if s.Kill(sig, false) != nil {
		return
	}
	t := time.AfterFunc(timeout, func() { s.Kill(guestspec.SIGKILL, false) })
	<-s.done
	t.Stop()
}

// Start starts the process of a container booted to wait for its Start,
// and waits until it runs or has failed to. A container still being
// created, or booted again for a network it was connected to, is waited
// for.
func (s *Supervisor) Start() container.Reply {
	for {
		s.mu.Lock()
		status, changed := s.state.Status, s.changed
		s.mu.Unlock()
		if status != container.Creating {
			break
		}
		select {
		case <-changed:
		case <-s.done:
			return container.Reply{Error: ErrNotRunning.Error()}
		case <-time.After(time.Minute):
			return container.Reply{Error: "the container is still being created"}
		}
	}
	b := s.current()
	switch s.status() {
	case container.Creating:
		return container.Reply{Error: "the container is still being created"}
	case container.Stopped:
		return container.Reply{Error: ErrNotRunning.Error()}
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

// Wait waits for the container's process to end, and says how it did. A
// container waiting to start again is waited for through its next run, as
// docker Wait does.
func (s *Supervisor) Wait() container.Reply {
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
		if b.reprepare {
			s.mu.Unlock()
			continue // the next boot takes its place
		}
		defer s.mu.Unlock()
		return b.exit
	}
}

// Networks connects the container to a network, or disconnects it, as req
// says, unless it runs. The change is recorded, and goes for the next
// boot: a container waiting to start again starts so, and a created one is
// booted again at once, ready to start, before Networks returns.
func (s *Supervisor) Networks(req container.Request) container.Reply {
	s.mu.Lock()
	status, b := s.state.Status, s.cur
	if status == container.Running {
		s.mu.Unlock()
		return container.Reply{Error: container.ErrNetworksRunning.Error()}
	}
	if status == container.Creating || status == container.Stopped || b == nil && status != container.Restarting {
		s.mu.Unlock()
		return container.Reply{Error: "the container is being created or stopped; try again"}
	}
	var cfg *container.Config
	var err error
	if req.Connect != nil {
		cfg, err = s.rec.Config.Connect(*req.Connect)
	} else {
		cfg, err = s.rec.Config.Disconnect(req.Disconnect, req.Disconnect)
	}
	if err == nil {
		err = s.lock.WriteConfig(cfg)
	}
	if err != nil {
		s.mu.Unlock()
		return container.Reply{Error: err.Error()}
	}
	s.rec.Config = cfg
	if status != container.Created {
		s.mu.Unlock()
		return s.reply()
	}
	return s.bootAgain(b)
}

// Reprepare boots a created container's VM again, ready to start, as
// OpReprepare asks: so that a volume it had to itself it now reaches
// through the volume's server, since another container needs the volume
// too.
func (s *Supervisor) Reprepare() container.Reply {
	s.mu.Lock()
	status, b := s.state.Status, s.cur
	if status != container.Created || b == nil {
		s.mu.Unlock()
		if status == container.Creating {
			return container.Reply{Error: "the container is still being created; try again"}
		}
		return container.Reply{Error: fmt.Sprintf("the container is %s, not created", status)}
	}
	return s.bootAgain(b)
}

// bootAgain stops created boot b, whose VM waits to start its process, and
// waits until the loop has booted it again and it is ready to start. s.mu
// is held, and released.
func (s *Supervisor) bootAgain(b *vmBoot) container.Reply {
	b.reprepare = true
	s.mu.Unlock()
	// The VM waits to start its process, which has not run: stop it, and
	// the loop boots it again as it is now configured.
	b.proxy.stop()
	for {
		s.mu.Lock()
		status, changed, cur := s.state.Status, s.changed, s.cur
		s.mu.Unlock()
		if cur != b && status != container.Creating {
			return s.reply()
		}
		select {
		case <-changed:
		case <-s.done:
			return s.reply()
		}
	}
}

// reprepared reports whether boot b ended to boot again, as Networks asked,
// and if so records that: the container is being created again.
func (s *Supervisor) reprepared(b *vmBoot) bool {
	s.mu.Lock()
	again := b.reprepare && !s.stopping
	s.mu.Unlock()
	if !again {
		return false
	}
	s.change(func(st *container.State) {
		st.Status, st.GuestPid = container.Creating, 0
		st.Ports, st.DirectVolumes = nil, nil
		b.vm = nil
	})
	close(b.exited)
	return true
}

// serveConns answers requests on the container's pipe until it closes.
func (s *Supervisor) serveConns() {
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

func (s *Supervisor) handle(c net.Conn) {
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
			rep = s.Start()
		case container.OpKill:
			if err := s.Kill(req.Signal, req.All); err != nil {
				rep = container.Reply{Error: err.Error()}
			}
		case container.OpStop:
			s.stop(req.Signal, time.Duration(req.Timeout*float64(time.Second)))
			rep = s.reply()
		case container.OpWait:
			rep = s.Wait()
		case container.OpConnect:
			s.connect(c, r, req)
			return
		case container.OpStats:
			rep = s.stats()
		case container.OpTop:
			rep = s.top()
		case container.OpAttach:
			s.attach(c, r, req)
			return
		case container.OpPause:
			rep = s.Pause()
		case container.OpUnpause:
			rep = s.Unpause()
		case container.OpNetworks:
			rep = s.Networks(req)
		case container.OpReprepare:
			rep = s.Reprepare()
		case container.OpControl:
			switch {
			case req.Control == nil:
				rep = container.Reply{Error: "bad request: a control request carries no control"}
			case s.current() == nil:
				rep = container.Reply{Error: ErrNotRunning.Error()}
			default:
				s.Control(*req.Control)
			}
		default:
			rep = container.Reply{Error: fmt.Sprintf("this runwhp does not know the request %q", req.Op)}
		}
	}
	data, _ := json.Marshal(rep)
	c.Write(append(data, '\n'))
}

// running returns the VM of the boot under way, if the container's process
// runs.
func (s *Supervisor) running() *vmHandles {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Status != container.Running || s.cur == nil {
		return nil
	}
	return s.cur.vm
}

// withRoot returns the VM of the boot under way, if init has the
// container's root ready: it is created, or its process runs.
func (s *Supervisor) withRoot() *vmHandles {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (s.state.Status != container.Running && s.state.Status != container.Created) || s.cur == nil {
		return nil
	}
	return s.cur.vm
}

// stats reports what the running container uses: its VM's processor time
// and memory, this process's memory, which holds the guest's, the bytes
// its devices moved, and what init says of the guest.
func (s *Supervisor) stats() container.Reply {
	h := s.running()
	if h == nil {
		return container.Reply{Error: ErrNotRunning.Error()}
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
	st.NetRx, st.NetTx = h.network.traffic()
	for _, b := range h.blks {
		st.BlockRead += b.BytesRead.Load()
		st.BlockWrite += b.BytesWritten.Load()
	}
	st.Guest = h.guest.get(time.Second)
	return container.Reply{OK: true, Stats: st}
}

// top lists the running container's processes, as init sees them; a
// paused container's too, as init is not frozen with them.
func (s *Supervisor) top() container.Reply {
	h := s.running()
	if h == nil {
		return container.Reply{Error: ErrNotRunning.Error()}
	}
	p := h.top.get(5 * time.Second)
	switch {
	case p == nil:
		return container.Reply{Error: "the container's init did not list its processes in time"}
	case p.Error != "":
		return container.Reply{Error: "listing the container's processes: " + p.Error}
	}
	return container.Reply{OK: true, Top: p}
}

// connect connects c to a vsock port in the guest, and carries bytes both
// ways until the guest closes the connection. What c sent beyond its
// request waits in r. The guest's end of file closes c; c's end of file
// shuts down the guest's receiving side, so a stream can end in one
// direction while the other goes on.
func (s *Supervisor) connect(c net.Conn, r *bufio.Reader, req container.Request) {
	reply := func(rep container.Reply) {
		data, _ := json.Marshal(rep)
		c.Write(append(data, '\n'))
	}
	vc, err := s.DialGuest(context.Background(), req.Port)
	if err != nil {
		reply(container.Reply{Error: err.Error()})
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

// State is the container's state as this runwhp has it.
func (s *Supervisor) State() container.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
