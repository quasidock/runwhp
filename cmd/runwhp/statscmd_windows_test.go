//go:build windows

package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestStatsRows(t *testing.T) {
	web := &container.Container{ID: strings.Repeat("ab", 32), Name: "web"}
	db := &container.Container{ID: strings.Repeat("cd", 32)}
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	guest := &guestspec.GuestStats{MemTotal: 8 << 30, MemAvailable: 6 << 30, Cached: 1 << 30, Tasks: 7}
	prev := []statsSample{
		{c: db, err: errors.New("container cdcdcdcdcdcd is not running")},
		{c: web, stats: &container.Stats{Time: t0, CPUs: 4, CPUTime: 10 * time.Second}},
	}
	cur := []statsSample{
		{c: db, err: errors.New("container cdcdcdcdcdcd is not running")},
		{c: web, stats: &container.Stats{Time: t0.Add(time.Second), CPUs: 4, CPUTime: 11500 * time.Millisecond,
			MemLimit: 8 << 30, HostCommit: 300 << 20, NetRx: 1500, NetTx: 2_500_000, BlockRead: 4 << 20, BlockWrite: 0, Guest: guest}},
	}
	rows := statsRows(prev, cur)
	if len(rows) != 2 || rows[0].Name != "web" || rows[1].Error == "" {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	if r.CPUPercent != 150 || r.MemUsage != 2<<30 || r.MemPercent != 25 || r.PIDs != 7 || !r.guest {
		t.Errorf("row %+v", r)
	}

	var out bytes.Buffer
	printStatsTable(&out, rows)
	want := `CONTAINER ID   NAME   CPU %     MEM USAGE / LIMIT   MEM %    HOST MEM   NET I/O         BLOCK I/O     PIDS
abababababab   web    150.00%   2GiB / 8GiB         25.00%   300MiB     1.5kB / 2.5MB   4.19MB / 0B   7
cdcdcdcdcdcd          --        --                  --       --         --              --            --
`
	if out.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", out.String(), want)
	}

	// Without init's answer, the guest's numbers are unknown, not zero.
	cur[1].stats.Guest = nil
	out.Reset()
	printStatsTable(&out, statsRows(prev, cur)[:1])
	if !strings.Contains(out.String(), "--") {
		t.Errorf("table without the guest's numbers:\n%s", out.String())
	}
}

func TestSizes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0B", 1023: "1023B", 1536: "1.5KiB", 300 << 20: "300MiB", 7852 << 20: "7.67GiB"} {
		if got := binarySize(n); got != want {
			t.Errorf("binarySize(%d) = %s, want %s", n, got, want)
		}
	}
	for n, want := range map[uint64]string{999: "999B", 1500: "1.5kB", 2_500_000: "2.5MB", 123_456_789_012: "123GB"} {
		if got := decimalSize(n); got != want {
			t.Errorf("decimalSize(%d) = %s, want %s", n, got, want)
		}
	}
}

// TestGuestStats checks that an answer is matched to its request: a late
// answer to one that gave up is not taken for the next.
func TestGuestStats(t *testing.T) {
	var mu sync.Mutex
	listening := false
	var sent []guestspec.Control
	g := &guestStats{
		listening: func() bool { mu.Lock(); defer mu.Unlock(); return listening },
		send:      func(c guestspec.Control) bool { mu.Lock(); sent = append(sent, c); mu.Unlock(); return true },
		replies:   make(chan *guestspec.GuestStats, 1),
	}
	if g.get(time.Millisecond) != nil || len(sent) != 0 {
		t.Error("asked init before it listened")
	}
	mu.Lock()
	listening = true
	mu.Unlock()
	// Nobody answers the first request.
	if g.get(10*time.Millisecond) != nil || len(sent) != 1 || sent[0].Stats != 1 {
		t.Fatalf("sent %+v", sent)
	}
	// Its answer arrives late, just as the second is answered.
	g.deliver(&guestspec.GuestStats{Seq: 1, Tasks: 1})
	go func() {
		for {
			mu.Lock()
			n := len(sent)
			mu.Unlock()
			if n == 2 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		g.deliver(&guestspec.GuestStats{Seq: 2, Tasks: 2})
	}()
	if st := g.get(5 * time.Second); st == nil || st.Seq != 2 || st.Tasks != 2 {
		t.Errorf("second answer %+v", st)
	}
	// deliver never blocks, even with nobody waiting.
	g.deliver(&guestspec.GuestStats{Seq: 9})
	g.deliver(&guestspec.GuestStats{Seq: 10})
}

func TestEventWriterStats(t *testing.T) {
	var got []uint64
	w := eventWriter{onStats: func(st *guestspec.GuestStats) { got = append(got, st.Seq) }}
	w.Write([]byte(`{"stats":{"seq":3,"memTotal":1}}` + "\n" + `{"started":true,"pid":5}` + "\n"))
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("stats events: %v", got)
	}
}
