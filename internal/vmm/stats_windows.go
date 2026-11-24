//go:build windows

package vmm

import (
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// guestStats asks init for the guest's statistics over the control port,
// one request at a time. Each request carries a number, which init's
// answer echoes, so an answer that arrives after its request gave up is
// not taken for the next one's.
type guestStats struct {
	listening func() bool                  // whether init reads the control port yet
	send      func(guestspec.Control) bool // queues a Control for init
	mu        sync.Mutex
	seq       uint64
	replies   chan *guestspec.GuestStats
}

func newGuestStats(proxy *SignalProxy) *guestStats {
	return &guestStats{listening: proxy.listening, send: proxy.queue.send, replies: make(chan *guestspec.GuestStats, 1)}
}

// deliver is eventWriter's onStats: it hands an answer over without
// blocking, and drops it if nothing waits for it.
func (g *guestStats) deliver(st *guestspec.GuestStats) {
	select {
	case g.replies <- st:
	default:
	}
}

// get asks init for the guest's statistics and waits up to timeout for
// them. It returns nil if init does not listen yet or does not answer.
func (g *guestStats) get(timeout time.Duration) *guestspec.GuestStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.listening() {
		return nil
	}
	select {
	case <-g.replies: // a late answer to an earlier request
	default:
	}
	g.seq++
	if !g.send(guestspec.Control{Stats: g.seq}) {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case st := <-g.replies:
			if st.Seq == g.seq {
				return st
			}
		case <-timer.C:
			return nil
		}
	}
}
