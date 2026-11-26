//go:build windows

package main

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

func TestHealthFlags(t *testing.T) {
	parse := func(args ...string) (runSpec, error) {
		var o runOptions
		fs := runFlags(&o)
		fs.SetOutput(io.Discard)
		if err := fs.Parse(append(args, "alpine")); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		return parseRunOptions(&o, false, fs.Args())
	}
	s, err := parse("--health-cmd", "test -f /ok", "--health-interval", "1s", "--health-retries", "2",
		"--health-start-period", "4s", "--health-start-interval", "500ms", "--health-timeout", "3s",
		"--stop-signal", "SIGUSR1", "--stop-timeout", "3")
	if err != nil {
		t.Fatal(err)
	}
	want := healthOverrides{test: []string{"CMD-SHELL", "test -f /ok"}, interval: time.Second, timeout: 3 * time.Second,
		startPeriod: 4 * time.Second, startInterval: 500 * time.Millisecond, retries: 2}
	if !reflect.DeepEqual(s.health, want) || s.stopSignal != "SIGUSR1" || s.stopTimeout == nil || *s.stopTimeout != 3 {
		t.Errorf("parsed %+v, %q, %v", s.health, s.stopSignal, s.stopTimeout)
	}
	if s, err := parse("--no-healthcheck"); err != nil || !s.health.disable {
		t.Errorf("--no-healthcheck: %+v, %v", s.health, err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--no-healthcheck", "--health-cmd", "x"}, "--no-healthcheck conflicts with --health-* options"},
		{[]string{"--health-interval", "-1s"}, "--health-interval cannot be negative"},
		{[]string{"--health-timeout", "10us"}, "Timeout in Healthcheck cannot be less than 1ms"},
		{[]string{"--health-retries", "-1"}, "--health-retries cannot be negative"},
		{[]string{"--health-interval", "soon"}, `invalid argument "soon" for "--health-interval" flag`},
		{[]string{"--stop-signal", "SIGBOGUS"}, "Invalid signal: SIGBOGUS"},
		{[]string{"--stop-timeout", "x"}, "invalid --stop-timeout"},
	} {
		if _, err := parse(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.args, err, c.want)
		}
	}
}

func TestResolveHealth(t *testing.T) {
	img := imageHealth([]byte(`{"config":{"Healthcheck":{"Test":["CMD","wget","-q","localhost"],"Interval":30000000000,"StartInterval":1000000000,"Retries":5}}}`))
	if img == nil || img.StartInterval != time.Second || img.Retries != 5 || img.Test[0] != "CMD" {
		t.Fatalf("image healthcheck %+v", img)
	}
	if imageHealth([]byte(`{"config":{}}`)) != nil || imageHealth(nil) != nil {
		t.Error("a healthcheck from nothing")
	}
	got := resolveHealth(img, healthOverrides{interval: time.Second})
	if got.Interval != time.Second || got.Retries != 5 || got.StartInterval != time.Second || got.Test[1] != "wget" {
		t.Errorf("interval over the image's: %+v", got)
	}
	got = resolveHealth(img, healthOverrides{test: []string{"CMD-SHELL", "true"}})
	if !reflect.DeepEqual(got.Test, []string{"CMD-SHELL", "true"}) || got.Retries != 5 {
		t.Errorf("command over the image's: %+v", got)
	}
	if got := resolveHealth(img, healthOverrides{disable: true}); got.Enabled() || got.Test[0] != "NONE" {
		t.Errorf("disabled: %+v", got)
	}
	if got := resolveHealth(nil, healthOverrides{interval: time.Second}); got != nil {
		t.Errorf("an interval alone: %+v", got)
	}
	if got := resolveHealth(nil, healthOverrides{}); got != nil {
		t.Errorf("nothing: %+v", got)
	}
	if img := imageHealth(nil); img != nil {
		t.Errorf("%+v", img)
	}
}

func TestHealthStatusText(t *testing.T) {
	now := time.Now()
	st := container.State{Status: container.Running, StartedAt: now.Add(-5 * time.Second)}
	for h, want := range map[string]string{
		"":                        "Up 5 seconds",
		container.HealthStarting:  "Up 5 seconds (health: starting)",
		container.HealthHealthy:   "Up 5 seconds (healthy)",
		container.HealthUnhealthy: "Up 5 seconds (unhealthy)",
	} {
		st.Health = nil
		if h != "" {
			st.Health = &container.Health{Status: h}
		}
		if got := statusText(st, now); got != want {
			t.Errorf("%q: %q, want %q", h, got, want)
		}
	}
	st.Paused = true
	if got := statusText(st, now); got != "Up 5 seconds (Paused)" {
		t.Errorf("paused: %q", got)
	}
	st.Health = &container.Health{Status: container.HealthHealthy}
	f, err := parseContainerFilters([]string{"health=healthy"})
	if err != nil || !f.matchContainer(&container.Container{State: st, Config: &container.Config{}}) {
		t.Errorf("health filter: %v", err)
	}
	if _, err := parseContainerFilters([]string{"health=bogus"}); err == nil || err.Error() != "Unrecognised filter value for health: bogus" {
		t.Errorf("bad health filter: %v", err)
	}
}
