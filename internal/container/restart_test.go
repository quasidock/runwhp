package container

import (
	"testing"
	"time"
)

func TestParseRestartPolicy(t *testing.T) {
	for _, c := range []struct {
		in   string
		want RestartPolicy
		ok   bool
	}{
		{"", RestartPolicy{}, true},
		{"no", RestartPolicy{}, true},
		{"always", RestartPolicy{Name: RestartAlways}, true},
		{"unless-stopped", RestartPolicy{Name: RestartUnlessStopped}, true},
		{"on-failure", RestartPolicy{Name: RestartOnFailure}, true},
		{"on-failure:3", RestartPolicy{Name: RestartOnFailure, MaxRetries: 3}, true},
		{"on-failure:0", RestartPolicy{Name: RestartOnFailure}, true},
		{"on-failure:-1", RestartPolicy{}, false},
		{"on-failure:x", RestartPolicy{}, false},
		{"always:3", RestartPolicy{}, false},
		{"no:1", RestartPolicy{}, false},
		{"sometimes", RestartPolicy{}, false},
		{"Always", RestartPolicy{}, false},
	} {
		got, err := ParseRestartPolicy(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseRestartPolicy(%q) = %+v, %v", c.in, got, err)
		}
	}
	for in, want := range map[string]string{"": "no", "on-failure": "on-failure", "on-failure:2": "on-failure:2", "always": "always"} {
		p, _ := ParseRestartPolicy(in)
		if p.String() != want {
			t.Errorf("%q prints as %q", in, p.String())
		}
	}
}

func TestShouldRestart(t *testing.T) {
	no, _ := ParseRestartPolicy("no")
	always, _ := ParseRestartPolicy("always")
	unless, _ := ParseRestartPolicy("unless-stopped")
	onFailure, _ := ParseRestartPolicy("on-failure")
	onFailure2, _ := ParseRestartPolicy("on-failure:2")
	for _, c := range []struct {
		p           RestartPolicy
		exit, count int
		want        bool
	}{
		{no, 1, 0, false},
		{always, 0, 0, true},
		{always, 137, 100, true},
		{unless, 0, 5, true},
		{onFailure, 0, 0, false},
		{onFailure, 1, 0, true},
		{onFailure, 137, 1000, true},
		{onFailure2, 3, 0, true},
		{onFailure2, 3, 1, true},
		{onFailure2, 3, 2, false},
		{onFailure2, 0, 0, false},
	} {
		if got := c.p.ShouldRestart(c.exit, c.count); got != c.want {
			t.Errorf("%v: exit %d after %d restarts: %v", c.p, c.exit, c.count, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	var b Backoff
	var got []time.Duration
	for range 12 {
		got = append(got, b.Next(time.Second))
	}
	want := []time.Duration{100, 200, 400, 800, 1600, 3200, 6400, 12800, 25600, 51200, 60000, 60000}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Fatalf("delays %v", got)
		}
	}
	if d := b.Next(9990 * time.Millisecond); d != time.Minute {
		t.Errorf("after 9.99 s: %v", d)
	}
	if d := b.Next(10 * time.Second); d != 100*time.Millisecond {
		t.Errorf("after 10 s: %v", d)
	}
	b = Backoff{Initial: 10 * time.Millisecond, Max: 30 * time.Millisecond}
	if a, c, d := b.Next(0), b.Next(0), b.Next(0); a != 10*time.Millisecond || c != 20*time.Millisecond || d != 30*time.Millisecond {
		t.Errorf("own bounds: %v %v %v", a, c, d)
	}
	b.Reset()
	if d := b.Next(0); d != 10*time.Millisecond {
		t.Errorf("after Reset: %v", d)
	}
}
