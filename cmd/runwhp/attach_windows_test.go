//go:build windows

package main

import (
	"bytes"
	"errors"
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

// TestOutputHubBehind checks that a client that stops reading is cut off
// once attachBacklog is queued for it, while the output and another client
// go on unhindered.
func TestOutputHubBehind(t *testing.T) {
	h := newOutputHub()
	var log bytes.Buffer
	out := h.wrap(container.Stdout, &log)
	stuck := &frameSink{block: make(chan struct{})}
	fine := &frameSink{}
	cs, cf := newAttachClient(stuck), newAttachClient(fine)
	h.add(cs)
	h.add(cf)
	go cs.write()
	go cf.write()
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	written := make(chan struct{})
	go func() {
		for range 2 * attachBacklog / len(chunk) {
			out.Write(chunk)
		}
		close(written)
	}()
	select {
	case <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("the output waited for a client that does not read")
	}
	if log.Len() != 2*attachBacklog {
		t.Errorf("the log got %d bytes", log.Len())
	}
	h.mu.Lock()
	_, stillThere := h.clients[cs]
	h.mu.Unlock()
	if stillThere {
		t.Error("the stuck client was not cut off")
	}
	close(stuck.block)
	<-cs.done
	if _, stderr, _ := stuck.frames(t); stderr != behindMessage {
		t.Errorf("the stuck client was told %q", stderr)
	}
	cf.finish(container.AppendFrame(nil, container.AttachExit, []byte("{}")))
	<-cf.done
	if stdout, _, _ := fine.frames(t); len(stdout) != 2*attachBacklog {
		t.Errorf("the other client got %d bytes", len(stdout))
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

func TestDetachFilter(t *testing.T) {
	keys, _ := parseDetachKeys(defaultDetachKeys)
	for _, c := range []struct {
		in       []string
		want     string
		detached bool
	}{
		{[]string{"ab", "c"}, "abc", false},
		{[]string{"ab\x10\x11cd"}, "ab", true},
		{[]string{"ab\x10", "\x11"}, "ab", true},   // split across reads
		{[]string{"a\x10b"}, "a\x10b", false},      // Ctrl+P alone passes through
		{[]string{"a\x10\x10\x11"}, "a\x10", true}, // Ctrl+P twice, then Ctrl+Q
		{[]string{"a\x10"}, "a\x10", false},        // a held Ctrl+P at the end of input
	} {
		in := chunks(c.in)
		f := &detachFilter{r: &in, keys: keys}
		var got []byte
		var err error
		buf := make([]byte, 3)
		for {
			var n int
			n, err = f.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		if string(got) != c.want || errors.Is(err, errDetached) != c.detached {
			t.Errorf("%q: read %q, %v; want %q, detached %v", c.in, got, err, c.want, c.detached)
		}
	}
}

func TestParseDetachKeys(t *testing.T) {
	for in, want := range map[string]string{
		"ctrl-p,ctrl-q": "\x10\x11",
		"ctrl-a":        "\x01",
		"CTRL-Z,x":      "\x1ax",
		"ctrl-@,ctrl-_": "\x00\x1f",
		"ctrl-[":        "\x1b",
	} {
		if got, err := parseDetachKeys(in); err != nil || string(got) != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ctrl-", "ctrl-1", "ab", "ctrl-p,"} {
		if got, err := parseDetachKeys(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
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
