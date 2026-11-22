package virtio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

// vsockGuest plays the guest's side of a vsock device, as Linux's driver
// does: receive buffers of one descriptor each, and packets of one
// descriptor holding the header and payload.
type vsockGuest struct {
	t      *testing.T
	d      *driver
	dev    *Vsock
	mu     sync.Mutex
	rxAddr map[uint16]uint64 // posted receive buffers by head
	rxSize uint32
}

type vsockPacket struct {
	h       vsockHeader
	payload []byte
}

const guestCID = 3

func newVsockGuest(t *testing.T, rxBufs int, rxSize uint32) *vsockGuest {
	dev := &Vsock{CID: guestCID, Logf: t.Logf}
	g := &vsockGuest{t: t, d: newDriver(t, dev), dev: dev, rxAddr: map[uint16]uint64{}, rxSize: rxSize}
	for range rxBufs {
		g.post(g.d.alloc(uint64(rxSize)))
	}
	g.d.kick(vsockRxQueue)
	return g
}

func (g *vsockGuest) post(addr uint64) {
	head := g.d.add(vsockRxQueue, buf{addr, g.rxSize, true})
	g.mu.Lock()
	g.rxAddr[head] = addr
	g.mu.Unlock()
}

// send transmits one packet from the guest.
func (g *vsockGuest) send(h vsockHeader, payload []byte) {
	h.srcCID, h.dstCID, h.typ, h.len = guestCID, vsockHostCID, vsockTypeStream, uint32(len(payload))
	addr := g.d.alloc(uint64(vsockHeaderLen + len(payload)))
	h.put(g.d.mem[addr:])
	copy(g.d.mem[addr+vsockHeaderLen:], payload)
	g.d.add(vsockTxQueue, buf{addr, uint32(vsockHeaderLen + len(payload)), false})
	g.d.kick(vsockTxQueue)
	g.d.collect(vsockTxQueue)
}

// recv returns the packets the device has delivered, posting each buffer
// again.
func (g *vsockGuest) recv() []vsockPacket {
	var out []vsockPacket
	for _, u := range g.d.collect(vsockRxQueue) {
		g.mu.Lock()
		addr := g.rxAddr[u.head]
		delete(g.rxAddr, u.head)
		g.mu.Unlock()
		data := append([]byte(nil), g.d.mem[addr:addr+uint64(u.len)]...)
		h := parseVsockHeader(data)
		if h.srcCID != vsockHostCID || h.dstCID != guestCID || h.typ != vsockTypeStream || int(h.len) != len(data)-vsockHeaderLen {
			g.t.Errorf("bad packet header %+v for %d bytes", h, len(data))
		}
		out = append(out, vsockPacket{h, data[vsockHeaderLen:]})
		g.post(addr)
	}
	if len(out) > 0 {
		g.d.kick(vsockRxQueue)
	}
	return out
}

// expect waits for the next packet the device delivers.
func (g *vsockGuest) expect(op uint16) vsockPacket {
	g.t.Helper()
	var got []vsockPacket
	waitFor(g.t, "a packet", func() bool {
		got = append(got, g.recv()...)
		return len(got) > 0
	})
	if len(got) > 1 {
		g.t.Fatalf("expected one packet, got %d: %+v", len(got), got)
	}
	if got[0].h.op != op {
		g.t.Fatalf("got op %d (%+v), want %d", got[0].h.op, got[0].h, op)
	}
	return got[0]
}

// accept answers the device's next connection request, advertising
// bufAlloc, and returns the host's port and the connection.
func (g *vsockGuest) dialAccept(port, bufAlloc uint32) (*VsockConn, uint32) {
	g.t.Helper()
	var c *VsockConn
	var err error
	done := make(chan struct{})
	go func() {
		c, err = g.dev.Dial(context.Background(), port)
		close(done)
	}()
	req := g.expect(vsockOpRequest)
	if req.h.dstPort != port || req.h.srcPort < vsockFirstPort || req.h.bufAlloc != vsockBufAlloc {
		g.t.Errorf("request %+v", req.h)
	}
	g.send(vsockHeader{op: vsockOpResponse, srcPort: port, dstPort: req.h.srcPort, bufAlloc: bufAlloc}, nil)
	<-done
	if err != nil {
		g.t.Fatal(err)
	}
	return c, req.h.srcPort
}

