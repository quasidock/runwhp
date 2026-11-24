package main

import (
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// testSupervisor runs no VM: boots are begun and ended by the test, which
// stands in for bootContainer.
func testSupervisor(t *testing.T, policy string) *supervisor {
	t.Helper()
	root := t.TempDir()
	store := container.Open(root)
	p, err := container.ParseRestartPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &container.Config{Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(),
		Image: "alpine:3.20", Args: []string{"true"}, Restart: p}
	rec, err := store.Create(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := supervise(root, store, rec, container.OutputLog)
	if err != nil {
		t.Fatal(err)
	}
	s.policy = p
	s.backoff = container.Backoff{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond}
	t.Cleanup(func() {
		select {
		case <-s.done:
		default:
			s.finish()
		}
	})
	return s
}

func testProxy(t *testing.T) *signalProxy {
	return &signalProxy{stop: func() {}, queue: newControlQueue(), logf: t.Logf}
}

// runBoot begins a boot, starts its process unless notStarted, and ends it
// with code.
func runBoot(t *testing.T, s *supervisor, code int, notStarted bool) (*vmBoot, bool, time.Duration) {
	t.Helper()
	b := s.beginBoot(testProxy(t))
	if b == nil {
		t.Fatal("no boot")
	}
	if !notStarted {
		s.started(b, 1)
	}
	restart, delay := s.endBoot(b, code, nil)
	return b, restart, delay
}

func TestRestartDecisions(t *testing.T) {
	s := testSupervisor(t, "on-failure:2")
	for i, want := range []bool{true, true, false} {
		_, restart, delay := runBoot(t, s, 3, false)
		if restart != want {
			t.Fatalf("run %d: restart %v", i+1, restart)
		}
		st := s.reply()
		if want {
			if st.Status != container.Restarting || delay != 10*time.Millisecond<<i || s.state.RestartCount != i+1 {
				t.Errorf("run %d: %+v, delay %v, count %d", i+1, st, delay, s.state.RestartCount)
			}
			if !s.waitRestart(delay) {
				t.Fatal("waitRestart")
			}
		} else if st.Status != container.Stopped || st.ExitCode != 3 {
			t.Errorf("last: %+v", st)
		}
	}
	// The record says so.
	c, err := s.store.Get(s.rec.ID)
	if err != nil || c.State.Status != container.Stopped || c.State.RestartCount != 2 || c.State.ExitCode != 3 {
		t.Errorf("recorded %+v, %v", c.State, err)
	}

	s = testSupervisor(t, "on-failure")
	if _, restart, _ := runBoot(t, s, 0, false); restart {
		t.Error("on-failure restarted after exit 0")
	}
	s = testSupervisor(t, "always")
	if _, restart, _ := runBoot(t, s, 1, true); restart {
		t.Error("restarted a container whose process never started")
	}
	s = testSupervisor(t, "no")
	if _, restart, _ := runBoot(t, s, 1, false); restart {
		t.Error("restarted with no policy")
	}
}

// TestRestartByHand checks that a container stopped or killed, or whose
// foreground runwhp was sent a signal, is not started again.
func TestRestartByHand(t *testing.T) {
	// kill: init is not listening, so the VM is stopped at once.
	s := testSupervisor(t, "always")
	b := s.beginBoot(testProxy(t))
	s.started(b, 1)
	if err := s.kill(1, false); err != nil {
		t.Fatal(err)
	}
	if restart, _ := s.endBoot(b, 0, nil); restart {
		t.Error("restarted after a kill")
	}

	s = testSupervisor(t, "always")
	p := testProxy(t)
	b = s.beginBoot(p)
	s.started(b, 1)
	p.signaled.Store(true)
	if restart, _ := s.endBoot(b, 130, nil); restart {
		t.Error("restarted after a signal to its runwhp")
	}

	// stop between boots ends the restarts, keeping the last exit.
	s = testSupervisor(t, "always")
	_, restart, _ := runBoot(t, s, 4, false)
	if !restart {
		t.Fatal("not restarting")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := s.kill(guestspec.SIGTERM, false); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	if s.waitRestart(time.Hour) {
		t.Fatal("waited through a stop")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("slow to stop")
	}
	if st := s.reply(); st.Status != container.Stopped || st.ExitCode != 4 {
		t.Errorf("after stop: %+v", st)
	}
	if s.beginBoot(testProxy(t)) != nil {
		t.Error("booted after a stop")
	}
}

// TestRestartWait checks that wait on a container waiting to start again
// answers with its next run's exit, and start leaves it as it is.
func TestRestartWait(t *testing.T) {
	s := testSupervisor(t, "always")
	runBoot(t, s, 1, false)
	if r := s.start(); !r.OK || r.Status != container.Restarting {
		t.Errorf("start while restarting: %+v", r)
	}
	got := make(chan container.Reply, 1)
	go func() { got <- s.wait() }()
	time.Sleep(20 * time.Millisecond)
	select {
	case r := <-got:
		t.Fatalf("wait answered before the next run: %+v", r)
	default:
	}
	b := s.beginBoot(testProxy(t))
	s.started(b, 1)
	s.mu.Lock()
	s.stopRestarts()
	s.mu.Unlock()
	s.endBoot(b, 5, nil)
	select {
	case r := <-got:
		if r.ExitCode != 5 || r.Status != container.Stopped {
			t.Errorf("wait: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not answer")
	}
}

func TestStatusTextRestarting(t *testing.T) {
	now := time.Now()
	st := container.State{Status: container.Restarting, ExitCode: 1, FinishedAt: now.Add(-3 * time.Second)}
	if got := statusText(st, now); got != "Restarting (1) 3 seconds ago" {
		t.Errorf("statusText = %q", got)
	}
}

func TestRestartBackoffEnv(t *testing.T) {
	for v, want := range map[string]container.Backoff{
		"10ms":       {Initial: 10 * time.Millisecond},
		"10ms,1s":    {Initial: 10 * time.Millisecond, Max: time.Second},
		"soon":       {},
		"0s":         {},
		"10ms,later": {},
	} {
		t.Setenv("RUNWHP_RESTART_BACKOFF", v)
		if got := restartBackoff(t.Logf); got != want {
			t.Errorf("%q: %+v", v, got)
		}
	}
}

// testFreezer answers each freeze or thaw at once, or with fail.
func testFreezer(fail string) *guestFreezer {
	g := &guestFreezer{listening: func() bool { return true }, replies: make(chan *guestspec.Freeze, 1)}
	g.send = func(c guestspec.Control) bool {
		go g.deliver(&guestspec.Freeze{Seq: c.Freeze.Seq, Frozen: c.Freeze.Frozen, Error: fail})
		return true
	}
	return g
}

func TestPause(t *testing.T) {
	s := testSupervisor(t, "")
	id := s.rec.ID
	if r := s.pause(); r.OK || r.Error != "container "+id+" is not running" {
		t.Errorf("pause before a boot: %+v", r)
	}
	b := s.beginBoot(testProxy(t))
	s.mu.Lock()
	b.vm = &vmHandles{freezer: testFreezer("")}
	s.mu.Unlock()
	s.started(b, 1)
	if r := s.unpause(); r.OK || r.Error != "Container "+id+" is not paused" {
		t.Errorf("unpause of a running container: %+v", r)
	}
	if r := s.pause(); !r.OK {
		t.Fatalf("pause: %+v", r)
	}
	if !s.state.Paused {
		t.Error("not recorded as paused")
	}
	if got := statusText(s.state, time.Now()); got != "Up Less than a second (Paused)" {
		t.Errorf("statusText = %q", got)
	}
	s.pending.Wait()
	s.persist()
	c, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(c); st.Status != container.Paused {
		t.Errorf("state's status = %q", st.Status)
	}
	if r := s.pause(); r.OK || r.Error != "container "+id+" is already paused" {
		t.Errorf("second pause: %+v", r)
	}
	if r := s.unpause(); !r.OK || s.state.Paused {
		t.Errorf("unpause: %+v, paused %v", r, s.state.Paused)
	}

	// A signal thaws it.
	s.pause()
	s.thawAfterSignal(b)
	if s.state.Paused {
		t.Error("still paused after a signal")
	}

	// A failed freeze leaves it as it was.
	s.mu.Lock()
	b.vm = &vmHandles{freezer: testFreezer("no cgroup")}
	s.mu.Unlock()
	if r := s.pause(); r.OK || r.Error != "pausing container "+id+": no cgroup" || s.state.Paused {
		t.Errorf("failed pause: %+v, paused %v", r, s.state.Paused)
	}

	// The boot's end clears it.
	s.mu.Lock()
	b.vm = &vmHandles{freezer: testFreezer("")}
	s.mu.Unlock()
	s.pause()
	s.endBoot(b, 0, nil)
	if s.state.Paused {
		t.Error("paused after the boot ended")
	}
}
