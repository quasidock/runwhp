//go:build linux

package main

import (
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