func TestVsockConfig(t *testing.T) {
	g := newVsockGuest(t, 4, 4096)
	if g.dev.DeviceID() != 19 || g.dev.NumQueues() != 3 {
		t.Error("bad device id or queue count")
	}
	if cid := g.d.tr.Read(regConfig, 4) | g.d.tr.Read(regConfig+4, 4)<<32; cid != guestCID {
		t.Errorf("guest_cid = %d", cid)
	}
}

func TestVsockRefusedAndTimeout(t *testing.T) {
	g := newVsockGuest(t, 4, 4096)
	errc := make(chan error, 1)
	go func() {
		_, err := g.dev.Dial(context.Background(), 1024)
		errc <- err
	}()
	req := g.expect(vsockOpRequest)
	g.send(vsockHeader{op: vsockOpRst, srcPort: 1024, dstPort: req.h.srcPort}, nil)
	if err := <-errc; !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("refused dial: %v", err)
	}

	old := vsockDialTimeout
	vsockDialTimeout = 50 * time.Millisecond
	defer func() { vsockDialTimeout = old }()
	if _, err := g.dev.Dial(context.Background(), 1024); err == nil {
		t.Error("a dial nobody answered succeeded")
	}
	g.recv()                                                // the request
	if rst := g.expect(vsockOpRst); rst.h.dstPort != 1024 { // and the reset after the timeout
		t.Errorf("reset %+v", rst.h)
	}
}

func TestVsockReadWrite(t *testing.T) {
	g := newVsockGuest(t, 8, 4096)
	c, local := g.dialAccept(1024, 64<<10)

	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	p := g.expect(vsockOpRW)
	if string(p.payload) != "hello" || p.h.srcPort != local || p.h.dstPort != 1024 {
		t.Errorf("data %q %+v", p.payload, p.h)
	}

	g.send(vsockHeader{op: vsockOpRW, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10, fwdCnt: 5}, []byte("wor"))
	g.send(vsockHeader{op: vsockOpRW, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10, fwdCnt: 5}, []byte("ld"))
	g.send(vsockHeader{op: vsockOpShutdown, flags: vsockShutSend, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10, fwdCnt: 5}, nil)
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "world" {
		t.Errorf("read %q, %v", got, err)
	}

	// CloseWrite sends what is queued, then the shutdown.
	c.Write([]byte("last"))
	c.CloseWrite()
	var pkts []vsockPacket
	waitFor(t, "data and shutdown", func() bool {
		pkts = append(pkts, g.recv()...)
		return len(pkts) >= 2
	})
	if pkts[0].h.op != vsockOpRW || string(pkts[0].payload) != "last" || pkts[1].h.op != vsockOpShutdown || pkts[1].h.flags != vsockShutSend {
		t.Errorf("packets %+v", pkts)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Error("wrote after CloseWrite")
	}

	// The guest closes too: it sends a reset, and the connection is gone.
	g.send(vsockHeader{op: vsockOpShutdown, flags: vsockShutRecv | vsockShutSend, srcPort: 1024, dstPort: local}, nil)
	g.expect(vsockOpRst)
	g.d.tr.mu.Lock()
	n := len(g.dev.conns)
	g.d.tr.mu.Unlock()
	if n != 0 {
		t.Errorf("%d connections left", n)
	}
}

