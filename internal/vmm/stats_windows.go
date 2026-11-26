//go:build windows

package vmm

import (
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// guestQuery asks init something over the control port, one request at a
// time: the guest's statistics, or the container's processes. Each
// request carries a number, which init's answer echoes, so an answer that
// arrives after its request gave up is not taken for the next one's.
type guestQuery[T any] struct {
	listening func() bool                  // whether init reads the control port yet
	send      func(guestspec.Control) bool // queues a Control for init
	ask       func(seq uint64) guestspec.Control
	seqOf     func(T) uint64
	mu        sync.Mutex
	seq       uint64
	replies   chan T
}

func newGuestQuery[T any](proxy *SignalProxy, ask func(uint64) guestspec.Control, seqOf func(T) uint64) *guestQuery[T] {
	return &guestQuery[T]{listening: proxy.listening, send: proxy.queue.send, ask: ask, seqOf: seqOf, replies: make(chan T, 1)}
}

// guestStats asks init for the guest's statistics.
type guestStats = guestQuery[*guestspec.GuestStats]

func newGuestStats(proxy *SignalProxy) *guestStats {
	return newGuestQuery(proxy, func(seq uint64) guestspec.Control { return guestspec.Control{Stats: seq} },
		func(st *guestspec.GuestStats) uint64 { return st.Seq })
}

// guestTop asks init for the container's processes.
type guestTop = guestQuery[*guestspec.Processes]

func newGuestTop(proxy *SignalProxy) *guestTop {
	return newGuestQuery(proxy, func(seq uint64) guestspec.Control { return guestspec.Control{Top: seq} },
		func(p *guestspec.Processes) uint64 { return p.Seq })
}

// deliver is eventWriter's onStats or onTop: it hands an answer over
// without blocking, and drops it if nothing waits for it.
func (g *guestQuery[T]) deliver(v T) {
	select {
	case g.replies <- v:
	default:
	}
}

// get asks init and waits up to timeout for its answer. It returns the
// zero T, nil, if init does not listen yet or does not answer.
func (g *guestQuery[T]) get(timeout time.Duration) T {
	var none T
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.listening() {
		return none
	}
	select {
	case <-g.replies: // a late answer to an earlier request
	default:
	}
	g.seq++
	if !g.send(g.ask(g.seq)) {
		return none
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case v := <-g.replies:
			if g.seqOf(v) == g.seq {
				return v
			}
		case <-timer.C:
			return none
		}
	}
}
