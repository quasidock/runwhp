//go:build windows

package vmm

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// guestFreezer asks init to freeze or thaw the container's processes over
// the control port, one request at a time, as guestStats asks for
// statistics.
type guestFreezer struct {
	listening func() bool
	send      func(guestspec.Control) bool
	mu        sync.Mutex
	seq       uint64
	replies   chan *guestspec.Freeze
}

func newGuestFreezer(proxy *SignalProxy) *guestFreezer {
	return &guestFreezer{listening: proxy.listening, send: proxy.queue.send, replies: make(chan *guestspec.Freeze, 1)}
}

// deliver is eventWriter's onFreeze.
func (g *guestFreezer) deliver(f *guestspec.Freeze) {
	select {
	case g.replies <- f:
	default:
	}
}

// FreezeWait bounds how long the host waits for init's answer: longer than
// init waits for the kernel.
const FreezeWait = 15 * time.Second

// set freezes or thaws the container's processes, and returns once init
// says they are.
func (g *guestFreezer) set(frozen bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.listening() {
		return ErrNotRunning
	}
	select {
	case <-g.replies:
	default:
	}
	g.seq++
	if !g.send(guestspec.Control{Freeze: &guestspec.Freeze{Seq: g.seq, Frozen: frozen}}) {
		return ErrNotRunning
	}
	timer := time.NewTimer(FreezeWait)
	defer timer.Stop()
	for {
		select {
		case f := <-g.replies:
			if f.Seq != g.seq {
				continue
			}
			if f.Error != "" {
				return errors.New(f.Error)
			}
			return nil
		case <-timer.C:
			return fmt.Errorf("init did not answer within %v", FreezeWait)
		}
	}
}

// Pause freezes the container's processes, as docker Pause does.
func (s *Supervisor) Pause() container.Reply {
	return s.setPaused(true)
}

// Unpause thaws them.
func (s *Supervisor) Unpause() container.Reply {
	return s.setPaused(false)
}

func (s *Supervisor) setPaused(paused bool) container.Reply {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.mu.Lock()
	running := s.state.Status == container.Running && s.cur != nil && s.cur.vm != nil
	was := s.state.Paused
	var h *vmHandles
	if running {
		h = s.cur.vm
	}
	s.mu.Unlock()
	// Docker's words.
	switch {
	case paused && !running:
		return container.Reply{Error: fmt.Sprintf("container %s is not running", s.rec.ID)}
	case paused && was:
		return container.Reply{Error: fmt.Sprintf("container %s is already paused", s.rec.ID)}
	case !paused && (!running || !was):
		return container.Reply{Error: fmt.Sprintf("Container %s is not paused", s.rec.ID)}
	}
	if err := h.freezer.set(paused); err != nil {
		what := "pausing"
		if !paused {
			what = "unpausing"
		}
		return container.Reply{Error: fmt.Sprintf("%s container %s: %v", what, s.rec.ID, err)}
	}
	s.update(func(st *container.State) { st.Paused = paused })
	return s.reply()
}

// thawAfterSignal thaws a paused container once a signal has been sent
// to it, so that its processes take it, as docker kill does: a frozen
// process acts on a signal only once thawed.
func (s *Supervisor) thawAfterSignal(b *vmBoot) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.mu.Lock()
	paused, h := s.state.Paused && s.cur == b, b.vm
	s.mu.Unlock()
	if !paused || h == nil {
		return
	}
	if err := h.freezer.set(false); err != nil {
		return
	}
	s.update(func(st *container.State) { st.Paused = false })
}
