//go:build linux

package main

import (
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	line := "812 (a (b) c) S 1 812 812 0 -1 4194560 120 0 0 0 0 0 0 0 20 0 3 0 4242 1024 10 18446744073709551615\n"
	if ppid, threads, ok := parseStat([]byte(line)); !ok || ppid != 1 || threads != 3 {
		t.Errorf("parseStat = %d, %d, %v", ppid, threads, ok)
	}
	if _, _, ok := parseStat([]byte("812 (sh) S 1")); ok {
		t.Error("parsed a truncated line")
	}
}

// TestTasks counts the tasks of the machine the test runs on: at least the
// test's own threads, read twice through the same /proc.
func TestTasks(t *testing.T) {
	proc, err := os.Open("/proc")
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	m := &memFiles{proc: proc}
	for range 2 {
		if n := m.tasks(); n < 2 {
			t.Errorf("tasks = %d; the test alone has more threads", n)
		}
	}
	if (&memFiles{}).tasks() != 0 {
		t.Error("tasks without /proc")
	}
}
