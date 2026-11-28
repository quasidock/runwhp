//go:build windows

package main

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netstack"
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

// docker port lists each port with its protocol, and finds one asked for
// by number and protocol, TCP when none is given.
func TestPortLines(t *testing.T) {
	ports := []netstack.Port{
		{Host: netip.MustParseAddrPort("127.0.0.1:8080"), Guest: 80},
		{Host: netip.MustParseAddrPort("127.0.0.1:5353"), Guest: 53, Proto: "udp"},
		{Host: netip.MustParseAddrPort("127.0.0.1:5354"), Guest: 53},
	}
	c := &container.Container{ID: "0123456789abcdef", Name: "web", Config: &container.Config{Publish: ports},
		State: container.State{Status: container.Running, Ports: ports}}
	lines, err := portLines(c, "")
	if want := []string{"80/tcp -> 127.0.0.1:8080", "53/udp -> 127.0.0.1:5353", "53/tcp -> 127.0.0.1:5354"}; err != nil || !reflect.DeepEqual(lines, want) {
		t.Errorf("all: %q, %v", lines, err)
	}
	for want, port := range map[string]string{"127.0.0.1:5353": "53/udp", "127.0.0.1:5354": "53", "127.0.0.1:8080": "80/TCP"} {
		if lines, err := portLines(c, port); err != nil || !reflect.DeepEqual(lines, []string{want}) {
			t.Errorf("%s: %q, %v", port, lines, err)
		}
	}
	if _, err := portLines(c, "80/udp"); err == nil || err.Error() != "no public port '80/udp' published for web" {
		t.Errorf("80/udp: %v", err)
	}
}
