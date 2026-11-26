package container

import (
	"strings"
	"testing"
	"time"
)

func TestHealthcheckArgs(t *testing.T) {
	for _, c := range []struct {
		test    []string
		args    []string
		enabled bool
	}{
		{[]string{"CMD", "pg_isready", "-U", "x"}, []string{"pg_isready", "-U", "x"}, true},
		{[]string{"CMD-SHELL", "test -f /ok"}, []string{"/bin/sh", "-c", "test -f /ok"}, true},
		{[]string{"NONE"}, nil, false},
		{nil, nil, false},
		{[]string{"CMD"}, []string{}, false},
		{[]string{"CMD-SHELL"}, nil, false},
	} {
		h := &Healthcheck{Test: c.test}
		if got := h.Args(); strings.Join(got, "|") != strings.Join(c.args, "|") || h.Enabled() != c.enabled {
			t.Errorf("%q: args %q, enabled %v", c.test, got, h.Enabled())
		}
	}
	var none *Healthcheck
	if none.Enabled() {
		t.Error("nil healthcheck enabled")
	}
	h := &Healthcheck{}
	if h.IntervalOrDefault() != 30*time.Second || h.TimeoutOrDefault() != 30*time.Second ||
		h.StartIntervalOrDefault() != 5*time.Second || h.RetriesOrDefault() != 3 {
		t.Errorf("defaults: %v %v %v %v", h.IntervalOrDefault(), h.TimeoutOrDefault(), h.StartIntervalOrDefault(), h.RetriesOrDefault())
	}
}

// TestNextHealth follows the sequence check 19 recorded from Docker 29:
// --health-retries 2, --health-start-period 4s, a check failing until 6 s,
// passing until 10 s, then failing.
func TestNextHealth(t *testing.T) {
	hc := &Healthcheck{Test: []string{"CMD-SHELL", "test -f /tmp/ok"}, Retries: 2, StartPeriod: 4 * time.Second}
	h := StartHealth(nil)
	step := func(code int, inStart bool, status string, streak int) {
		t.Helper()
		h = NextHealth(h, hc, HealthResult{ExitCode: code}, inStart)
		if h.Status != status || h.FailingStreak != streak {
			t.Fatalf("after exit %d (start period %v): %s streak %d, want %s streak %d", code, inStart, h.Status, h.FailingStreak, status, streak)
		}
	}
	step(1, true, HealthStarting, 0) // failures in the start period do not count
	step(1, true, HealthStarting, 0)
	step(1, false, HealthStarting, 1) // after it, they do
	step(1, false, HealthUnhealthy, 2)
	step(1, false, HealthUnhealthy, 3) // and keep counting
	step(0, false, HealthHealthy, 0)   // one success is enough
	step(1, false, HealthHealthy, 1)
	step(1, false, HealthUnhealthy, 2)
	if len(h.Log) != HealthLogSize {
		t.Errorf("log keeps %d results, want %d", len(h.Log), HealthLogSize)
	}

	// A success in the start period makes it healthy, and a failure after
	// that counts, start period or not.
	h = StartHealth(nil)
	step(0, true, HealthHealthy, 0)
	step(1, true, HealthHealthy, 1)

	// A restart starts again, keeping the log.
	again := StartHealth(h)
	if again.Status != HealthStarting || again.FailingStreak != 0 || len(again.Log) != len(h.Log) {
		t.Errorf("restarted: %+v", again)
	}
	again.Log[0].Output = "changed"
	if h.Log[0].Output == "changed" {
		t.Error("StartHealth shares its log with the last run's")
	}
}

func TestNextHealthInterval(t *testing.T) {
	hc := &Healthcheck{Interval: time.Second, StartInterval: 500 * time.Millisecond, StartPeriod: 4 * time.Second}
	starting, healthy := &Health{Status: HealthStarting}, &Health{Status: HealthHealthy}
	for _, c := range []struct {
		h     *Health
		since time.Duration
		want  time.Duration
	}{
		{starting, time.Second, 500 * time.Millisecond},
		{nil, 0, 500 * time.Millisecond},
		{healthy, time.Second, time.Second},
		{starting, 5 * time.Second, time.Second},
	} {
		if got := NextHealthInterval(c.h, hc, c.since); got != c.want {
			t.Errorf("%+v after %v: %v, want %v", c.h, c.since, got, c.want)
		}
	}
	if got := NextHealthInterval(starting, &Healthcheck{}, 0); got != DefaultHealthInterval {
		t.Errorf("no start period: %v", got)
	}
}

func TestTruncateHealthOutput(t *testing.T) {
	big := []byte(strings.Repeat("x", 10000))
	if got := TruncateHealthOutput(big); len(got) != 4099 || !strings.HasSuffix(got, "x...") {
		t.Errorf("10000 bytes kept as %d", len(got))
	}
	if got := TruncateHealthOutput(big[:4096]); len(got) != 4096 {
		t.Errorf("4096 bytes kept as %d", len(got))
	}
	if got := (State{}).HealthStatus(); got != HealthNone {
		t.Errorf("no health: %q", got)
	}
}
