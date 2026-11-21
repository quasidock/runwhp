package vm

import "sync"

// haltedReason is why a guest whose processors have all halted with
// interrupts disabled stopped.
const haltedReason = "guest halted with interrupts disabled"

// stopper decides when the guest stops. Any processor stops it at once by
// asking for a reset, by faulting, or by failing. A processor that halts
// with interrupts disabled is only parked: Linux parks the others that way
// before one of them resets the machine, and takes processors offline that
// way, so a single parked processor must not end the guest. Once every
// processor is parked, nothing but an INIT or an NMI could wake one, and the
// guest stops.
//
// With the local APIC emulated by the hypervisor, as runwhp runs it, WHP
// keeps halted processors to itself and has not been seen to report such a
// halt, so in practice a guest stops by resetting or failing.
//
// All methods are safe to call from any goroutine.
type stopper struct {
	mu      sync.Mutex
	parked  []bool
	nparked int
	done    chan struct{}

	stopped bool
	reset   bool
	reason  string
	err     error
}

func newStopper(cpus int) *stopper {
	return &stopper{parked: make([]bool, cpus), done: make(chan struct{})}
}

// Done is closed once the guest is to stop.
func (s *stopper) Done() <-chan struct{} { return s.done }

// isStopped reports whether the guest is to stop.
func (s *stopper) isStopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// stop ends the guest. Only the first call counts.
func (s *stopper) stop(reason string, reset bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(reason, reset, err)
}

func (s *stopper) stopLocked(reason string, reset bool, err error) {
	if s.stopped {
		return
	}
	s.stopped, s.reason, s.reset, s.err = true, reason, reset, err
	close(s.done)
}

// park records that processor cpu halted with interrupts disabled, and stops
// the guest if that leaves no processor running.
func (s *stopper) park(cpu int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.parked[cpu] {
		s.parked[cpu] = true
		s.nparked++
	}
	if s.nparked == len(s.parked) {
		s.stopLocked(haltedReason, false, nil)
	}
}

// unpark records that processor cpu runs again.
func (s *stopper) unpark(cpu int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parked[cpu] {
		s.parked[cpu] = false
		s.nparked--
	}
}

// outcome reports how the guest stopped.
func (s *stopper) outcome() (reason string, reset bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.reset, s.err
}
