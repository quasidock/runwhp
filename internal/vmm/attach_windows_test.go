//go:build windows

package vmm

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

// frameSink collects what an attach client's writer sends, decoded.
type frameSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	block  chan struct{} // if set, writes wait on it
	failed bool
}

func (s *frameSink) Write(p []byte) (int, error) {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return 0, io.ErrClosedPipe
	}
	return s.buf.Write(p)
}

func (s *frameSink) frames(t *testing.T) (stdout, stderr string, exit []byte) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := bytes.NewReader(s.buf.Bytes())
	var out, errs strings.Builder
	for {
		kind, p, err := container.ReadFrame(r)
		if err == io.EOF {
			return out.String(), errs.String(), exit
		}
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case container.AttachStdout:
			out.Write(p)
		case container.AttachStderr:
			errs.Write(p)
		case container.AttachExit:
			exit = p
		}
	}
}

// TestOutputHub checks that output goes where it goes and to every attached
// client, in order, that a client attached later gets what follows, and
// that the exit frame comes last.
func TestOutputHub(t *testing.T) {
	h := newOutputHub()
	var log bytes.Buffer
	out, errs := h.wrap(container.Stdout, &log), h.wrap(container.Stderr, nil)
	a, b := &frameSink{}, &frameSink{}
	ca := newAttachClient(a)
	h.add(ca)
	go ca.write()
	out.Write([]byte("one "))
	cb := newAttachClient(b)
	h.add(cb)
	go cb.write()
	out.Write([]byte("two"))
	errs.Write([]byte("oops"))
	for _, c := range []*attachClient{ca, cb} {
		c.finish(container.AppendFrame(nil, container.AttachExit, []byte(`{"ok":true}`)))
		<-c.done
	}
	if log.String() != "one two" {
		t.Errorf("the log got %q", log.String())
	}
	for name, want := range map[*frameSink]string{a: "one two", b: "two"} {
		stdout, stderr, exit := name.frames(t)
		if stdout != want || stderr != "oops" || string(exit) != `{"ok":true}` {
			t.Errorf("client got stdout %q, stderr %q, exit %q; want %q", stdout, stderr, exit, want)
		}
	}
	// Finished clients take no more.
	out.Write([]byte("three"))
	if stdout, _, _ := a.frames(t); stdout != "one two" {
		t.Errorf("after the exit: %q", stdout)
	}
}

// slowSink takes a write only every delay.
type slowSink struct {
	frameSink
	delay time.Duration
}

func (s *slowSink) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.frameSink.Write(p)
}

// TestOutputHubSlow checks that a client reading slowly gets every byte of
// output far beyond attachBacklog, the output waiting for it, as Docker's
// waits for its attached clients, while another client and the log get
// everything too.
func TestOutputHubSlow(t *testing.T) {
	h := newOutputHub()
	var log bytes.Buffer
	out := h.wrap(container.Stdout, &log)
	slow, fine := &slowSink{delay: time.Millisecond}, &frameSink{}
	cs, cf := newAttachClient(slow), newAttachClient(fine)
	h.add(cs)
	h.add(cf)
	go cs.write()
	go cf.write()
	chunk := bytes.Repeat([]byte("x"), 16<<10)
	total := 64 << 20
	for range total / len(chunk) {
		out.Write(chunk)
		cs.mu.Lock()
		queued := cs.queued
		cs.mu.Unlock()
		if queued > attachBacklog+len(chunk)+64 {
			t.Fatalf("%d bytes queued for the slow client", queued)
		}
	}
	for _, c := range []*attachClient{cs, cf} {
		c.finish(container.AppendFrame(nil, container.AttachExit, []byte("{}")))
		<-c.done
	}
	if log.Len() != total {
		t.Errorf("the log got %d bytes", log.Len())
	}
	for name, s := range map[string]*frameSink{"slow": &slow.frameSink, "other": fine} {
		if stdout, _, exit := s.frames(t); len(stdout) != total || string(exit) != "{}" {
			t.Errorf("the %s client got %d bytes, exit %q", name, len(stdout), exit)
		}
	}
}

