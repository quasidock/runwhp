package dockerapi

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestTop(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	p := &guestspec.Processes{
		Now: now, MemTotal: 1 << 30,
		Procs: []guestspec.Process{
			// Listed out of order: Top sorts by PID.
			{PID: 9, PPID: 3, PGID: 9, SID: 3, TPGID: 9, UID: 1000, State: "R", TTY: 136<<8 | 0, Threads: 1,
				Start: now.Add(-10 * time.Second), CPU: 5 * time.Second, VSZ: 2 << 20, RSS: 1 << 20, Comm: "top", Args: []string{"top", "-b"}},
			{PID: 3, PPID: 1, PGID: 3, SID: 3, User: "root", State: "S", Threads: 4, Nice: 5,
				Start: now.Add(-48 * time.Hour), CPU: 90061 * time.Second, VSZ: 8 << 20, RSS: 4 << 20, Comm: "sh"},
		},
	}
	titles, rows, err := Top(p, "", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("titles %q", titles)
	}
	want := [][]string{
		{"root", "3", "1", "52", "Nov24", "?", "1-01:01:01", "[sh]"},
		{"1000", "9", "3", "50", "11:59", "pts/0", "00:00:05", "top -b"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("-ef rows\n%q\nwant\n%q", rows, want)
	}

	titles, rows, err = Top(p, "aux", time.UTC)
	if err != nil || len(titles) != 11 || titles[2] != "%CPU" {
		t.Fatalf("aux: %q, %v", titles, err)
	}
	want = [][]string{
		{"root", "3", "52.1", "0.4", "8192", "4096", "?", "SNsl", "Nov24", "1501:01", "[sh]"},
		{"1000", "9", "50.0", "0.1", "2048", "1024", "pts/0", "R+", "11:59", "0:05", "top -b"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("aux rows\n%q\nwant\n%q", rows, want)
	}

	if _, _, err := Top(p, "-o pid", time.UTC); !errors.Is(err, ErrPsArgs) {
		t.Errorf("-o pid: %v", err)
	}
	if _, rows, _ := Top(&guestspec.Processes{}, "-ef", time.UTC); rows == nil {
		t.Error("no processes gave nil rows, which Docker's JSON has as []")
	}
	if got := ttyName(4<<8 | 65); got != "ttyS1" {
		t.Errorf("ttyName %q", got)
	}
}
