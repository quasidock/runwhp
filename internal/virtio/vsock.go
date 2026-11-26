package virtio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Vsock is a virtio socket device (virtio spec 5.10): stream connections
// between the host, context ID 2, and the guest, context ID CID. The host
// connects to ports the guest listens on with Dial, and the guest to ports
// the host listens on with Listen; its requests for any other port are
// refused.
//
// Each connection has its own flow control: the guest says how much it can
// buffer and how much it has consumed, and the host sends no more than
// that; the host does the same for what the guest sends it. A slow reader
// on one connection never holds up another.
type Vsock struct {
	CID  uint64
	Logf func(format string, a ...any)

	// Conns counts connections made; TxPackets and TxBytes count what the
	// guest sent, RxPackets and RxBytes what it was sent; Resets counts
	// connections reset, and Stalls the times a connection waited for the
	// guest to make room.
	Conns, TxPackets, TxBytes, RxPackets, RxBytes, Resets, Stalls atomic.Uint64

	t atomic.Pointer[Transport]

	// Guarded by the transport's lock.
	conns    map[vsockKey]*VsockConn
	nextPort uint32
	ctrl     []vsockCtrl  // control packets for the guest, sent before data
	ready    []*VsockConn // connections with data for the guest, in turn
	stopped  bool

	lmu       sync.Mutex
	listeners map[uint32]*VsockListener
}

// Vsock queues, header and protocol constants.
const (
	vsockRxQueue    = 0
	vsockTxQueue    = 1
	vsockEventQueue = 2

	vsockHostCID = 2

	vsockHeaderLen  = 44
	vsockTypeStream = 1

	vsockOpRequest       = 1
	vsockOpResponse      = 2
	vsockOpRst           = 3
	vsockOpShutdown      = 4
	vsockOpRW            = 5
	vsockOpCreditUpdate  = 6
	vsockOpCreditRequest = 7

	vsockShutRecv = 1 // the sender will receive no more
	vsockShutSend = 2 // the sender will send no more

	// vsockBufAlloc is how much the host buffers for each connection: what
	// the guest may send it beyond what it has read.
	vsockBufAlloc = 256 << 10
	// vsockSendQueue bounds what a Write queues for the guest.
	vsockSendQueue = 64 << 10
	// vsockMaxPacket bounds the payload of a packet from the guest.
	vsockMaxPacket = 64 << 10

	// vsockFirstPort is the first of the ports Dial takes for itself;
	// Listen takes those below it.
	vsockFirstPort = 49152
	// vsockBacklog bounds the connections a listener holds unaccepted.
	vsockBacklog = 16
)

// How long a Dial waits for the guest to answer, and how long a Close waits
// for the guest's reset before sending one; tests shorten them.
var (
	vsockDialTimeout  = 2 * time.Second
	vsockCloseTimeout = 2 * time.Second
)

type vsockKey struct{ local, peer uint32 }

type vsockHeader struct {
	srcCID, dstCID   uint64
	srcPort, dstPort uint32
	len              uint32
	typ, op          uint16
	flags            uint32
	bufAlloc, fwdCnt uint32
}

func (h *vsockHeader) put(b []byte) {
	le.PutUint64(b[0:], h.srcCID)
	le.PutUint64(b[8:], h.dstCID)
	le.PutUint32(b[16:], h.srcPort)
	le.PutUint32(b[20:], h.dstPort)
	le.PutUint32(b[24:], h.len)
	le.PutUint16(b[28:], h.typ)
	le.PutUint16(b[30:], h.op)
	le.PutUint32(b[32:], h.flags)
	le.PutUint32(b[36:], h.bufAlloc)
	le.PutUint32(b[40:], h.fwdCnt)
}

func parseVsockHeader(b []byte) vsockHeader {
	return vsockHeader{
		srcCID: le.Uint64(b[0:]), dstCID: le.Uint64(b[8:]),
		srcPort: le.Uint32(b[16:]), dstPort: le.Uint32(b[20:]),
		len: le.Uint32(b[24:]), typ: le.Uint16(b[28:]), op: le.Uint16(b[30:]),
		flags: le.Uint32(b[32:]), bufAlloc: le.Uint32(b[36:]), fwdCnt: le.Uint32(b[40:]),
	}
}