// TestOutputHubWaitEnds checks that output waiting for a client that does
// not read is let go when it detaches, when its connection fails, and when
// the guest stops; and that runwhps attach meanwhile.
func TestOutputHubWaitEnds(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), attachBacklog)
	for _, end := range []string{"detach", "fail", "release"} {
		t.Run(end, func(t *testing.T) {
			h := newOutputHub()
			out := h.wrap(container.Stdout, io.Discard)
			stuck := &frameSink{block: make(chan struct{})}
			c := newAttachClient(stuck)
			h.add(c)
			go c.write()
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				for range 4 {
					out.Write(chunk)
				}
			}()
			select {
			case <-wrote:
				t.Fatal("the output did not wait for a client that does not read")
			case <-time.After(100 * time.Millisecond):
			}
			// The wait holds no lock: another runwhp attaches.
			added := make(chan struct{})
			go func() {
				h.add(newAttachClient(io.Discard))
				close(added)
			}()
			select {
			case <-added:
			case <-time.After(5 * time.Second):
				t.Fatal("attaching waited for the stuck client")
			}
			h.mu.Lock()
			for o := range h.clients {
				if o != c {
					delete(h.clients, o)
				}
			}
			h.mu.Unlock()
			switch end {
			case "detach":
				c.close()
				close(stuck.block)
			case "fail":
				stuck.mu.Lock()
				stuck.failed = true
				stuck.mu.Unlock()
				close(stuck.block)
			case "release":
				h.release()
			}
			select {
			case <-wrote:
			case <-time.After(10 * time.Second):
				t.Fatalf("the output still waits after %s", end)
			}
			if end == "release" {
				close(stuck.block)
				h.hold() // a next boot waits again
				if !h.free && h.released == nil {
					t.Error("hold left no released channel")
				}
			}
		})
	}
}

// TestOutputHubDetach checks that a detached client is written nothing
// more and leaves the others be.
func TestOutputHubDetach(t *testing.T) {
	h := newOutputHub()
	out := h.wrap(container.Stdout, io.Discard)
	s := &frameSink{}
	c := newAttachClient(s)
	h.add(c)
	go c.write()
	out.Write([]byte("seen"))
	time.Sleep(50 * time.Millisecond)
	c.close()
	<-c.done
	h.remove(c)
	out.Write([]byte("unseen"))
	if stdout, _, exit := s.frames(t); stdout != "seen" || exit != nil {
		t.Errorf("detached client got %q, exit %q", stdout, exit)
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the input did not end")
		return ""
	}
}

// TestAttachStdin checks the container's input as attached runwhps send it:
// in order, and ending with the first runwhp's end only when it takes input
// once.
func TestAttachStdin(t *testing.T) {
	once := newAttachStdin(true)
	once.push([]byte("a"))
	once.push([]byte("b"))
	once.endInput()
	once.push([]byte("ignored")) // after the end
	if got := readAll(t, once.view(nil)); got != "ab" {
		t.Errorf("stdin-once read %q", got)
	}

	open := newAttachStdin(false)
	open.push([]byte("first "))
	open.endInput() // one attached runwhp's input ended; the container's goes on
	open.push([]byte("second"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		open.close() // the container stopped
	}()
	if got := readAll(t, open.view(nil)); got != "first second" {
		t.Errorf("open stdin read %q", got)
	}
}

// TestAttachStdinViews checks the input of a container that boots again:
// a boot's view ends with it, and leaves what it has not read for the next.
func TestAttachStdinViews(t *testing.T) {
	in := newAttachStdin(false)
	done1 := make(chan struct{})
	first := in.view(done1)
	in.push([]byte("one"))
	in.push([]byte("two"))
	buf := make([]byte, 2)
	if n, err := first.Read(buf); string(buf[:n]) != "on" || err != nil {
		t.Fatalf("first boot read %q, %v", buf[:n], err)
	}
	close(done1)
	if n, err := first.Read(buf); n != 0 || err != io.EOF {
		t.Errorf("first boot after its end: %d, %v", n, err)
	}
	second := in.view(make(chan struct{}))
	in.push([]byte("three"))
	in.close()
	if got := readAll(t, second); got != "etwothree" {
		t.Errorf("second boot read %q", got)
	}
	if n, err := in.view(nil).Read(buf); n != 0 || err != io.EOF {
		t.Errorf("after the end: %d, %v", n, err)
	}
}

// TestControlQueueDone checks that the console's reading of the control
// port ends with the boot it is for.
func TestControlQueueDone(t *testing.T) {
	q := newControlQueue()
	done := make(chan struct{})
	q.done = done
	got := make(chan error, 1)
	go func() {
		_, err := q.Read(make([]byte, 16))
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(done)
	select {
	case err := <-got:
		if err != io.EOF {
			t.Errorf("read %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still reading after the boot ended")
	}
}

// chunks is a reader returning one string per read.
type chunks []string

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}
