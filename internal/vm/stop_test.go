package vm

import (
	"errors"
	"sync"
	"testing"
)

func TestStopFirstWins(t *testing.T) {
	s := newStopper(2)
	if s.isStopped() {
		t.Fatal("stopped before anything happened")
	}
	s.stop("guest requested reset", true, nil)
	s.stop("triple fault", false, errors.New("late"))
	s.park(0)
	s.park(1)
	if !s.isStopped() {
		t.Fatal("not stopped")
	}
	reason, reset, err := s.outcome()
	if reason != "guest requested reset" || !reset || err != nil {
		t.Errorf("outcome = %q, %v, %v", reason, reset, err)
	}
}

// TestStopParked follows a Linux reboot: CPU 0 parks the others, which halt
// with interrupts disabled, and then resets the machine.
func TestStopParked(t *testing.T) {
	s := newStopper(4)
	for cpu := 1; cpu < 4; cpu++ {
		s.park(cpu)
		s.park(cpu) // a second halt exit on a parked CPU counts once
	}
	if s.isStopped() {
		t.Fatal("stopped while CPU 0 still runs")
	}
	s.stop("guest requested reset", true, nil)
	if reason, reset, _ := s.outcome(); !reset || reason != "guest requested reset" {
		t.Errorf("outcome = %q, %v", reason, reset)
	}
}

func TestStopAllParked(t *testing.T) {
	for _, n := range []int{1, 2, 16} {
		s := newStopper(n)
		for cpu := 0; cpu < n-1; cpu++ {
			s.park(cpu)
		}
		if s.isStopped() {
			t.Fatalf("%d CPUs: stopped with one still running", n)
		}
		s.park(n - 1)
		if reason, reset, err := s.outcome(); !s.isStopped() || reason != haltedReason || reset || err != nil {
			t.Errorf("%d CPUs: outcome = %q, %v, %v", n, reason, reset, err)
		}
	}
}

func TestStopUnpark(t *testing.T) {
	s := newStopper(2)
	s.park(1)
	s.unpark(1)
	s.unpark(1) // unparking a running CPU changes nothing
	s.park(0)
	if s.isStopped() {
		t.Fatal("stopped although CPU 1 was woken")
	}
	s.park(1)
	if !s.isStopped() {
		t.Fatal("not stopped with both CPUs parked")
	}
}

func TestStopConcurrent(t *testing.T) {
	const n = 16
	s := newStopper(n)
	var wg sync.WaitGroup
	for cpu := 0; cpu < n; cpu++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				s.park(cpu)
				s.unpark(cpu)
			}
			s.park(cpu)
		}()
	}
	wg.Wait()
	select {
	case <-s.Done():
	default:
		t.Fatal("not stopped with every CPU parked")
	}
	if reason, _, _ := s.outcome(); reason != haltedReason {
		t.Errorf("reason = %q", reason)
	}
}