// TestVsockCredit has the guest advertise a small buffer: the host sends
// no more than it, asks for credit, and resumes when it gets some.
func TestVsockCredit(t *testing.T) {
	g := newVsockGuest(t, 8, 4096)
	c, local := g.dialAccept(1024, 1000)
	data := bytes.Repeat([]byte("abcdefghij"), 300)
	done := make(chan error, 1)
	go func() {
		_, err := c.Write(data)
		done <- err
	}()
	var got []byte
	fwd := uint32(0)
	for len(got) < len(data) {
		var pkts []vsockPacket
		waitFor(t, "packets", func() bool {
			pkts = append(pkts, g.recv()...)
			return len(pkts) > 0 && pkts[len(pkts)-1].h.op == vsockOpCreditRequest || len(got)+sum(pkts) >= len(data)
		})
		for _, p := range pkts {
			if p.h.op == vsockOpRW {
				got = append(got, p.payload...)
			}
		}
		if uint32(len(got))-fwd > 1000 {
			t.Fatalf("the host sent %d bytes past the guest's credit", uint32(len(got))-fwd-1000)
		}
		fwd = uint32(len(got))
		g.send(vsockHeader{op: vsockOpCreditUpdate, srcPort: 1024, dstPort: local, bufAlloc: 1000, fwdCnt: fwd}, nil)
	}
	if err := <-done; err != nil || !bytes.Equal(got, data) {
		t.Errorf("write: %v; got %d bytes", err, len(got))
	}
	if g.dev.Stalls.Load() == 0 {
		t.Error("no stall counted")
	}
	// A credit request is answered with the host's credit.
	g.send(vsockHeader{op: vsockOpCreditRequest, srcPort: 1024, dstPort: local, bufAlloc: 1000, fwdCnt: fwd}, nil)
	if p := g.expect(vsockOpCreditUpdate); p.h.bufAlloc != vsockBufAlloc {
		t.Errorf("credit update %+v", p.h)
	}
}

func sum(pkts []vsockPacket) int {
	n := 0
	for _, p := range pkts {
		n += len(p.payload)
	}
	return n
}

// TestVsockHostCredit checks the host's side of flow control: it tells the
// guest what it has read, and resets a guest that sends past its credit.
func TestVsockHostCredit(t *testing.T) {
	g := newVsockGuest(t, 8, 4096)
	c, local := g.dialAccept(1024, 64<<10)
	chunk := make([]byte, 32<<10)
	for range vsockBufAlloc / len(chunk) {
		g.send(vsockHeader{op: vsockOpRW, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10}, chunk)
	}
	buf := make([]byte, vsockBufAlloc/4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if p := g.expect(vsockOpCreditUpdate); p.h.fwdCnt != vsockBufAlloc/4 {
		t.Errorf("credit update %+v", p.h)
	}
	// The buffer holds 3/4 of bufAlloc: a quarter more fits, and then
	// nothing does.
	g.send(vsockHeader{op: vsockOpRW, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10}, make([]byte, vsockBufAlloc/4))
	g.send(vsockHeader{op: vsockOpRW, srcPort: 1024, dstPort: local, bufAlloc: 64 << 10}, make([]byte, 1))
	g.expect(vsockOpRst)
	for {
		if _, err := c.Read(buf); err != nil {
			if !errors.Is(err, syscall.ECONNRESET) {
				t.Errorf("read after the overrun: %v", err)
			}
			break
		}
	}
}

// TestVsockSmallBuffers delivers data in pieces the size of the guest's
// receive buffers.
func TestVsockSmallBuffers(t *testing.T) {
	g := newVsockGuest(t, 16, 100)
	c, _ := g.dialAccept(1024, 64<<10)
	data := []byte("the quick brown fox jumps over the lazy dog, again and again and again and again, " +
		"until the dog gets up and leaves, which takes a while")
	c.Write(data)
	var got []byte
	waitFor(t, "all the data", func() bool {
		for _, p := range g.recv() {
			if len(p.payload) > 100-vsockHeaderLen {
				t.Errorf("a packet of %d bytes in a 100-byte buffer", len(p.payload))
			}
			got = append(got, p.payload...)
		}
		return len(got) >= len(data)
	})
	if !bytes.Equal(got, data) {
		t.Errorf("got %q", got)
	}
}

func TestVsockStrayPackets(t *testing.T) {
	g := newVsockGuest(t, 8, 4096)
	// A connection request: nothing on the host listens.
	g.send(vsockHeader{op: vsockOpRequest, srcPort: 5000, dstPort: 80, bufAlloc: 4096}, nil)
	if p := g.expect(vsockOpRst); p.h.srcPort != 80 || p.h.dstPort != 5000 {
		t.Errorf("reset %+v", p.h)
	}
	// Data for no connection.
	g.send(vsockHeader{op: vsockOpRW, srcPort: 5000, dstPort: 80}, []byte("x"))
	g.expect(vsockOpRst)
	// A reset for no connection is not answered.
	g.send(vsockHeader{op: vsockOpRst, srcPort: 5000, dstPort: 80}, nil)
	time.Sleep(20 * time.Millisecond)
	if p := g.recv(); len(p) != 0 {
		t.Errorf("answered a reset: %+v", p)
	}
	// A packet from the wrong context.
	h := vsockHeader{op: vsockOpRW, srcPort: 5000, dstPort: 80, srcCID: 99}
	addr := g.d.alloc(vsockHeaderLen)
	h.dstCID, h.typ = vsockHostCID, vsockTypeStream
	h.put(g.d.mem[addr:])
	g.d.add(vsockTxQueue, buf{addr, vsockHeaderLen, false})
	g.d.kick(vsockTxQueue)
	g.expect(vsockOpRst)
}

func TestVsockClose(t *testing.T) {
	old := vsockCloseTimeout
	vsockCloseTimeout = 50 * time.Millisecond
	defer func() { vsockCloseTimeout = old }()
	g := newVsockGuest(t, 8, 4096)
	c, _ := g.dialAccept(1024, 64<<10)
	c.Close()
	if p := g.expect(vsockOpShutdown); p.h.flags != vsockShutRecv|vsockShutSend {
		t.Errorf("shutdown %+v", p.h)
	}
	// The guest never answers: the host resets the connection itself.
	g.expect(vsockOpRst)
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("read from a closed connection")
	}
}