// vsockCtrl is a control packet waiting for a receive buffer. c, when set,
// supplies the credit fields as they stand when it is sent.
type vsockCtrl struct {
	op          uint16
	flags       uint32
	local, peer uint32
	c           *VsockConn
}

func (v *Vsock) DeviceID() uint32 { return 19 }
func (v *Vsock) NumQueues() int   { return 3 }

// Features offers none beyond version 1: without VIRTIO_VSOCK_F_STREAM a
// device is taken to have stream sockets and nothing else.
func (v *Vsock) Features() uint64 { return FeatVersion1 }

func (v *Vsock) Config() []byte {
	cfg := make([]byte, 8)
	le.PutUint64(cfg, v.CID)
	return cfg
}

// Attach implements Attacher.
func (v *Vsock) Attach(t *Transport) { v.t.Store(t) }

func (v *Vsock) logf(format string, a ...any) {
	if v.Logf != nil {
		v.Logf(format, a...)
	}
}

// Reset is called when the driver resets the device: every connection is
// reset.
func (v *Vsock) Reset() {
	v.failAll(syscall.ECONNRESET)
}

// Drain is called once the guest has stopped. Nothing of guest memory is
// held between notifications, so it only fails every connection and
// refuses new ones.
func (v *Vsock) Drain(ctx context.Context) bool {
	if t := v.t.Load(); t != nil {
		t.Do(func() {
			v.stopped = true
			v.failAll(net.ErrClosed)
		})
	}
	v.lmu.Lock()
	ls := v.listeners
	v.listeners = nil
	for _, l := range ls {
		l.markShut()
	}
	v.lmu.Unlock()
	for _, l := range ls {
		l.drain()
	}
	return true
}

func (v *Vsock) failAll(err error) {
	for k, c := range v.conns {
		c.fail(err)
		delete(v.conns, k)
	}
	v.ctrl, v.ready = nil, nil
}

func (v *Vsock) Notify(t *Transport, q int) {
	switch q {
	case vsockTxQueue:
		v.transmit(t)
		v.pump(t)
	case vsockRxQueue:
		v.pump(t)
	}
}

// transmit takes every packet the guest made available.
func (v *Vsock) transmit(t *Transport) {
	q := t.Queue(vsockTxQueue)
	if q == nil {
		return
	}
	mem := t.Memory()
	served := false
	for {
		ch, ok, err := q.Pop(mem)
		if !ok {
			break
		}
		if err == nil {
			if pkt := gather(ch.Buffers, 0); len(pkt) >= vsockHeaderLen {
				v.receive(pkt)
			}
		}
		q.Push(mem, ch.Head, 0)
		served = true
	}
	if served {
		t.Interrupt(vsockTxQueue)
	}
}

