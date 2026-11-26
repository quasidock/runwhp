package netswitch

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/runwhp/internal/virtio"
)

// A link carries one guest's frames to another's VMM, one way: the sender
// dials the receiver's address, sends a Hello line, reads a HelloReply
// line, and then sends records, each a recordHeader and a frame, until
// either end closes. The receiver never writes after its reply.
//
// recordHeader is a frame's length, little-endian in 4 bytes, then its
// offload: a flag byte, 1 for a partial checksum, a zero byte, and the
// checksum's start and offset in 2 bytes each, then 2 zero bytes.
const recordHeader = 12

// MaxFrame is the largest frame a link carries: an Ethernet header and the
// largest IPv4 packet.
const MaxFrame = 14 + 65535

// LinkVersion is the version of the link protocol, in Hello.
const LinkVersion = 1

// Hello opens a link.
type Hello struct {
	V int `json:"v"`
	// Network is the network's ID; From and To are the containers' IDs.
	Network string `json:"network"`
	From    string `json:"from"`
	To      string `json:"to"`
	// FromIP is the sender's address on the network, from which its MAC
	// follows.
	FromIP string `json:"fromIP"`
}

// HelloReply answers a Hello: OK, or why not.
type HelloReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func writeRecord(w io.Writer, frame []byte, off virtio.Offload) error {
	var h [recordHeader]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(len(frame)))
	if off.NeedsCsum {
		h[4] = 1
	}
	binary.LittleEndian.PutUint16(h[6:], off.Start)
	binary.LittleEndian.PutUint16(h[8:], off.Offset)
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(frame)
	return err
}

// readRecord reads one record into a new slice.
func readRecord(r io.Reader) ([]byte, virtio.Offload, error) {
	var h [recordHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, virtio.Offload{}, err
	}
	n := binary.LittleEndian.Uint32(h[0:])
	if n > MaxFrame || h[4] > 1 {
		return nil, virtio.Offload{}, fmt.Errorf("bad record header % x", h)
	}
	off := virtio.Offload{NeedsCsum: h[4] == 1, Start: binary.LittleEndian.Uint16(h[6:]), Offset: binary.LittleEndian.Uint16(h[8:])}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, virtio.Offload{}, err
	}
	return frame, off, nil
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Accept answers a link's Hello: the router its frames are for, and the
// peer they come from, or an error to refuse it with.
type Accept func(h Hello) (*Router, Peer, error)

// Serve reads a link's Hello from c, answers it as accept says, and then
// hands every frame on it to the router, until c ends or ctx is done. It
// closes c.
func Serve(ctx context.Context, c io.ReadWriteCloser, accept Accept) error {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	r := bufio.NewReaderSize(c, 256<<10)
	var h Hello
	if err := readLine(r, &h); err != nil {
		return fmt.Errorf("reading a link's hello: %w", err)
	}
	var router *Router
	var from Peer
	err := errors.New("unknown link protocol version")
	if h.V == LinkVersion {
		router, from, err = accept(h)
	}
	if err != nil {
		writeLine(c, HelloReply{Error: err.Error()})
		return err
	}
	if err := writeLine(c, HelloReply{OK: true}); err != nil {
		return err
	}
	for {
		frame, off, err := readRecord(r)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		router.FromPeer(from, frame, off)
	}
}

// Sender is the Links of one guest on one network: a link to each peer it
// sends to, made when the first frame for that peer comes, and given up
// once it has carried nothing for a while.
type Sender struct {
	// Hello is what the guest says to open a link, less To.
	Hello Hello
	// Dial connects to a peer's VMM.
	Dial func(ctx context.Context, p Peer) (io.ReadWriteCloser, error)
	// Failed, if set, is told of a peer a link to which failed, so that the
	// directory can look again.
	Failed func(p Peer)
	// Budget is the most bytes a link holds before it drops frames; zero
	// means 4 MiB.
	Budget int
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, a ...any)

	mu     sync.Mutex
	links  map[string]*link // by peer address
	closed bool
	wg     sync.WaitGroup

	// Counters, for Stats.
	sent, dropped, dials, failures atomic.Uint64
}

const (
	linkQueue   = 4096
	linkIdle    = 60 * time.Second
	dialTimeout = time.Second
	minBackoff  = 100 * time.Millisecond
	maxBackoff  = 2 * time.Second
)

type record struct {
	frame []byte
	off   virtio.Offload
}

type link struct {
	s     *Sender
	peer  Peer
	q     chan record
	bytes atomic.Int64
	done  chan struct{}

	// conn is the connection, while there is one; stop closes it, so that
	// a write a stalled peer holds up ends.
	mu   sync.Mutex
	conn io.Closer
}

// stop ends the link's goroutine, closing its connection under any write.
func (l *link) stop() {
	close(l.done)
	l.mu.Lock()
	if l.conn != nil {
		l.conn.Close()
	}
	l.mu.Unlock()
}

// setConn records the connection, or closes c and says no if the link was
// stopped meanwhile.
func (l *link) setConn(c io.Closer) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.done:
		if c != nil {
			c.Close()
		}
		return false
	default:
	}
	l.conn = c
	return true
}

