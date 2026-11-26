//go:build windows

package vmm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// fakeExecServer plays init's exec server over in-memory connections: the
// first connection is the control one, and each later one attaches a
// stream. serve runs the session once every stream is attached.
type fakeExecServer struct {
	t       *testing.T
	streams []string
	serve   func(ctl *json.Encoder, ctlIn *bufio.Reader, streams map[string]net.Conn)

	mu       sync.Mutex
	ctl      *json.Encoder
	ctlIn    *bufio.Reader
	attached map[string]net.Conn
	exec     *guestspec.Exec
}

func (f *fakeExecServer) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	host, guest := net.Pipe()
	go func() {
		r := bufio.NewReader(guest)
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var hello guestspec.ExecHello
		if err := json.Unmarshal(line, &hello); err != nil {
			f.t.Errorf("hello %q: %v", line, err)
			return
		}
		f.mu.Lock()
		if hello.Exec != nil {
			f.exec, f.ctl, f.ctlIn, f.attached = hello.Exec, json.NewEncoder(guest), r, map[string]net.Conn{}
			ctl := f.ctl
			f.mu.Unlock()
			if f.streams == nil {
				f.serve(ctl, r, nil)
				return
			}
			ctl.Encode(guestspec.ExecEvent{ID: "1", Streams: f.streams})
			return
		}
		if hello.Attach != "1" {
			f.t.Errorf("attach to %q", hello.Attach)
		}
		f.attached[hello.Stream] = &bufferedConn{guest, r}
		ready := len(f.attached) == len(f.streams)
		ctl, ctlIn, attached := f.ctl, f.ctlIn, f.attached
		f.mu.Unlock()
		if ready {
			f.serve(ctl, ctlIn, attached)
		}
	}()
	return host, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func TestExecSession(t *testing.T) {
	var gotSignal guestspec.Control
	f := &fakeExecServer{t: t, streams: []string{guestspec.PortStdout, guestspec.PortStderr, guestspec.PortStdin}}
	f.serve = func(ctl *json.Encoder, ctlIn *bufio.Reader, s map[string]net.Conn) {
		ctl.Encode(guestspec.ExecEvent{Started: true, Pid: 7})
		in, _ := io.ReadAll(s[guestspec.PortStdin])
		io.WriteString(s[guestspec.PortStdout], "out:"+string(in))
		io.WriteString(s[guestspec.PortStderr], "err")
		s[guestspec.PortStdout].Close()
		s[guestspec.PortStderr].Close()
		line, _ := ctlIn.ReadBytes('\n')
		json.Unmarshal(line, &gotSignal)
		ctl.Encode(guestspec.ExecEvent{Exited: true, Exit: 3})
	}
	var stdout, stderr bytes.Buffer
	sess, err := StartExec(context.Background(), f.dial, &guestspec.Exec{Args: []string{"cat"}, Stdin: true},
		ExecStreams{Stdin: strings.NewReader("hello"), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := sess.Started(); pid != 7 || err != nil {
		t.Errorf("started: pid %d, %v", pid, err)
	}
	if err := sess.Send(guestspec.Control{Signal: guestspec.SIGTERM}); err != nil {
		t.Fatal(err)
	}
	code, err := sess.Wait()
	if code != 3 || err != nil {
		t.Errorf("exit %d, %v", code, err)
	}
	if stdout.String() != "out:hello" || stderr.String() != "err" {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if gotSignal.Signal != guestspec.SIGTERM {
		t.Errorf("init got %+v", gotSignal)
	}
	if f.exec == nil || f.exec.Args[0] != "cat" {
		t.Errorf("init got exec %+v", f.exec)
	}
}

func TestExecSessionRefused(t *testing.T) {
	f := &fakeExecServer{t: t}
	f.serve = func(ctl *json.Encoder, _ *bufio.Reader, _ map[string]net.Conn) {
		ctl.Encode(guestspec.ExecEvent{Exited: true, Exit: guestspec.ExitNotFound, Error: "nothere: not found"})
	}
	sess, err := StartExec(context.Background(), f.dial, &guestspec.Exec{Args: []string{"nothere"}}, ExecStreams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Started(); err == nil {
		t.Error("a refused command started")
	}
	if code, err := sess.Wait(); code != guestspec.ExitNotFound || err == nil || err.Error() != "nothere: not found" {
		t.Errorf("exit %d, %v", code, err)
	}
}

func TestExecSessionDetach(t *testing.T) {
	// With no streams, the fake answers the hello itself.
	f := &fakeExecServer{t: t}
	f.serve = func(ctl *json.Encoder, _ *bufio.Reader, _ map[string]net.Conn) {
		ctl.Encode(guestspec.ExecEvent{ID: "1"})
		ctl.Encode(guestspec.ExecEvent{Started: true, Pid: 9})
		ctl.Encode(guestspec.ExecEvent{Exited: true, Exit: 5})
	}
	sess, err := StartExec(context.Background(), f.dial, &guestspec.Exec{Args: []string{"sleep", "9"}, Detach: true}, ExecStreams{})
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := sess.Started(); pid != 9 || err != nil {
		t.Errorf("started: pid %d, %v", pid, err)
	}
	// Whoever keeps the session open learns how the process ended.
	if code, err := sess.Wait(); code != 5 || err != nil {
		t.Errorf("a detached exec ended with %d, %v", code, err)
	}
}

func TestExecSessionGuestGone(t *testing.T) {
	f := &fakeExecServer{t: t}
	f.serve = func(ctl *json.Encoder, _ *bufio.Reader, _ map[string]net.Conn) {
		ctl.Encode(guestspec.ExecEvent{ID: "1"})
		ctl.Encode(guestspec.ExecEvent{Started: true, Pid: 4})
	}
	sess, err := StartExec(context.Background(), f.dial, &guestspec.Exec{Args: []string{"sh"}}, ExecStreams{})
	if err != nil {
		t.Fatal(err)
	}
	sess.Started()
	sess.Close() // as the guest going away closes the control connection
	if code, err := sess.Wait(); code != guestspec.ExitSignalBase+guestspec.SIGKILL || err == nil {
		t.Errorf("exit %d, %v", code, err)
	}
}