// receive handles one packet from the guest.
func (v *Vsock) receive(pkt []byte) {
	h := parseVsockHeader(pkt)
	payload := pkt[vsockHeaderLen:]
	v.TxPackets.Add(1)
	rst := func() {
		if h.op != vsockOpRst {
			v.queueCtrl(vsockCtrl{op: vsockOpRst, local: h.dstPort, peer: h.srcPort})
		}
	}
	if h.srcCID != v.CID || h.dstCID != vsockHostCID || h.typ != vsockTypeStream {
		rst()
		return
	}
	key := vsockKey{h.dstPort, h.srcPort}
	c := v.conns[key]
	switch {
	case c == nil && h.op == vsockOpRequest:
		if !v.accept(h) {
			rst() // nothing listens on the port, or its backlog is full
		}
		return
	case c == nil:
		rst() // a packet for a connection gone
		return
	case int(h.len) != len(payload) || len(payload) > vsockMaxPacket:
		v.logf("vsock: port %d sent a malformed packet; resetting", c.peer)
		v.reset(c, syscall.ECONNRESET)
		return
	}
	before := c.credit()
	c.peerBufAlloc, c.peerFwdCnt = h.bufAlloc, h.fwdCnt
	if c.credit() > before {
		c.creditRequested = false
		v.schedule(c)
	}
	switch h.op {
	case vsockOpResponse:
		if c.state != vsockConnecting {
			v.reset(c, syscall.ECONNRESET)
			return
		}
		c.state = vsockEstablished
		c.settle(nil)
		v.schedule(c)
	case vsockOpRst:
		err := error(syscall.ECONNRESET)
		if c.state == vsockConnecting {
			err = syscall.ECONNREFUSED
		}
		v.forget(c)
		c.fail(err)
	case vsockOpShutdown:
		if h.flags&vsockShutSend != 0 {
			c.peerShutSend = true
			c.wake(&c.readable)
		}
		if h.flags&vsockShutRecv != 0 {
			c.peerShutRecv = true
			c.sendq = nil
			c.wake(&c.writable)
		}
		if c.peerShutSend && c.peerShutRecv {
			v.queueCtrl(vsockCtrl{op: vsockOpRst, local: c.local, peer: c.peer})
			v.forget(c)
			c.state = vsockClosed
		}
	case vsockOpRW:
		if c.state != vsockEstablished || c.peerShutSend {
			v.reset(c, syscall.ECONNRESET)
			return
		}
		if c.rxBytes+len(payload) > vsockBufAlloc {
			v.logf("vsock: port %d sent past its credit; resetting", c.peer)
			v.reset(c, syscall.ECONNRESET)
			return
		}
		if len(payload) > 0 {
			c.rx = append(c.rx, payload)
			c.rxBytes += len(payload)
			v.TxBytes.Add(uint64(len(payload)))
			c.wake(&c.readable)
		}
	case vsockOpRequest:
		v.reset(c, syscall.ECONNRESET) // a second request for the same pair
	case vsockOpCreditUpdate:
	case vsockOpCreditRequest:
		v.queueCtrl(vsockCtrl{op: vsockOpCreditUpdate, local: c.local, peer: c.peer, c: c})
	default:
		v.reset(c, syscall.ECONNRESET)
	}
}

func (v *Vsock) queueCtrl(p vsockCtrl) { v.ctrl = append(v.ctrl, p) }

// schedule puts a connection with data to send in line.
func (v *Vsock) schedule(c *VsockConn) {
	if !c.inReady && (len(c.sendq) > 0 || c.wantShut != 0) {
		c.inReady = true
		v.ready = append(v.ready, c)
	}
}

// forget removes a connection from the table.
func (v *Vsock) forget(c *VsockConn) {
	if v.conns[vsockKey{c.local, c.peer}] == c {
		delete(v.conns, vsockKey{c.local, c.peer})
	}
}

// reset sends the guest a reset for c and fails it.
func (v *Vsock) reset(c *VsockConn, err error) {
	v.Resets.Add(1)
	v.queueCtrl(vsockCtrl{op: vsockOpRst, local: c.local, peer: c.peer})
	v.forget(c)
	c.fail(err)
}

