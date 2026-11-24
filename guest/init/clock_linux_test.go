//go:build linux

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestClockAction(t *testing.T) {
	for _, c := range []struct {
		offset   time.Duration
		step, ok bool
	}{
		{0, false, false},
		{500 * time.Microsecond, false, false},
		{-500 * time.Microsecond, false, false},
		{5 * time.Millisecond, false, true},
		{-20 * time.Millisecond, false, true},
		{21 * time.Millisecond, true, true},
		{-200 * time.Millisecond, true, true},
		{600 * time.Millisecond, true, true},
		{time.Hour, true, true},
		{-time.Hour, true, true},
	} {
		if step, ok := clockAction(c.offset); step != c.step || ok != c.ok {
			t.Errorf("clockAction(%v) = step %v, act %v; want %v, %v", c.offset, step, ok, c.step, c.ok)
		}
	}
}

// TestWaitClock checks that the process waits for the host's first time,
// and starts without it once the wait is over.
func TestWaitClock(t *testing.T) {
	c := newController(false)
	if _, ok := c.waitClock(20 * time.Millisecond); ok {
		t.Fatal("the clock was set before any time came")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go c.listen(r)
	go func() {
		time.Sleep(30 * time.Millisecond)
		// The host's time as it is now, which leaves the clock alone.
		fmt.Fprintf(w, "{\"time\":%d}\n", time.Now().UnixNano())
		w.Close()
	}()
	took, ok := c.waitClock(5 * time.Second)
	if !ok || took < 20*time.Millisecond || took > 2*time.Second {
		t.Errorf("waited %v, set %v", took, ok)
	}
	if _, ok := c.waitClock(0); !ok {
		t.Error("a second wait did not return at once")
	}
}
