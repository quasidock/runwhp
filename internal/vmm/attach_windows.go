//go:build windows

package vmm

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// The supervisor's side of runwhp attach: other runwhps attach to the
// container's output and, for a container in the background run with -i,
// its input, over the container's pipe (see container.OpAttach).

// attachBacklog is how far an attached runwhp may fall behind the
// container's output before it is cut off. The container's output never
// waits for an attached runwhp: it goes where it goes first, to a log, a
// file or the console of the runwhp running it.
const attachBacklog = 4 << 20

// outputHub passes the container's output on to attached runwhps.
type outputHub struct {
	mu      sync.Mutex
	clients map[*attachClient]bool
}

func newOutputHub() *outputHub { return &outputHub{clients: map[*attachClient]bool{}} }

// wrap returns a writer that writes to w, then hands what it wrote to the
// attached runwhps as stream.
func (h *outputHub) wrap(stream byte, w io.Writer) io.Writer {
	return &hubWriter{h: h, stream: stream, w: w}
}

type hubWriter struct {
	h      *outputHub
	stream byte
	w      io.Writer
}

func (hw *hubWriter) Write(p []byte) (int, error) {
	n, err := len(p), error(nil)
	if hw.w != nil {
		n, err = hw.w.Write(p)
	}
	hw.h.offer(hw.stream, p)
	return n, err
}

func (h *outputHub) offer(stream byte, p []byte) {
	if len(p) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	frame := container.AppendFrame(nil, stream, p)
	for c := range h.clients {
		if !c.enqueue(frame) {
			delete(h.clients, c)
		}
	}
}