// pump sends what waits for the guest, control packets first, then data
// from each connection in turn, while the guest has receive buffers, and
// raises one interrupt.
func (v *Vsock) pump(t *Transport) {
	q := t.Queue(vsockRxQueue)
	if q == nil {
		return
	}
	mem := t.Memory()
	served := false
	for {
		var h vsockHeader
		var payload []byte
		var after func()
		switch {
		case len(v.ctrl) > 0:
			p := v.ctrl[0]
			h = vsockHeader{op: p.op, flags: p.flags, srcPort: p.local, dstPort: p.peer}
			if p.c != nil {
				h.bufAlloc, h.fwdCnt = vsockBufAlloc, p.c.fwdCnt
				p.c.lastFwdSent = p.c.fwdCnt
			}
			after = func() { v.ctrl = v.ctrl[1:] }
		case len(v.ready) > 0:
			c := v.ready[0]
			credit := c.credit()
			if c.state != vsockEstablished || len(c.sendq) == 0 || credit == 0 {
				v.ready = v.ready[1:]
				c.inReady = false
				switch {
				case c.state != vsockEstablished:
				case len(c.sendq) > 0:
					if !c.creditRequested {
						c.creditRequested = true
						v.Stalls.Add(1)
						v.queueCtrl(vsockCtrl{op: vsockOpCreditRequest, local: c.local, peer: c.peer, c: c})
					}
				case c.wantShut != 0 && !c.shutSent:
					c.shutSent = true
					v.queueCtrl(vsockCtrl{op: vsockOpShutdown, flags: c.wantShut, local: c.local, peer: c.peer, c: c})
				}
				continue
			}
			h = vsockHeader{op: vsockOpRW, srcPort: c.local, dstPort: c.peer, bufAlloc: vsockBufAlloc, fwdCnt: c.fwdCnt}
			payload = c.sendq[:min(len(c.sendq), int(credit))]
			after = func() {
				n := len(payload)
				c.sendq = c.sendq[n:]
				if len(c.sendq) == 0 {
					c.sendq = nil
				}
				c.txCnt += uint32(n)
				c.lastFwdSent = c.fwdCnt
				v.RxBytes.Add(uint64(n))
				c.wake(&c.writable)
				// Its turn is over; back of the line if it has more.
				v.ready = v.ready[1:]
				c.inReady = false
				v.schedule(c)
			}
		default:
			if served {
				t.Interrupt(vsockRxQueue)
			}
			return
		}
		ch, ok, err := q.Pop(mem)
		if !ok {
			break
		}
		room := 0
		for _, b := range ch.Buffers {
			if b.Writable {
				room += len(b.Data)
			}
		}
		if err != nil || room <= vsockHeaderLen {
			v.logf("vsock: a receive buffer of %d bytes is too small", room)
			q.Push(mem, ch.Head, 0)
			served = true
			continue
		}
		if len(payload) > room-vsockHeaderLen {
			payload = payload[:room-vsockHeaderLen]
		}
		h.srcCID, h.dstCID, h.typ, h.len = vsockHostCID, v.CID, vsockTypeStream, uint32(len(payload))
		var hdr [vsockHeaderLen]byte
		h.put(hdr[:])
		src := [][]byte{hdr[:], payload}
		var written uint32
		for _, b := range ch.Buffers {
			if b.Writable {
				written += uint32(copyFrom(&src, b.Data))
			}
		}
		q.Push(mem, ch.Head, written)
		v.RxPackets.Add(1)
		served = true
		after()
	}
	if served {
		t.Interrupt(vsockRxQueue)
	}
}

func (v *Vsock) newConn(t *Transport, local, peer uint32) *VsockConn {
	return &VsockConn{v: v, t: t, local: local, peer: peer, done: make(chan struct{}), closed: make(chan struct{}),
		readable: make(chan struct{}, 1), writable: make(chan struct{}, 1)}
}

// initConns makes the connection table. The transport's lock is held.
func (v *Vsock) initConns() {
	if v.conns == nil {
		v.conns = map[vsockKey]*VsockConn{}
		v.nextPort = vsockFirstPort
	}
}

// accept answers a connection request from the guest, if something listens
// on its port and has room for it. The transport's lock is held.
func (v *Vsock) accept(h vsockHeader) bool {
	t := v.t.Load()
	if t == nil || v.stopped {
		return false
	}
	c := v.newConn(t, h.dstPort, h.srcPort)
	c.state = vsockEstablished
	c.peerBufAlloc, c.peerFwdCnt = h.bufAlloc, h.fwdCnt
	c.settle(nil)
	// The listener's lock is held while the connection is queued, so that
	// none is queued once it has been shut: shutting drains what is queued.
	v.lmu.Lock()
	l := v.listeners[h.dstPort]
	queued := false
	if l != nil && !l.isShut {
		select {
		case l.backlog <- c:
			queued = true
		default:
			v.logf("vsock: port %d has %d connections waiting; refusing another", h.dstPort, vsockBacklog)
		}
	}
	v.lmu.Unlock()
	if !queued {
		return false
	}
	v.initConns()
	v.conns[vsockKey{c.local, c.peer}] = c
	v.Conns.Add(1)
	v.queueCtrl(vsockCtrl{op: vsockOpResponse, local: c.local, peer: c.peer, c: c})
	return true
}

// VsockListener takes the guest's connections to one host port.
type VsockListener struct {
	v       *Vsock
	port    uint32
	backlog chan *VsockConn
	done    chan struct{}
	isShut  bool // guarded by v.lmu
}

