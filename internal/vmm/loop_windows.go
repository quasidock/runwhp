//go:build windows

package vmm

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"golang.org/x/sys/windows"
)

// NewSignalProxy makes a container's signal proxy: stop stops its VM, and
// logf takes what the proxy does with --debug.
func NewSignalProxy(stop context.CancelFunc, logf func(string, ...any)) *SignalProxy {
	return &SignalProxy{stop: stop, queue: newControlQueue(), logf: logf}
}

// NewConsoleReader reads keyboard input from the console h; lines says the
// console edits lines itself, without -t.
func NewConsoleReader(h windows.Handle, lines bool) *ConsoleReader {
	return &ConsoleReader{h: h, lines: lines}
}

// PrintConsole prints the last kernel console lines a failed boot left.
func PrintConsole(w io.Writer, lines []string) {
	(&ring{lines: lines}).print(w)
}

// NewLayerServers serves the layers save rebuilds, in order, booting a
// guest for each run of them as it comes to it.
func NewLayerServers(ctx context.Context, root string, layers *layer.Store, order []string, debug bool) *LayerServers {
	return &LayerServers{ctx: ctx, root: root, layers: layers, order: order, debug: debug}
}

// Report is told how the first boot of a container run in the background
// went, once: it is ready, running, or waiting to start again, or it
// failed.
type Report interface {
	OK(status container.Status)
	Fail(err error, code int, console []string)
	// Done says the report was made.
	Done() bool
}

// Background says how RunBackground runs a container.
type Background struct {
	// IO is where the container's streams go. With OpenStdin its input is
	// attached clients' instead, and with StdinOnce it ends with the first
	// of them to end its own.
	IO                   BootIO
	OpenStdin, StdinOnce bool
	// Files are closed before the container is removed.
	Files []io.Closer
	// WaitStart boots the container without starting its process, until
	// it is started.
	WaitStart bool
	// HandedOver says another runwhp handed the container over, to start
	// it again after RestartAfter.
	HandedOver   bool
	RestartAfter time.Duration
	Backoff      container.Backoff
	Report       Report
	Logf         func(string, ...any)
}

// RunBackground runs the container in this process, and runs it again as
// its restart policy says, each time in a VM of its own on its writable
// layer, until it stops for good.
func (s *Supervisor) RunBackground(o Background) {
	cfg := s.rec.Config
	s.files = o.Files
	s.streams(&o.IO, o.OpenStdin, o.StdinOnce)
	s.policy = cfg.Restart
	s.backoff = o.Backoff
	if o.HandedOver {
		// The runwhp that handed the container over exits once this one
		// has it, and the restart's delay goes on here.
		fmt.Fprintf(Diag, "%s taking the container over to start it again in %v, as --restart %s says (restart %d)\n",
			time.Now().Format(time.RFC3339), o.RestartAfter, cfg.Restart, s.restarts)
		s.backoff.Continue(o.RestartAfter)
		o.Report.OK(container.Restarting)
		if !s.waitRestart(o.RestartAfter) {
			s.finish()
			return
		}
	}

	again := false // booting a created container again, to wait for its start
	for n := 0; ; n++ {
		ctx, cancel := context.WithCancel(context.Background())
		proxy := NewSignalProxy(cancel, o.Logf)
		b := s.beginBoot(proxy)
		if b == nil {
			cancel()
			s.stoppedRestarting()
			break
		}
		bb := o.IO
		switch {
		case s.stdin != nil:
			bb.Stdin = s.stdin.view(b.exited)
		case n > 0:
			bb.Stdin = nil // a file or pipe of input goes to the first run only
		}
		hooks := s.hooks(b, o.WaitStart && n == 0 || again)
		if n == 0 {
			// The runwhp that started this one waits to hear how the first
			// boot went.
			if o.WaitStart {
				hooks.ready = func() {
					s.isReady(b)
					o.Report.OK(container.Created)
				}
			} else {
				hooks.started = func(pid int) {
					s.started(b, pid)
					s.persist()
					o.Report.OK(container.Running)
				}
			}
		}
		bootCfg := s.rec.Config
		layer := StartLayer(s.rec.RW(), bootCfg.StorageSize, bootCfg.AutoRemove)
		code, err := bootContainer(ctx, s.root, bootCfg, layer, guestOpts{}, image.PullNever, proxy, bb, hooks, o.Logf, time.Now())
		layer.Close()
		cancel()
		if again = s.reprepared(b); again {
			continue
		}
		if err != nil {
			errorf("%v", err)
		}
		if !o.Report.Done() {
			e := err
			if e == nil {
				e = fmt.Errorf("the container exited with %d before it was ready", code)
			}
			s.mu.Lock()
			console := b.console
			s.mu.Unlock()
			o.Report.Fail(e, code, console)
		}
		restart, delay := s.endBoot(b, code, err)
		if !restart {
			break
		}
		st := s.reply()
		s.mu.Lock()
		count := s.restarts
		s.mu.Unlock()
		fmt.Fprintf(Diag, "%s container exited with %d; starting it again in %v, as --restart %s says (restart %d)\n",
			time.Now().Format(time.RFC3339), st.ExitCode, delay, cfg.Restart, count)
		if cfg.Debug {
			fmt.Fprintf(Diag, "[vmm] %d goroutines\n", runtime.NumGoroutine())
		}
		if !s.waitRestart(delay) {
			break
		}
	}
	s.finish()
}

// Foreground says how RunForeground runs a container.
type Foreground struct {
	// Layer is the writable layer, being opened; RunForeground closes it.
	Layer   *PendingLayer
	Policy  image.Policy
	Proxy   *SignalProxy
	IO      BootIO
	Backoff container.Backoff
	Logf    func(string, ...any)
	Start   time.Time
	// BeforeHandOff, if set, is called before the container is handed
	// over to start again; HandOff starts the runwhp it goes to.
	BeforeHandOff func()
	HandOff       HandOffFunc
}

// RunForeground runs the container once in this process, with its input
// this process's, and returns its exit code. If its restart policy says to
// start it again, a runwhp in the background that o.HandOff starts takes it
// over to, and this one returns how it ended.
func (s *Supervisor) RunForeground(ctx context.Context, o Foreground) (int, error) {
	s.streams(&o.IO, false, false) // its input stays this console's
	s.policy = s.rec.Config.Restart
	s.backoff = o.Backoff
	b := s.beginBoot(o.Proxy)
	code, err := bootContainer(ctx, s.root, s.rec.Config, o.Layer, guestOpts{}, o.Policy, o.Proxy, o.IO, s.hooks(b, false), o.Logf, o.Start)
	o.Layer.Close()
	if restart, delay := s.endBoot(b, code, err); restart {
		// As Docker's daemon would, a runwhp in the background starts it
		// again, and this one exits with how it ended.
		if o.BeforeHandOff != nil {
			o.BeforeHandOff()
		}
		herr := s.handOff(ctx, delay, o.HandOff)
		if herr == nil {
			return code, err
		}
		errorf("could not hand container %s over to start it again: %v", s.rec.Short(), herr)
		s.stoppedRestarting()
	}
	s.finish()
	return code, err
}