func (h *outputHub) add(c *attachClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *outputHub) remove(c *attachClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// attachClient is one attached runwhp: frames queued for it, and the
// goroutine writing them to its connection.
type attachClient struct {
	w    io.Writer
	wake chan struct{}
	done chan struct{} // closed once the writer has stopped

	mu     sync.Mutex
	queue  [][]byte
	queued int
	behind bool   // cut off for falling attachBacklog behind
	closed bool   // detached: write nothing more
	exit   []byte // the last frame, once the container has stopped
}

func newAttachClient(w io.Writer) *attachClient {
	return &attachClient{w: w, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (c *attachClient) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueue queues a frame without waiting, and reports whether the client
// still takes output.
func (c *attachClient) enqueue(frame []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.behind || c.closed || c.exit != nil {
		return false
	}
	if c.queued+len(frame) > attachBacklog {
		c.behind = true
		c.signal()
		return false
	}
	c.queue = append(c.queue, frame)
	c.queued += len(frame)
	c.signal()
	return true
}

// finish has the writer send what is queued, then exit, and stop.
func (c *attachClient) finish(exit []byte) {
	c.mu.Lock()
	c.exit = exit
	c.mu.Unlock()
	c.signal()
}

// close has the writer stop without writing more.
func (c *attachClient) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.signal()
}

const behindMessage = "runwhp: this attach fell too far behind the container's output, and was cut off\n"

// write writes queued frames until the client is finished, closed or cut
// off, or its connection fails.
func (c *attachClient) write() {
	defer close(c.done)
	for range c.wake {
		c.mu.Lock()
		queue, behind, closed, exit := c.queue, c.behind, c.closed, c.exit
		c.queue, c.queued = nil, 0
		c.mu.Unlock()
		if closed {
			return
		}
		for _, f := range queue {
			if _, err := c.w.Write(f); err != nil {
				return
			}
		}
		switch {
		case behind:
			c.w.Write(container.AppendFrame(nil, container.AttachStderr, []byte(behindMessage)))
			return
		case exit != nil:
			c.w.Write(exit)
			return
		}
	}
}

// attachStdin is the container's input when it is open to attached
// runwhps: what they send, in turn, until it ends.
type attachStdin struct {
	ch    chan []byte
	ended chan struct{}
	end   sync.Once
	once  bool // the first attached runwhp's end of input ends it

	mu  sync.Mutex
	cur []byte // taken from ch, not yet read
}

// attachInputBacklog is how many pieces of input wait for the container
// before an attached runwhp sending more is held up.
const attachInputBacklog = 64

func newAttachStdin(once bool) *attachStdin {
	return &attachStdin{ch: make(chan []byte, attachInputBacklog), ended: make(chan struct{}), once: once}
}

// view is the container's input for one boot, for its console's stdin
// port: it ends once done is closed, as the boot's VM goes, leaving what
// it has not handed out for the next boot's.
func (a *attachStdin) view(done <-chan struct{}) io.Reader { return &stdinView{a, done} }

type stdinView struct {
	a    *attachStdin
	done <-chan struct{}
}

func (v *stdinView) Read(p []byte) (int, error) {
	a := v.a
	for {
		// The boot's end goes first: select picks among ready cases at
		// random.
		select {
		case <-v.done:
			return 0, io.EOF
		default:
		}
		a.mu.Lock()
		if len(a.cur) > 0 {
			n := copy(p, a.cur)
			a.cur = a.cur[n:]
			a.mu.Unlock()
			return n, nil
		}
		a.mu.Unlock()
		var next []byte
		select {
		case next = <-a.ch:
		case <-a.ended:
			select {
			case next = <-a.ch: // what came before the end goes first
			default:
				return 0, io.EOF
			}
		case <-v.done:
			return 0, io.EOF
		}
		a.mu.Lock()
		a.cur = next
		a.mu.Unlock()
	}
}

// push passes input on, waiting while attachInputBacklog pieces wait.
// Input that comes once the container's has ended is dropped.
func (a *attachStdin) push(p []byte) {
	// A select picks among ready cases at random, and the backlog has
	// room, so the end has to be checked first.
	select {
	case <-a.ended:
		return
	default:
	}
	select {
	case a.ch <- p:
	case <-a.ended:
	}
}

// endInput is an attached runwhp's input ending: the container's too, if
// it takes input once.
func (a *attachStdin) endInput() {
	if a.once {
		a.close()
	}
}

// close ends the container's input.
func (a *attachStdin) close() { a.end.Do(func() { close(a.ended) }) }

// attach serves an attached runwhp on c until it detaches or the container
// stops. What it sent beyond its request waits in r.
func (s *Supervisor) attach(c net.Conn, r *bufio.Reader, req container.Request) {
	reply := func(rep container.Reply) {
		data, _ := json.Marshal(rep)
		c.Write(append(data, '\n'))
	}
	s.mu.Lock()
	in, b, status := s.stdin, s.cur, s.state.Status
	s.mu.Unlock()
	if status == container.Restarting {
		reply(container.Reply{Error: "container is restarting: attach once it is running"})
		return
	}
	if status == container.Stopped || b == nil {
		reply(container.Reply{Error: ErrNotRunning.Error()})
		return
	}
	cl := newAttachClient(c)
	s.hub.add(cl) // before the reply, so nothing after it is missed
	defer s.hub.remove(cl)
	reply(container.Reply{OK: true, Status: s.status(), Stdin: in != nil})
	go cl.write()

	takesInput := req.Stdin && in != nil
	detached := make(chan struct{})
	go func() {
		defer close(detached)
		for {
			kind, p, err := container.ReadFrame(r)
			if err != nil {
				return
			}
			switch kind {
			case container.AttachStdin:
				switch {
				case !takesInput:
				case len(p) == 0:
					in.endInput()
				default:
					in.push(p)
				}
			case container.AttachControl:
				var m guestspec.Control
				if json.Unmarshal(p, &m) == nil {
					s.Control(m)
				}
			}
		}
	}()

	select {
	case <-b.exited:
		s.mu.Lock()
		exit, _ := json.Marshal(b.exit)
		s.mu.Unlock()
		cl.finish(container.AppendFrame(nil, container.AttachExit, exit))
		select {
		case <-cl.done:
		case <-time.After(5 * time.Second):
		}
	case <-detached:
		cl.close()
	case <-cl.done: // cut off, or its connection failed
	}
}

// Control passes on what an attached runwhp may send the container: its
// terminal's size, and signals.
func (s *Supervisor) Control(m guestspec.Control) {
	var out guestspec.Control
	if m.Rows > 0 && m.Cols > 0 {
		out.Rows, out.Cols = m.Rows, m.Cols
	}
	if m.Signal > 0 && m.Signal <= 64 {
		out.Signal, out.Group = m.Signal, m.Group
	}
	if b := s.current(); out != (guestspec.Control{}) && b != nil && b.proxy.listening() {
		b.proxy.queue.send(out)
	}
}
