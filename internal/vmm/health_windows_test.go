package vmm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

// fakeProbe answers checks with the codes it is given, in turn, then 0.
type fakeProbe struct {
	mu    sync.Mutex
	codes []int
	calls int
	args  []string
}

func (f *fakeProbe) probe(ctx context.Context, args []string, timeout time.Duration) container.HealthResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.args = args
	code := 0
	if len(f.codes) > 0 {
		code, f.codes = f.codes[0], f.codes[1:]
	}
	now := time.Now()
	return container.HealthResult{Start: now, End: now, ExitCode: code}
}

func (f *fakeProbe) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func waitHealth(t *testing.T, s *Supervisor, status string, streak int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		s.mu.Lock()
		h := s.state.Health
		s.mu.Unlock()
		if h != nil && h.Status == status && h.FailingStreak == streak {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health %+v, want %s with streak %d", h, status, streak)
		}
	}
}

func TestHealthLoop(t *testing.T) {
	s := testSupervisor(t, "")
	hc := &container.Healthcheck{Test: []string{"CMD", "check"}, Interval: 10 * time.Millisecond, Retries: 2}
	b := s.beginBoot(testProxy(t))
	s.started(b, 1)
	f := &fakeProbe{codes: []int{1, 1, 0}}
	done := make(chan struct{})
	go func() { s.healthLoop(b, hc, f.probe); close(done) }()

	waitHealth(t, s, container.HealthUnhealthy, 2)
	waitHealth(t, s, container.HealthHealthy, 0)
	f.mu.Lock()
	if f.args[0] != "check" {
		t.Errorf("ran %q", f.args)
	}
	f.mu.Unlock()

	// Paused, it is not checked.
	s.mu.Lock()
	s.state.Paused = true
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	n := f.count()
	time.Sleep(50 * time.Millisecond)
	if f.count() != n {
		t.Errorf("checked %d times while paused", f.count()-n)
	}
	s.mu.Lock()
	s.state.Paused = false
	s.mu.Unlock()

	// The state records it.
	s.persist()
	c, err := s.store.Get(s.rec.ID)
	if err != nil || c.State.Health == nil || c.State.Health.Status != container.HealthHealthy || len(c.State.Health.Log) == 0 {
		t.Errorf("recorded %+v, %v", c.State.Health, err)
	}

	// It ends with the boot, and the health stays.
	s.endBoot(b, 0, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end with the boot")
	}
	if s.state.Health == nil || s.state.Health.Status != container.HealthHealthy {
		t.Errorf("after the boot: %+v", s.state.Health)
	}

	// The next boot starts again.
	b2 := s.beginBoot(testProxy(t))
	s.started(b2, 1)
	f2 := &fakeProbe{codes: []int{1}}
	go s.healthLoop(b2, &container.Healthcheck{Test: []string{"CMD", "check"}, Interval: time.Hour}, f2.probe)
	waitHealth(t, s, container.HealthStarting, 0)
	s.endBoot(b2, 0, nil)
}
