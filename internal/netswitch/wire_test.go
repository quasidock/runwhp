package netswitch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/virtio"
)

func TestRecords(t *testing.T) {
	var b bytes.Buffer
	frames := [][]byte{{}, bytes.Repeat([]byte{7}, 60), bytes.Repeat([]byte{9}, MaxFrame)}
	offs := []virtio.Offload{{}, {NeedsCsum: true, Start: 34, Offset: 16}, {}}
	for i := range frames {
		if err := writeRecord(&b, frames[i], offs[i]); err != nil {
			t.Fatal(err)
		}
	}
	for i := range frames {
		f, off, err := readRecord(&b)
		if err != nil || !bytes.Equal(f, frames[i]) || off != offs[i] {
			t.Fatalf("record %d: %d bytes, %+v, %v", i, len(f), off, err)
		}
	}
	if _, _, err := readRecord(&b); !errors.Is(err, io.EOF) {
		t.Errorf("after the last: %v", err)
	}
	for _, h := range [][]byte{
		{0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // too long
		{1, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0},          // unknown flag
	} {
		if _, _, err := readRecord(bytes.NewReader(append(h, 0))); err == nil || !strings.Contains(err.Error(), "bad record") {
			t.Errorf("header % x: %v", h, err)
		}
	}
}

// pipeConn is one end of an in-memory connection, counting its writes.
type pipeConn struct {
	net.Conn
	writes atomic.Int64
}

func (c *pipeConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(b)
}

func TestServeRefuses(t *testing.T) {
	for _, c := range []struct {
		hello Hello
		want  string
	}{
		{Hello{V: 2}, "unknown link protocol version"},
		{Hello{V: LinkVersion, Network: "other"}, "no such network"},
	} {
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- Serve(context.Background(), b, func(h Hello) (*Router, Peer, error) {
				return nil, Peer{}, errors.New("no such network")
			})
		}()
		writeLine(a, c.hello)
		var reply HelloReply
		if err := readLine(bufio.NewReader(a), &reply); err != nil || reply.OK || reply.Error != c.want {
			t.Errorf("%+v: %+v, %v", c.hello, reply, err)
		}
		if err := <-done; err == nil {
			t.Error("Serve returned no error for a refused link")
		}
		a.Close()
	}
}

// sink accepts links in memory and records what arrives on them.
type sink struct {
	mu     sync.Mutex
	frames [][]byte
	conns  []*pipeConn // the senders' ends
	serve  []net.Conn  // the receivers' ends
	dials  atomic.Int64
	hold   chan struct{} // if set, dials wait for it
	stall  bool          // if set, nothing is read after the hello
}

func (s *sink) dial(ctx context.Context, p Peer) (io.ReadWriteCloser, error) {
	s.dials.Add(1)
	if s.hold != nil {
		<-s.hold
	}
	a, b := net.Pipe()
	pc := &pipeConn{Conn: a}
	s.mu.Lock()
	s.conns = append(s.conns, pc)
	s.serve = append(s.serve, b)
	s.mu.Unlock()
	go func() {
		r := bufio.NewReader(b)
		var h Hello
		if readLine(r, &h) != nil {
			return
		}
		writeLine(b, HelloReply{OK: true})
		if s.stall {
			return
		}
		for {
			f, _, err := readRecord(r)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.frames = append(s.frames, f)
			s.mu.Unlock()
		}
	}()
	return pc, nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSenderBatches(t *testing.T) {
	s := &sink{hold: make(chan struct{})}
	snd := &Sender{Hello: Hello{Network: "n", From: "a"}, Dial: s.dial}
	defer snd.Close()
	p := Peer{Container: "b", IP: peerIP, MAC: MAC(peerIP)}
	// Queued while the link dials: written in a few writes once it has.
	for range 1000 {
		snd.Send(p, make([]byte, 80), virtio.Offload{})
	}
	close(s.hold)
	waitFor(t, "1000 frames", func() bool { return s.count() == 1000 })
	if w := s.conns[0].writes.Load(); w > 5 {
		t.Errorf("%d writes for 1000 queued frames", w)
	}
}

func TestSenderDropsWhenFull(t *testing.T) {
	s := &sink{stall: true}
	snd := &Sender{Hello: Hello{Network: "n", From: "a"}, Dial: s.dial, Budget: 10000}
	defer snd.Close()
	p := Peer{Container: "b", IP: peerIP, MAC: MAC(peerIP)}
	start := time.Now()
	for range 1000 {
		snd.Send(p, make([]byte, 1000), virtio.Offload{})
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("sending to a stalled peer took %v", d)
	}
	// Room for 10 frames queued, and as many as the link's 256 KiB buffer
	// took before its flush stalled.
	if snd.dropped.Load() < 1000-10-(256<<10)/1000-5 {
		t.Errorf("dropped %d of 1000 frames for a stalled peer", snd.dropped.Load())
	}
}

func TestSenderRedials(t *testing.T) {
	s := &sink{}
	var failed atomic.Int64
	snd := &Sender{Hello: Hello{Network: "n", From: "a"}, Dial: s.dial, Failed: func(Peer) { failed.Add(1) }}
	defer snd.Close()
	p := Peer{Container: "b", IP: peerIP, MAC: MAC(peerIP)}
	snd.Send(p, []byte("one"), virtio.Offload{})
	waitFor(t, "the first frame", func() bool { return s.count() == 1 })

	// The peer's VMM goes away: the next write fails, and a later frame
	// dials again.
	s.mu.Lock()
	s.serve[0].Close()
	s.mu.Unlock()
	waitFor(t, "a new link", func() bool {
		snd.Send(p, []byte("again"), virtio.Offload{})
		time.Sleep(20 * time.Millisecond)
		return s.dials.Load() == 2 && s.count() >= 2
	})
	if failed.Load() == 0 {
		t.Error("Failed not told of the broken link")
	}
}

// A refused hello fails the dial, and frames meanwhile are dropped until
// the backoff has passed.
func TestSenderRefused(t *testing.T) {
	var dials atomic.Int64
	snd := &Sender{Hello: Hello{Network: "n", From: "a"}, Dial: func(ctx context.Context, p Peer) (io.ReadWriteCloser, error) {
		dials.Add(1)
		a, b := net.Pipe()
		go Serve(context.Background(), b, func(Hello) (*Router, Peer, error) { return nil, Peer{}, errors.New("no") })
		return a, nil
	}}
	defer snd.Close()
	p := Peer{Container: "b", IP: peerIP, MAC: MAC(peerIP)}
	for range 20 {
		snd.Send(p, []byte("x"), virtio.Offload{})
	}
	waitFor(t, "the drops", func() bool { return snd.dropped.Load() == 20 })
	if d := dials.Load(); d != 1 {
		t.Errorf("%d dials within the backoff", d)
	}
}

func TestPeerFromHello(t *testing.T) {
	p, err := PeerFromHello(Hello{From: "c", FromIP: "172.18.0.3"})
	if err != nil || p.IP != peerIP || p.MAC != MAC(peerIP) || p.Container != "c" {
		t.Errorf("%+v %v", p, err)
	}
	for _, ip := range []string{"", "::1", "x"} {
		if _, err := PeerFromHello(Hello{FromIP: ip}); err == nil {
			t.Errorf("%q accepted", ip)
		}
	}
	_ = netip.Addr{}
}
