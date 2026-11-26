//go:build windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type healthJSON struct {
	Status        string
	FailingStreak int
	Log           []struct {
		ExitCode int
		Output   string
	}
}

// healthOf is a container's health, as state shows it.
func (s *ownStore) healthOf(ref string) *healthJSON {
	s.t.Helper()
	var st struct {
		Health      *healthJSON
		Healthcheck *struct{ Test []string }
	}
	if err := json.Unmarshal([]byte(s.ok("state", ref).stdout), &st); err != nil {
		s.t.Fatal(err)
	}
	return st.Health
}

// waitHealth waits for a container's health to reach status.
func (s *ownStore) waitHealth(ref, status string, d time.Duration) *healthJSON {
	s.t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(100 * time.Millisecond) {
		h := s.healthOf(ref)
		if h != nil && h.Status == status {
			return h
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("container %s's health is %+v after %v, want %s", ref, h, d, status)
		}
	}
}

// statusOf is a container's STATUS in list.
func (s *ownStore) statusOf(name string) string {
	s.t.Helper()
	for _, line := range strings.Split(s.ok("list", "-f", "name=^"+name+"$").stdout, "\n")[1:] {
		if i := strings.Index(line, "Up "); i >= 0 {
			return strings.TrimSpace(strings.SplitN(line[i:], "   ", 2)[0])
		}
	}
	return ""
}

func TestHealthcheck(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(s.ok(append([]string{"run", "-d"}, withFlags(args)...)...).stdout)
	}

	// Starting, healthy once the file is there, unhealthy two checks after
	// it goes.
	run("--name", "h1", "--health-cmd", "test -f /tmp/ok && echo probe-ok", "--health-interval", "1s", "--health-retries", "2",
		"--health-start-period", "3s", "--health-start-interval", "200ms",
		base, "sh", "-c", "sleep 2; touch /tmp/ok; sleep 4; rm /tmp/ok; sleep 300")
	if h := s.healthOf("h1"); h == nil || h.Status != "starting" {
		t.Errorf("at first: %+v", h)
	}
	if got := s.statusOf("h1"); !strings.HasSuffix(got, "(health: starting)") {
		t.Errorf("STATUS while starting: %q", got)
	}
	h := s.waitHealth("h1", "healthy", 10*time.Second)
	if last := h.Log[len(h.Log)-1]; last.ExitCode != 0 || last.Output != "probe-ok\n" {
		t.Errorf("a passing check: %+v", last)
	}
	if got := s.statusOf("h1"); !strings.HasSuffix(got, "(healthy)") {
		t.Errorf("STATUS when healthy: %q", got)
	}
	if got := s.ok("list", "-q", "-f", "health=healthy").stdout; !strings.Contains(got, s.idOf("h1")) {
		t.Errorf("list -f health=healthy: %q", got)
	}
	h = s.waitHealth("h1", "unhealthy", 15*time.Second)
	if h.FailingStreak < 2 || len(h.Log) > 5 || h.Log[len(h.Log)-1].ExitCode != 1 {
		t.Errorf("unhealthy: %+v", h)
	}

	// Paused, it is not checked; restarted, it starts again.
	s.ok("pause", "h1")
	before := s.healthOf("h1").FailingStreak
	time.Sleep(2500 * time.Millisecond)
	if got := s.healthOf("h1").FailingStreak; got != before {
		t.Errorf("checked while paused: streak %d, then %d", before, got)
	}
	if got := s.statusOf("h1"); !strings.HasSuffix(got, "(Paused)") {
		t.Errorf("STATUS when paused: %q", got)
	}
	s.ok("unpause", "h1")
	s.ok("restart", "-t", "0", "h1")
	if h := s.healthOf("h1"); h == nil || h.Status != "starting" || h.FailingStreak != 0 || len(h.Log) == 0 {
		t.Errorf("after a restart: %+v", h)
	}

	// A check that takes too long.
	run("--name", "h2", "--health-cmd", "echo out; sleep 5", "--health-interval", "1s", "--health-timeout", "1s", "--health-retries", "1", base, "sleep", "300")
	h = s.waitHealth("h2", "unhealthy", 10*time.Second)
	if last := h.Log[0]; last.ExitCode != -1 || last.Output != "Health check exceeded timeout (1s): out\n" {
		t.Errorf("a check that timed out: %+v", last)
	}

	// The image's HEALTHCHECK, and --no-healthcheck.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\nHEALTHCHECK --interval=1s --start-period=5s --start-interval=200ms CMD [\"true\"]\n"), 0o644)
	s.ok("build", "-q", "-t", "healthy:1", dir)
	run("--name", "h3", "healthy:1", "sleep", "300")
	start := time.Now()
	s.waitHealth("h3", "healthy", 10*time.Second)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the image's start interval: healthy after %v", d)
	}
	run("--name", "h4", "--no-healthcheck", "healthy:1", "sleep", "300")
	time.Sleep(1500 * time.Millisecond)
	if h := s.healthOf("h4"); h != nil {
		t.Errorf("--no-healthcheck: %+v", h)
	}
	if got := s.statusOf("h4"); strings.Contains(got, "health") {
		t.Errorf("STATUS without a check: %q", got)
	}

	// --stop-timeout and --stop-signal.
	run("--name", "s1", "--stop-timeout", "1", base, "sh", "-c", "trap '' TERM; sleep 300 & wait")
	start = time.Now()
	s.ok("stop", "s1")
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("stop of a container with --stop-timeout 1 took %v", d)
	}
	var st struct{ ExitCode int }
	json.Unmarshal([]byte(s.ok("state", "s1").stdout), &st)
	if st.ExitCode != 137 {
		t.Errorf("--stop-timeout: exit %d, want 137", st.ExitCode)
	}
	run("--name", "s2", "--stop-signal", "SIGUSR1", base, "sh", "-c", "trap 'exit 3' USR1; sleep 300 & wait")
	time.Sleep(500 * time.Millisecond)
	s.ok("stop", "s2")
	json.Unmarshal([]byte(s.ok("state", "s2").stdout), &st)
	if st.ExitCode != 3 {
		t.Errorf("--stop-signal SIGUSR1: exit %d, want 3", st.ExitCode)
	}
}

// idOf is a container's ID.
func (s *ownStore) idOf(ref string) string {
	s.t.Helper()
	var st struct{ ID string }
	json.Unmarshal([]byte(s.ok("state", ref).stdout), &st)
	return st.ID
}
