//go:build windows

package main

import (
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

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

func TestStatusPaused(t *testing.T) {
	now := time.Now()
	st := container.State{Status: container.Running, Paused: true, StartedAt: now, Pid: 1}
	if got := statusText(st, now); got != "Up Less than a second (Paused)" {
		t.Errorf("statusText = %q", got)
	}
	c, err := container.Open(t.TempDir()).CreateWith(&container.Config{ID: container.NewID(), Args: []string{"true"}}, "", st)
	if err != nil {
		t.Fatal(err)
	}
	c.State.Status = container.Running // CreateWith records it as creating
	if s := stateOf(c); s.Status != container.Paused {
		t.Errorf("state's status = %q", s.Status)
	}
}
