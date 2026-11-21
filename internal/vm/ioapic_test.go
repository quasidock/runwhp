package vm

import (
	"sync"
	"testing"
)

type delivery struct{ mode, destMode, dest, vector uint32 }

func testIOAPIC() (*ioapic, func() []delivery) {
	var mu sync.Mutex
	var got []delivery
	a := newIOAPIC(func(mode, destMode, dest, vector uint32) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, delivery{mode, destMode, dest, vector})
		return nil
	}, func(string, ...any) {})
	return a, func() []delivery {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

func program(a *ioapic, pin int, low, high uint32) {
	a.write(0x00, uint32(0x10+2*pin+1))
	a.write(0x10, high)
	a.write(0x00, uint32(0x10+2*pin))
	a.write(0x10, low)
}

// TestIOAPICRouting checks that each line goes where the guest programmed
// it: to the local APIC its destination names, in the mode it asked for.
func TestIOAPICRouting(t *testing.T) {
	a, deliveries := testIOAPIC()
	program(a, 5, 0x31, 3<<24)               // fixed, physical, CPU 3, vector 0x31
	program(a, 6, 0x41|1<<8|1<<11, 0x0c<<24) // lowest priority, logical, CPUs 2 and 3
	program(a, 7, 0x51|1<<16, 1<<24)         // masked
	a.pulse(5)
	a.pulse(6)
	a.pulse(7)
	want := []delivery{{0, 0, 3, 0x31}, {1, 1, 0x0c, 0x41}}
	got := deliveries()
	if len(got) != len(want) {
		t.Fatalf("deliveries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delivery %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Unmasking line 7, now aimed at CPU 2, delivers the edge it missed.
	program(a, 7, 0x51, 2<<24)
	if got := deliveries(); len(got) != 1 || got[0] != (delivery{0, 0, 2, 0x51}) {
		t.Errorf("after unmask: %+v", got)
	}
	a.pulse(7)
	if got := deliveries(); len(got) != 1 {
		t.Errorf("second edge: %+v", got)
	}
}

func TestIOAPICConcurrentPulses(t *testing.T) {
	a, deliveries := testIOAPIC()
	for pin := 5; pin < 13; pin++ {
		program(a, pin, uint32(0x30+pin), uint32(pin%4)<<24)
	}
	var wg sync.WaitGroup
	for pin := 5; pin < 13; pin++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				a.pulse(pin)
			}
		}()
	}
	wg.Wait()
	if n := len(deliveries()); n != 8000 || a.deliveredCount() != 8000 {
		t.Errorf("%d deliveries, %d counted; want 8000", n, a.deliveredCount())
	}
}