// Listen takes the guest's connections to port, which must be below the
// ports Dial takes, until the listener is closed or the device drained.
func (v *Vsock) Listen(port uint32) (*VsockListener, error) {
	if port == 0 || port >= vsockFirstPort {
		return nil, fmt.Errorf("vsock: cannot listen on port %d: ports from %d are the host's own", port, vsockFirstPort)
	}
	v.lmu.Lock()
	defer v.lmu.Unlock()
	if v.listeners[port] != nil {
		return nil, fmt.Errorf("vsock: port %d is already listened on", port)
	}
	if v.listeners == nil {
		v.listeners = map[uint32]*VsockListener{}
	}
	l := &VsockListener{v: v, port: port, backlog: make(chan *VsockConn, vsockBacklog), done: make(chan struct{})}
	v.listeners[port] = l
	return l, nil
}

// Accept waits for the guest's next connection.
func (l *VsockListener) Accept() (*VsockConn, error) {
	select {
	case c := <-l.backlog:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops listening; connections not yet accepted are closed.
func (l *VsockListener) Close() error {
	l.v.lmu.Lock()
	if l.v.listeners[l.port] == l {
		delete(l.v.listeners, l.port)
	}
	l.markShut()
	l.v.lmu.Unlock()
	l.drain()
	return nil
}

// markShut ends Accept and stops connections being queued; v.lmu is held.
func (l *VsockListener) markShut() {
	if !l.isShut {
		l.isShut = true
		close(l.done)
	}
}

// drain closes the connections queued and never accepted. Neither lock is
// held: closing a connection takes the transport's.
func (l *VsockListener) drain() {
	for {
		select {
		case c := <-l.backlog:
			c.Close()
		default:
			return
		}
	}
}

// Port is the host port the listener takes connections to.
func (l *VsockListener) Port() uint32 { return l.port }

// Dial connects to port on the guest.
func (v *Vsock) Dial(ctx context.Context, port uint32) (*VsockConn, error) {
	t := v.t.Load()
	if t == nil {
		return nil, errors.New("vsock: the device is not attached")
	}
	c := v.newConn(t, 0, port)
	var err error
	ran := t.Do(func() {
		if v.stopped {
			err = net.ErrClosed
			return
		}
		v.initConns()
		for {
			c.local = v.nextPort
			if v.nextPort++; v.nextPort == 0 {
				v.nextPort = vsockFirstPort
			}
			if v.conns[vsockKey{c.local, port}] == nil {
				break
			}
		}
		v.conns[vsockKey{c.local, port}] = c
		v.queueCtrl(vsockCtrl{op: vsockOpRequest, local: c.local, peer: port, c: c})
		v.pump(t)
	})
	if !ran {
		err = net.ErrClosed
	}
	if err != nil {
		return nil, err
	}
	v.Conns.Add(1)
	timer := time.NewTimer(vsockDialTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = fmt.Errorf("vsock: no answer from port %d", port)
	}
	if err != nil {
		t.Do(func() { v.reset(c, err) })
		return nil, err
	}
	if c.err != nil {
		return nil, fmt.Errorf("vsock: connecting to port %d: %w", port, c.err)
	}
	return c, nil
}

// VsockConn is one connection to the guest.
type VsockConn struct {
	v           *Vsock
	t           *Transport
	local, peer uint32

	done      chan struct{} // closed once connected or failed
	doneOnce  sync.Once
	closed    chan struct{} // closed by Close
	closeOnce sync.Once
	readable  chan struct{}
	writable  chan struct{}

	// Guarded by the transport's lock.
	state           int
	err             error
	rx              [][]byte
	rxBytes         int
	fwdCnt          uint32 // bytes read from the guest
	lastFwdSent     uint32 // fwdCnt as the guest last heard it
	sendq           []byte
	txCnt           uint32 // bytes sent to the guest
	peerBufAlloc    uint32
	peerFwdCnt      uint32
	creditRequested bool
	inReady         bool
	peerShutSend    bool // the guest will send no more
	peerShutRecv    bool // the guest will receive no more
	wantShut        uint32
	shutSent        bool
}

const (
	vsockConnecting = iota
	vsockEstablished
	vsockClosed
)

// credit is how much more the guest can take.
func (c *VsockConn) credit() uint32 {
	used := c.txCnt - c.peerFwdCnt
	if used >= c.peerBufAlloc {
		return 0
	}
	return c.peerBufAlloc - used
}

func (c *VsockConn) wake(ch *chan struct{}) {
	select {
	case *ch <- struct{}{}:
	default:
	}
}

func (c *VsockConn) settle(err error) {
	c.doneOnce.Do(func() {
		if err != nil {
			c.err = err
		}
		close(c.done)
	})
}

// fail ends the connection with err. The transport's lock is held.
func (c *VsockConn) fail(err error) {
	if c.err == nil {
		c.err = err
	}
	c.state = vsockClosed
	c.sendq = nil
	c.settle(err)
	c.wake(&c.readable)
	c.wake(&c.writable)
}

// Read reads what the guest sent. It returns io.EOF once the guest has
// shut down its sending side and everything before that has been read.
func (c *VsockConn) Read(p []byte) (int, error) {
	for {
		var n int
		var err error
		ran := c.t.Do(func() {
			switch {
			case c.rxBytes > 0:
				for n < len(p) && len(c.rx) > 0 {
					k := copy(p[n:], c.rx[0])
					n += k
					if c.rx[0] = c.rx[0][k:]; len(c.rx[0]) == 0 {
						c.rx = c.rx[1:]
					}
				}
				c.rxBytes -= n
				c.fwdCnt += uint32(n)
				if c.state == vsockEstablished && c.fwdCnt-c.lastFwdSent >= vsockBufAlloc/4 {
					c.v.queueCtrl(vsockCtrl{op: vsockOpCreditUpdate, local: c.local, peer: c.peer, c: c})
					c.v.pump(c.t)
				}
			case c.peerShutSend:
				err = io.EOF
			case c.err != nil:
				err = c.err
			}
		})
		if !ran {
			return 0, net.ErrClosed
		}
		if n > 0 || err != nil {
			return n, err
		}
		select {
		case <-c.readable:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

// Write sends p to the guest, as fast as the guest makes room for it.
func (c *VsockConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		var n int
		var err error
		ran := c.t.Do(func() {
			switch {
			case c.err != nil:
				err = c.err
			case c.wantShut&vsockShutSend != 0:
				err = net.ErrClosed
			case c.peerShutRecv:
				err = syscall.EPIPE
			default:
				n = min(vsockSendQueue-len(c.sendq), len(p)-written)
				if n > 0 {
					c.sendq = append(c.sendq, p[written:written+n]...)
					c.v.schedule(c)
					c.v.pump(c.t)
				}
			}
		})
		if !ran {
			return written, net.ErrClosed
		}
		if err != nil {
			return written, err
		}
		written += n
		if n == 0 {
			select {
			case <-c.writable:
			case <-c.closed:
				return written, net.ErrClosed
			}
		}
	}
	return written, nil
}

// CloseWrite tells the guest the host will send no more, once what is
// queued has gone.
func (c *VsockConn) CloseWrite() error {
	c.t.Do(func() { c.shutdown(vsockShutSend) })
	return nil
}

// shutdown asks for a SHUTDOWN with flags, sent once the send queue has
// drained. The transport's lock is held.
func (c *VsockConn) shutdown(flags uint32) {
	if c.state == vsockClosed || c.wantShut&flags == flags {
		return
	}
	c.wantShut |= flags
	c.shutSent = false
	c.v.schedule(c)
	c.v.pump(c.t)
}

// Close shuts the connection both ways. The guest answers with a reset;
// without one within two seconds, the host resets the connection itself.
func (c *VsockConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.t.Do(func() {
			c.rx, c.rxBytes = nil, 0
			c.shutdown(vsockShutRecv | vsockShutSend)
		})
		time.AfterFunc(vsockCloseTimeout, func() {
			c.t.Do(func() {
				if c.v.conns[vsockKey{c.local, c.peer}] == c {
					c.v.reset(c, net.ErrClosed)
					c.v.pump(c.t)
				}
			})
		})
	})
	return nil
}

// Port is the guest port the connection reaches.
func (c *VsockConn) Port() uint32 { return c.peer }

// Stats describes the device's traffic, for --debug.
func (v *Vsock) Stats() string {
	return fmt.Sprintf("vsock: %d connections, %d packets/%d bytes from the guest, %d packets/%d bytes to it, %d resets, %d stalls",
		v.Conns.Load(), v.TxPackets.Load(), v.TxBytes.Load(), v.RxPackets.Load(), v.RxBytes.Load(), v.Resets.Load(), v.Stalls.Load())
}
