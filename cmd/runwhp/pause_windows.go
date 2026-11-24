//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func newGuestFreezer(proxy *signalProxy) *guestFreezer {
	return &guestFreezer{listening: proxy.listening, send: proxy.queue.send, replies: make(chan *guestspec.Freeze, 1)}
}

// deliver is eventWriter's onFreeze.
func (g *guestFreezer) deliver(f *guestspec.Freeze) {
	select {
	case g.replies <- f:
	default:
	}
}

// freezeWait bounds how long the host waits for init's answer: longer than
// init waits for the kernel.
const freezeWait = 15 * time.Second

// set freezes or thaws the container's processes, and returns once init
// says they are.
func (g *guestFreezer) set(frozen bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.listening() {
		return errNotRunning
	}
	select {
	case <-g.replies:
	default:
	}
	g.seq++
	if !g.send(guestspec.Control{Freeze: &guestspec.Freeze{Seq: g.seq, Frozen: frozen}}) {
		return errNotRunning
	}
	timer := time.NewTimer(freezeWait)
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
			return fmt.Errorf("init did not answer within %v", freezeWait)
		}
	}
}

// pause freezes the container's processes, as docker pause does.
func (s *supervisor) pause() container.Reply {
	return s.setPaused(true)
}

// unpause thaws them.
func (s *supervisor) unpause() container.Reply {
	return s.setPaused(false)
}

func (s *supervisor) setPaused(paused bool) container.Reply {
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
func (s *supervisor) thawAfterSignal(b *vmBoot) {
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

const pauseUsage = `Usage: runwhp pause CONTAINER...

Pause containers: freeze every process in each, until runwhp unpause.
runwhp itself goes on serving the container: stats, logs, cp, diff and
export work, and kill and stop thaw it to deliver their signals.
`

const unpauseUsage = `Usage: runwhp unpause CONTAINER...

Unpause containers: thaw the processes runwhp pause froze.
`

func cmdPause(args []string, stdout io.Writer) int {
	return pauseCommand("pause", pauseUsage, container.OpPause, args, stdout)
}

func cmdUnpause(args []string, stdout io.Writer) int {
	return pauseCommand("unpause", unpauseUsage, container.OpUnpause, args, stdout)
}

func pauseCommand(name, usage, op string, args []string, stdout io.Writer) int {
	fs := newFlags(name, usage)
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	root, _ := storeRoot()
	code := 0
	for _, ref := range fs.Args() {
		c, err := store.Resolve(ref)
		if err == nil {
			err = askPause(root, c, op)
		}
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

// askPause asks the runwhp running c to pause or unpause it, and gives
// its answer as Docker words it.
func askPause(root string, c *container.Container, op string) error {
	notRunning := fmt.Errorf("container %s is not running", c.ID)
	if op == container.OpUnpause {
		notRunning = fmt.Errorf("Container %s is not paused", c.ID)
	}
	if !c.State.Active() {
		return notRunning
	}
	ctx, cancel := context.WithTimeout(context.Background(), freezeWait+5*time.Second)
	defer cancel()
	rep, err := container.Ask(ctx, root, c.ID, container.Request{Op: op})
	switch {
	case errors.Is(err, container.ErrNoRunwhp):
		return notRunning
	case err != nil:
		return fmt.Errorf("container %s: %w", c.Short(), err)
	case !rep.OK:
		return errors.New(rep.Error)
	}
	return nil
}
