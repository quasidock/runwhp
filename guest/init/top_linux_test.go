//go:build linux

package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0)
	// pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime vsize rss
	line := "812 (a (b) c) S 1 812 700 34816 812 4194560 120 0 0 0 250 50 0 0 20 -5 3 0 4200 1048576 10 18446744073709551615\n"
	p, ok := parseProcStat([]byte(line), boot)
	if !ok {
		t.Fatal("not parsed")
	}
	if p.Comm != "a (b) c" || p.State != "S" || p.PPID != 1 || p.PGID != 812 || p.SID != 700 || p.TTY != 34816 || p.TPGID != 812 {
		t.Errorf("parsed %+v", p)
	}
	if p.CPU != 3*time.Second || p.Nice != -5 || p.Threads != 3 || !p.Start.Equal(boot.Add(42*time.Second)) {
		t.Errorf("cpu %v, nice %d, threads %d, start %v", p.CPU, p.Nice, p.Threads, p.Start.Sub(boot))
	}
	if p.VSZ != 1<<20 || p.RSS != 10*uint64(os.Getpagesize()) {
		t.Errorf("vsz %d, rss %d", p.VSZ, p.RSS)
	}
	if _, ok := parseProcStat([]byte("812 (sh) S 1"), boot); ok {
		t.Error("parsed a truncated line")
	}
	if uid := statusUID([]byte("Name:\tsh\nUid:\t1000\t1000\t1000\t1000\n")); uid != 1000 {
		t.Errorf("uid %d", uid)
	}
	if args := splitCmdline([]byte("sh\x00-c\x00echo hi\x00")); !slices.Equal(args, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("args %q", args)
	}
	if splitCmdline(nil) != nil {
		t.Error("args of a process without any")
	}
}

// TestProcesses lists the processes of the machine the test runs on: the
// test itself among them, with its arguments.
func TestProcesses(t *testing.T) {
	proc, err := os.Open("/proc")
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	stat, err := os.Open("/proc/stat")
	if err != nil {
		t.Fatal(err)
	}
	defer stat.Close()
	m := &memFiles{proc: proc, stat: stat}
	ps, err := m.processes()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	for _, p := range ps {
		if p.PID == self {
			if len(p.Args) == 0 || !strings.HasSuffix(p.Args[0], ".test") || p.Start.After(time.Now()) || p.Threads < 1 {
				t.Errorf("the test: %+v", p)
			}
			return
		}
	}
	if self > 2 {
		t.Errorf("the test, pid %d, is not among %d processes", self, len(ps))
	}
}
