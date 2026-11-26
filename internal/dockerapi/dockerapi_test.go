package dockerapi

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/jsonstream"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestSplitVersion(t *testing.T) {
	for _, tc := range []struct{ in, v, rest string }{
		{"/v1.56/containers/json", "1.56", "/containers/json"},
		{"/v1.40/_ping", "1.40", "/_ping"},
		{"/v1.41", "1.41", "/"},
		{"/containers/json", "", "/containers/json"},
		{"/volumes", "", "/volumes"},
		{"/vx.y/info", "", "/vx.y/info"},
		{"/version", "", "/version"},
	} {
		v, rest := SplitVersion(tc.in)
		if v != tc.v || rest != tc.rest {
			t.Errorf("SplitVersion(%q) = %q, %q; want %q, %q", tc.in, v, rest, tc.v, tc.rest)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"", "1.40", "1.44", "1.56"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	if err := CheckVersion("1.57"); err == nil || err.Error() != "client version 1.57 is too new. Maximum supported API version is 1.56" {
		t.Errorf("1.57: %v", err)
	}
	if err := CheckVersion("1.39"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("1.39: %v", err)
	}
	if CompareVersions("1.9", "1.10") >= 0 {
		t.Error("1.9 sorted after 1.10")
	}
}

func TestDecodeFilters(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want map[string][]string
	}{
		{"", map[string][]string{}},
		{`{"label":["a=b","c"],"Status":["running"]}`, map[string][]string{"label": {"a=b", "c"}, "status": {"running"}}},
		{`{"label":{"c":true,"a=b":true,"off":false}}`, map[string][]string{"label": {"a=b", "c"}}},
	} {
		got, err := DecodeFilters(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("DecodeFilters(%s) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := DecodeFilters(`{"label":"x"}`); err == nil {
		t.Error("a string value was taken")
	}
	f, _ := DecodeFilters(`{"name":["x"],"bogus":["y"]}`)
	if err := CheckFilterKeys(f, "name", "label"); err == nil || err.Error() != "invalid filter 'bogus'" {
		t.Errorf("CheckFilterKeys: %v", err)
	}
}

func TestStream(t *testing.T) {
	var buf bytes.Buffer
	flushes := 0
	s := NewStream(&buf, func() { flushes++ })
	s.Status("abc", "Downloading", &jsonstream.Progress{Current: 1, Total: 2})
	s.Text("Step 1/2\n")
	s.Aux(map[string]string{"ID": "sha256:x"})
	s.Error(errors.New("no luck"))
	want := `{"status":"Downloading","progressDetail":{"current":1,"total":2},"id":"abc"}` + "\r\n" +
		`{"stream":"Step 1/2\n"}` + "\r\n" +
		`{"aux":{"ID":"sha256:x"}}` + "\r\n" +
		`{"errorDetail":{"message":"no luck"},"error":"no luck"}` + "\r\n"
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}
	if flushes != 4 || !s.Failed {
		t.Errorf("flushes %d, failed %v", flushes, s.Failed)
	}
}

func TestStatsCPUPercent(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	prev := &container.Stats{Time: t0, CPUs: 4, CPUTime: 10 * time.Second}
	// 1.5 s of vCPU time in 1 s: 150%, as runwhp stats shows it.
	cur := &container.Stats{Time: t0.Add(time.Second), CPUs: 4, CPUTime: 11500 * time.Millisecond, MemLimit: 1 << 30,
		Guest: &guestspec.GuestStats{MemTotal: 1000, MemAvailable: 400, Tasks: 7}}
	s := Stats("id", "/name", cur, prev)
	if got := CPUPercent(s); got < 149.99 || got > 150.01 {
		t.Errorf("CPU %.3f%%, want 150%%", got)
	}
	if s.MemoryStats.Usage != 600 || s.MemoryStats.Limit != 1<<30 || s.PidsStats.Current != 7 {
		t.Errorf("memory %+v, pids %+v", s.MemoryStats, s.PidsStats)
	}
	if !s.PreRead.Equal(t0) || s.OSType != "linux" {
		t.Errorf("preread %v, os %q", s.PreRead, s.OSType)
	}
}

func TestContainerEvents(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	now := t0.Add(10 * time.Second)
	a := ContainerSnap{ID: "a", Name: "web", Image: "alpine", Created: t0, Labels: map[string]string{"app": "x"}}
	b := ContainerSnap{ID: "b", Name: "db", Image: "postgres", Created: t0, Running: true, StartedAt: t0.Add(time.Second)}
	prev := map[string]ContainerSnap{"a": a, "b": b}
	// a started; b died and started again, became healthy, was renamed;
	// c appeared and ran to its end; a d that was running is gone.
	a2 := a
	a2.Running, a2.StartedAt = true, t0.Add(5*time.Second)
	b2 := b
	b2.FinishedAt, b2.ExitCode, b2.StartedAt, b2.Health, b2.Name = t0.Add(3*time.Second), 137, t0.Add(4*time.Second), "healthy", "db2"
	c := ContainerSnap{ID: "c", Name: "job", Image: "alpine", Created: t0.Add(6 * time.Second), StartedAt: t0.Add(7 * time.Second), FinishedAt: t0.Add(8 * time.Second), ExitCode: 3}
	d := ContainerSnap{ID: "d", Name: "gone", Running: true}
	prev["d"] = d
	got := ContainerEvents(prev, map[string]ContainerSnap{"a": a2, "b": b2, "c": c}, now)
	var seq []string
	for _, m := range got {
		s := m.Actor.ID + " " + string(m.Action)
		if code, ok := m.Actor.Attributes["exitCode"]; ok {
			s += " " + code
		}
		seq = append(seq, s)
	}
	want := []string{"b die 137", "b start", "a start", "c create", "c start", "c die 3",
		"b rename", "b health_status: healthy", "d die 0", "d destroy"}
	if !reflect.DeepEqual(seq, want) {
		t.Errorf("got  %q\nwant %q", seq, want)
	}
	if got[0].Actor.Attributes["name"] != "db2" || got[2].Actor.Attributes["app"] != "x" {
		t.Errorf("attributes: %v, %v", got[0].Actor.Attributes, got[2].Actor.Attributes)
	}
	f := EventFilter{"event": {"die"}, "container": {"db2"}}
	if !f.Match(got[0]) || f.Match(got[1]) {
		t.Error("filter")
	}
}
