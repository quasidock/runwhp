//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMeminfo(t *testing.T) {
	f := parseMeminfo([]byte("MemTotal:        8227224 kB\nMemAvailable:    8100000 kB\nActive(file):      1024 kB\nHugePages_Total:       0\nbogus line\n"))
	if f["MemTotal"] != 8227224<<10 || f["Active(file)"] != 1<<20 || f["HugePages_Total"] != 0 || len(f) != 4 {
		t.Errorf("parsed %v", f)
	}
}

func TestCPUTimes(t *testing.T) {
	stat := "cpu  100 5 50 1000 20 3 2 0 0 0\ncpu0 50 2 25 500 10 1 1 0 0 0\ncpu1 50 3 25 500 10 2 1 0 0 0\nintr 12345\n"
	busy, total, cpus := cpuTimes([]byte(stat))
	if busy != 160 || total != 1180 || cpus != 2 {
		t.Errorf("busy %d, total %d, cpus %d", busy, total, cpus)
	}
}

// sampleAt builds a sample after ticks clock ticks on cpus processors, of
// which busy were busy, with file bytes of page cache in 8 GiB of RAM.
func sampleAt(prev memSample, ticks, busy uint64, file uint64) memSample {
	return memSample{busy: prev.busy + busy, total: prev.total + ticks, cpus: 4, file: file, memTotal: 8 << 30}
}

func TestReclaimer(t *testing.T) {
	const interval = 5 * time.Second
	const ticks = 4 * 500 // 4 processors, 5 s at 100 ticks a second
	r := &reclaimer{idle: 15 * time.Second}
	s := memSample{cpus: 4, memTotal: 8 << 30, file: 2 << 30}
	if n := r.next(s, interval); n != 0 {
		t.Errorf("first sample reclaims %d", n)
	}
	// Busy: 10% of one processor.
	s = sampleAt(s, ticks, 50, 2<<30)
	if n := r.next(s, interval); n != 0 || r.idleFor != 0 {
		t.Errorf("busy: reclaims %d, idle for %v", n, r.idleFor)
	}
	// Idle (1% of one processor) for less than 15 s: nothing yet.
	for range 2 {
		s = sampleAt(s, ticks, 5, 2<<30)
		if n := r.next(s, interval); n != 0 {
			t.Errorf("idle for %v: reclaims %d", r.idleFor, n)
		}
	}
	// 15 s idle: a tenth of 2 GiB.
	s = sampleAt(s, ticks, 5, 2<<30)
	if n := r.next(s, interval); n != 2<<30/10 {
		t.Errorf("idle for 15 s: reclaims %d, want %d", n, 2<<30/10)
	}
	// Little page cache: at least 32 MiB at a time, but not below the
	// floor of 2% of 8 GiB.
	floor := uint64(8<<30) / 50
	s = sampleAt(s, ticks, 5, floor+100<<20)
	if n := r.next(s, interval); n != 32<<20 {
		t.Errorf("reclaims %d, want 32 MiB", n)
	}
	s = sampleAt(s, ticks, 5, floor+10<<20)
	if n := r.next(s, interval); n != 10<<20 {
		t.Errorf("reclaims %d, want what is above the floor", n)
	}
	s = sampleAt(s, ticks, 5, floor)
	if n := r.next(s, interval); n != 0 {
		t.Errorf("at the floor: reclaims %d", n)
	}
	// Exhausted: nothing more until the guest has been busy again.
	r.exhausted = true
	s = sampleAt(s, ticks, 5, 1<<30)
	if n := r.next(s, interval); n != 0 {
		t.Errorf("exhausted: reclaims %d", n)
	}
	s = sampleAt(s, ticks, 400, 1<<30)
	r.next(s, interval)
	if r.exhausted || r.idleFor != 0 {
		t.Error("being busy did not start a new round")
	}
	// A small guest's floor is 64 MiB.
	small := &reclaimer{}
	s = memSample{cpus: 1, memTotal: 512 << 20, file: 70 << 20}
	small.next(s, interval)
	s = memSample{busy: s.busy, total: s.total + 500, cpus: 1, memTotal: 512 << 20, file: 70 << 20}
	if n := small.next(s, interval); n != 6<<20 {
		t.Errorf("small guest reclaims %d, want 6 MiB", n)
	}
}

func TestRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	big := make([]byte, 20000)
	for i := range big {
		big[i] = byte(i)
	}
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for range 2 { // from the start each time
		got, err := read(f)
		if err != nil || len(got) != len(big) || got[19999] != big[19999] {
			t.Fatalf("read %d bytes, %v", len(got), err)
		}
	}
	if _, err := read(nil); err == nil {
		t.Error("read of a file that never opened succeeded")
	}
}