// Send implements Links.
func (s *Sender) Send(p Peer, frame []byte, off virtio.Offload) {
	l := s.link(p)
	if l == nil {
		s.dropped.Add(1)
		return
	}
	budget := int64(s.Budget)
	if budget == 0 {
		budget = 4 << 20
	}
	if l.bytes.Add(int64(len(frame))) > budget {
		l.bytes.Add(-int64(len(frame)))
		s.dropped.Add(1)
		return
	}
	select {
	case l.q <- record{frame, off}:
	default:
		l.bytes.Add(-int64(len(frame)))
		s.dropped.Add(1)
	}
}

// link is the link to p, made if there is none. Dial finds where p's VMM
// is each time it dials, so a link outlives its peer's restarts: its next
// write fails, and the frame after that dials the new VMM.
func (s *Sender) link(p Peer) *link {
	key := p.Container + "|" + p.IP.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if l := s.links[key]; l != nil {
		return l
	}
	if s.links == nil {
		s.links = map[string]*link{}
	}
	l := &link{s: s, peer: p, q: make(chan record, linkQueue), done: make(chan struct{})}
	s.links[key] = l
	s.wg.Add(1)
	go l.run(key)
	return l
}

// Close closes every link.
func (s *Sender) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, l := range s.links {
			l.stop()
		}
		s.links = nil
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Stats summarises what the links did.
func (s *Sender) Stats() string {
	return fmt.Sprintf("links: %d frames sent, %d dropped, %d dials, %d failures", s.sent.Load(), s.dropped.Load(), s.dials.Load(), s.failures.Load())
}

func (s *Sender) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// run carries the link's frames: it dials on the first, writes what is
// queued in as few writes as it can, and after a failure drops frames
// until it has waited a little and dialed again.
func (l *link) run(key string) {
	s := l.s
	defer s.wg.Done()
	var conn io.ReadWriteCloser
	var w *bufio.Writer
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	var retryAt time.Time
	backoff := minBackoff
	idle := time.NewTimer(linkIdle)
	defer idle.Stop()
	write := func(r record) error {
		l.bytes.Add(-int64(len(r.frame)))
		if err := writeRecord(w, r.frame, r.off); err != nil {
			return err
		}
		s.sent.Add(1)
		return nil
	}
	for {
		var r record
		select {
		case r = <-l.q:
		case <-l.done:
			return
		case <-idle.C:
			// Nothing for a while: give the link up, unless a frame came
			// meanwhile and made a new one.
			s.mu.Lock()
			if s.links[key] == l {
				delete(s.links, key)
			}
			s.mu.Unlock()
			l.drain()
			return
		}
		idle.Reset(linkIdle)
		if conn == nil {
			if time.Now().Before(retryAt) {
				l.bytes.Add(-int64(len(r.frame)))
				s.dropped.Add(1)
				continue
			}
			var err error
			if conn, err = l.dial(); err != nil {
				s.failures.Add(1)
				s.logf("link to %s at %s: %v", l.peer.Container, l.peer.IP, err)
				retryAt = time.Now().Add(backoff)
				backoff = min(2*backoff, maxBackoff)
				l.bytes.Add(-int64(len(r.frame)))
				s.dropped.Add(1)
				if s.Failed != nil {
					s.Failed(l.peer)
				}
				continue
			}
			if !l.setConn(conn) {
				conn = nil
				return
			}
			backoff = minBackoff
			w = bufio.NewWriterSize(conn, 256<<10)
		}
		err := write(r)
	more:
		for err == nil {
			select {
			case r = <-l.q:
				err = write(r)
			default:
				break more
			}
		}
		if err == nil {
			err = w.Flush()
		}
		if err != nil {
			s.failures.Add(1)
			s.logf("link to %s at %s: %v", l.peer.Container, l.peer.IP, err)
			conn.Close()
			conn, w = nil, nil
			if !l.setConn(nil) {
				return
			}
			if s.Failed != nil {
				s.Failed(l.peer)
			}
		}
	}
}

// dial connects and says hello.
func (l *link) dial() (io.ReadWriteCloser, error) {
	s := l.s
	s.dials.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	c, err := s.Dial(ctx, l.peer)
	if err != nil {
		return nil, err
	}
	h := s.Hello
	h.V, h.To = LinkVersion, l.peer.Container
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := writeLine(c, h); err != nil {
		c.Close()
		return nil, err
	}
	var reply HelloReply
	if err := readLine(bufio.NewReader(c), &reply); err != nil {
		c.Close()
		return nil, fmt.Errorf("no answer to hello: %w", err)
	}
	if !reply.OK {
		c.Close()
		return nil, fmt.Errorf("refused: %s", reply.Error)
	}
	return c, nil
}

// drain drops what is queued.
func (l *link) drain() {
	for {
		select {
		case r := <-l.q:
			l.bytes.Add(-int64(len(r.frame)))
			l.s.dropped.Add(1)
		default:
			return
		}
	}
}

// PeerFromHello is the peer a Hello says it comes from.
func PeerFromHello(h Hello) (Peer, error) {
	ip, err := netip.ParseAddr(h.FromIP)
	if err != nil || !ip.Is4() {
		return Peer{}, fmt.Errorf("bad address %q", h.FromIP)
	}
	return Peer{Container: h.From, IP: ip, MAC: MAC(ip)}, nil
}
