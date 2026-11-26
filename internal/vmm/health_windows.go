package vmm

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// A healthProbe runs a healthcheck's command in the container, giving up
// after timeout, and returns how it went.
type healthProbe func(ctx context.Context, args []string, timeout time.Duration) container.HealthResult

// healthLoop runs the container's healthcheck for as long as boot b's
// process runs, as Docker's daemon runs it: the first check an interval
// after the process starts, the start interval apart while the container
// is starting within its start period, the interval apart after that, and
// none while it is paused. Each result goes into the container's state.
func (s *Supervisor) healthLoop(b *vmBoot, hc *container.Healthcheck, probe healthProbe) {
	s.mu.Lock()
	started := b.startedAt
	s.mu.Unlock()
	s.update(func(st *container.State) {
		if s.cur == b {
			st.Health = container.StartHealth(st.Health)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-b.exited
		cancel()
	}()
	for {
		s.mu.Lock()
		h := s.state.Health
		s.mu.Unlock()
		t := time.NewTimer(container.NextHealthInterval(h, hc, time.Since(started)))
		select {
		case <-b.exited:
			t.Stop()
			return
		case <-t.C:
		}
		s.mu.Lock()
		paused := s.state.Paused
		s.mu.Unlock()
		if paused {
			continue
		}
		start := time.Now()
		r := probe(ctx, hc.Args(), hc.TimeoutOrDefault())
		if ctx.Err() != nil {
			return
		}
		inStart := start.Sub(started) < hc.StartPeriod
		s.update(func(st *container.State) {
			// The process may have ended meanwhile.
			if s.cur == b && st.Status == container.Running {
				st.Health = container.NextHealth(st.Health, hc, r, inStart)
			}
		})
	}
}

// execProbe runs a check through init's exec server, as runwhp exec runs
// a command, with the container's environment, user and directory, its
// output and errors kept together, as much as Docker keeps.
func (s *Supervisor) execProbe(ctx context.Context, args []string, timeout time.Duration) container.HealthResult {
	r := container.HealthResult{Start: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &healthOutput{}
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) { return s.DialGuest(ctx, guestspec.ExecPort) }
	sess, err := StartExec(ctx, dial, &guestspec.Exec{Args: args}, ExecStreams{Stdout: out, Stderr: out})
	if err != nil {
		r.End, r.ExitCode, r.Output = time.Now(), -1, err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			r.Output = timedOut(timeout, "")
		}
		return r
	}
	defer sess.Close()
	select {
	case <-sess.Done():
		code, err := sess.Wait()
		r.End, r.ExitCode, r.Output = time.Now(), code, out.String()
		if err != nil {
			// init could not run the command.
			if r.ExitCode == 0 {
				r.ExitCode = -1
			}
			if r.Output == "" {
				r.Output = err.Error()
			}
		}
	case <-ctx.Done():
		sess.Send(guestspec.Control{Signal: guestspec.SIGKILL})
		r.End, r.ExitCode, r.Output = time.Now(), -1, timedOut(timeout, out.String())
	}
	return r
}

// timedOut is Docker's output for a check that ran out of time, with what
// it wrote until then.
func timedOut(timeout time.Duration, output string) string {
	msg := fmt.Sprintf("Health check exceeded timeout (%v)", timeout)
	if output != "" {
		msg += ": " + output
	}
	return msg
}

// healthOutput keeps the first bytes a check writes, as many as its result
// keeps, and whether there were more.
type healthOutput struct {
	mu  sync.Mutex
	buf []byte
}

func (o *healthOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if room := container.HealthOutputSize + 1 - len(o.buf); room > 0 {
		o.buf = append(o.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (o *healthOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return container.TruncateHealthOutput(o.buf)
}