func TestVsockFairness(t *testing.T) {
	g := newVsockGuest(t, 16, 4096)
	a, la := g.dialAccept(1024, 1<<20)
	b, lb := g.dialAccept(1025, 1<<20)
	// Both queue more than a buffer's worth before the guest has room.
	g.d.tr.mu.Lock()
	big := bytes.Repeat([]byte("a"), 3*4096)
	a.sendq, b.sendq = append([]byte(nil), big...), bytes.Repeat([]byte("b"), 3*4096)
	g.dev.schedule(a)
	g.dev.schedule(b)
	g.d.tr.mu.Unlock()
	g.d.kick(vsockRxQueue)
	var order []uint32
	waitFor(t, "data from both", func() bool {
		for _, p := range g.recv() {
			order = append(order, p.h.srcPort)
		}
		return len(order) >= 4
	})
	if order[0] == order[1] || order[1] == order[2] {
		t.Errorf("not in turn: %v (a %d, b %d)", order, la, lb)
	}
}

func TestVsockResetAndDrain(t *testing.T) {
	g := newVsockGuest(t, 8, 4096)
	c, _ := g.dialAccept(1024, 64<<10)
	g.d.tr.Write(regStatus, 4, 0) // reset
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("read after a device reset: %v", err)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Error("wrote after a device reset")
	}

	g = newVsockGuest(t, 8, 4096)
	c, _ = g.dialAccept(1024, 64<<10)
	if !g.dev.Drain(context.Background()) {
		t.Error("Drain reported busy")
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("read after Drain: %v", err)
	}
	if _, err := g.dev.Dial(context.Background(), 1024); err == nil {
		t.Error("dialed after Drain")
	}
	var unattached Vsock
	if _, err := unattached.Dial(context.Background(), 1); err == nil {
		t.Error("dialed an unattached device")
	}
}
