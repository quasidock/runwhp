package virtio

import (
	"bytes"
	"context"
	"testing"
)

// netFrames collects what a Net hands to Recv. Recv runs with the transport
// locked, and the tests read it after their kick returns, so no lock is needed.
type netFrames struct{ got [][]byte }

func (f *netFrames) recv(frame []byte) { f.got = append(f.got, frame) }

func newNet(t *testing.T) (*Net, *driver, *netFrames) {
	frames := &netFrames{}
	dev := &Net{MAC: [6]byte{2, 0, 0, 0, 0, 0x0f}, MTU: 65520, Recv: frames.recv}
	return dev, newDriver(t, dev), frames
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7)
	}
	return b
}

// postRx makes count receive buffers of size bytes available, as the Linux
// driver does with mergeable buffers: one writable descriptor per chain.
func postRx(d *driver, count, size int) []uint64 {
	var addrs []uint64
	for range count {
		a := d.alloc(uint64(size))
		d.add(netRxQueue, buf{a, uint32(size), true})
		addrs = append(addrs, a)
	}
	return addrs
}

func TestNetConfig(t *testing.T) {
	dev, d, _ := newNet(t)
	if dev.Features()&netFeatMrgRxbuf == 0 || dev.Features()&netFeatMTU == 0 || dev.Features()&netFeatMAC == 0 {
		t.Errorf("features = %#x", dev.Features())
	}
	var mac []byte
	for i := range 6 {
		mac = append(mac, byte(d.tr.Read(regConfig+uint64(i), 1)))
	}
	if !bytes.Equal(mac, dev.MAC[:]) {
		t.Errorf("mac = % x", mac)
	}
	if mtu := d.tr.Read(regConfig+10, 2); mtu != 65520 {
		t.Errorf("mtu = %d", mtu)
	}
	if pairs := d.tr.Read(regConfig+8, 2); pairs != 1 {
		t.Errorf("max_virtqueue_pairs = %d", pairs)
	}
}

func TestNetTransmit(t *testing.T) {
	_, d, frames := newNet(t)
	one, two := pattern(100, 1), pattern(3000, 2)

	// The header in a descriptor of its own, the frame split over two more.
	hdr := d.alloc(NetHeaderLen)
	a, b := d.alloc(40), d.alloc(60)
	copy(d.mem[a:], one[:40])
	copy(d.mem[b:], one[40:])
	d.add(netTxQueue, buf{hdr, NetHeaderLen, false}, buf{a, 40, false}, buf{b, 60, false})

	// Header and frame in one descriptor, as Linux sends with VERSION_1.
	c := d.alloc(NetHeaderLen + 3000)
	copy(d.mem[c+NetHeaderLen:], two)
	d.add(netTxQueue, buf{c, NetHeaderLen + 3000, false})

	// A chain holding nothing but a header carries no frame.
	d.add(netTxQueue, buf{d.alloc(NetHeaderLen), NetHeaderLen, false})

	d.kick(netTxQueue)
	if u := d.collect(netTxQueue); len(u) != 3 || u[0].len != 0 {
		t.Fatalf("used = %+v", u)
	}
	if len(frames.got) != 2 || !bytes.Equal(frames.got[0], one) || !bytes.Equal(frames.got[1], two) {
		t.Fatalf("got %d frames", len(frames.got))
	}
	if d.interrupts() != 1 {
		t.Errorf("interrupts = %d, want 1 for the batch", d.interrupts())
	}
}

func TestNetReceiveMergeable(t *testing.T) {
	dev, d, _ := newNet(t)
	rx := postRx(d, 4, 1000)
	d.kick(netRxQueue)

	frame := pattern(2500, 3)
	dev.Deliver(frame)
	u := d.collect(netRxQueue)
	if len(u) != 3 || u[0].len != 1000 || u[1].len != 1000 || u[2].len != NetHeaderLen+2500-2000 {
		t.Fatalf("used = %+v", u)
	}
	if n := le.Uint16(d.mem[rx[0]+10:]); n != 3 {
		t.Errorf("num_buffers = %d", n)
	}
	var got []byte
	got = append(got, d.mem[rx[0]+NetHeaderLen:rx[0]+1000]...)
	got = append(got, d.mem[rx[1]:rx[1]+1000]...)
	got = append(got, d.mem[rx[2]:rx[2]+uint64(u[2].len)]...)
	if !bytes.Equal(got, frame) {
		t.Error("frame corrupted")
	}
	if dev.RxFrames.Load() != 1 || dev.RxBytes.Load() != 2500 || d.interrupts() != 1 {
		t.Errorf("frames %d bytes %d interrupts %d", dev.RxFrames.Load(), dev.RxBytes.Load(), d.interrupts())
	}

	// The fourth buffer is too small for this frame on its own: it waits,
	// with the buffer left posted, until more room arrives.
	dev.Deliver(pattern(1500, 4))
	if u := d.collect(netRxQueue); len(u) != 0 {
		t.Fatalf("frame delivered without room: %+v", u)
	}
	postRx(d, 1, 1000)
	d.kick(netRxQueue)
	if u := d.collect(netRxQueue); len(u) != 2 || u[0].len != 1000 || u[1].len != NetHeaderLen+1500-1000 {
		t.Fatalf("after more room: used = %+v", u)
	}
}

func TestNetBacklogKeepsOrder(t *testing.T) {
	dev, d, _ := newNet(t)
	first, second := pattern(50, 5), pattern(60, 6)
	dev.Deliver(first)
	dev.Deliver(second)
	rx := postRx(d, 2, 2048)
	if u := d.collect(netRxQueue); len(u) != 0 {
		t.Fatal("delivered before the kick")
	}
	d.kick(netRxQueue)
	u := d.collect(netRxQueue)
	if len(u) != 2 {
		t.Fatalf("used = %+v", u)
	}
	if !bytes.Equal(d.mem[rx[0]+NetHeaderLen:rx[0]+uint64(u[0].len)], first) ||
		!bytes.Equal(d.mem[rx[1]+NetHeaderLen:rx[1]+uint64(u[1].len)], second) {
		t.Error("frames out of order or corrupted")
	}
}

func TestNetBacklogLimit(t *testing.T) {
	dev, d, _ := newNet(t)
	dev.Backlog = 100
	dev.Deliver(pattern(60, 1))
	dev.Deliver(pattern(60, 2)) // over the limit
	dev.Deliver(pattern(40, 3)) // fits
	if dev.RxDropped.Load() != 1 {
		t.Fatalf("dropped = %d", dev.RxDropped.Load())
	}
	postRx(d, 4, 2048)
	d.kick(netRxQueue)
	if u := d.collect(netRxQueue); len(u) != 2 || u[0].len != NetHeaderLen+60 || u[1].len != NetHeaderLen+40 {
		t.Fatalf("used = %+v", u)
	}
}

func TestNetWithoutMergeableBuffers(t *testing.T) {
	dev, d, _ := newNet(t)
	d.tr.Write(regDrvFeatSel, 4, 0)
	d.tr.Write(regDrvFeatures, 4, uint64(uint32(dev.Features()&^netFeatMrgRxbuf)))

	postRx(d, 1, 100)
	rx := postRx(d, 1, 100)
	d.kick(netRxQueue)
	dev.Deliver(pattern(200, 1)) // can never fit in one chain
	dev.Deliver(pattern(50, 2))
	u := d.collect(netRxQueue)
	if len(u) != 2 || u[0].len != 0 || u[1].len != NetHeaderLen+50 {
		t.Fatalf("used = %+v", u)
	}
	if n := le.Uint16(d.mem[rx[0]+10:]); n != 1 {
		t.Errorf("num_buffers = %d", n)
	}
	if dev.RxDropped.Load() != 1 || dev.RxFrames.Load() != 1 {
		t.Errorf("dropped %d delivered %d", dev.RxDropped.Load(), dev.RxFrames.Load())
	}
}

func TestNetResetAndDrain(t *testing.T) {
	dev, d, _ := newNet(t)
	dev.Deliver(pattern(10, 1))
	d.tr.Write(regStatus, 4, 0) // reset
	if dev.RxDropped.Load() != 1 {
		t.Errorf("reset kept the backlog: dropped = %d", dev.RxDropped.Load())
	}

	d = newDriver(t, dev)
	dev.Deliver(pattern(10, 2))
	if !dev.Drain(context.Background()) {
		t.Error("Drain reported busy")
	}
	postRx(d, 1, 2048)
	d.kick(netRxQueue)
	dev.Deliver(pattern(10, 3))
	if u := d.collect(netRxQueue); len(u) != 0 {
		t.Errorf("delivered after Drain: %+v", u)
	}
	if dev.RxDropped.Load() != 3 {
		t.Errorf("dropped = %d, want 3", dev.RxDropped.Load())
	}

	var unattached Net
	unattached.Deliver([]byte{1})
	if unattached.RxDropped.Load() != 1 {
		t.Error("frame for an unattached device not counted as dropped")
	}
}

// usedWatcher records a ring's used index each time the device slices that
// ring, as it does to return each chain.
type usedWatcher struct {
	testMem
	ring *uint64
	seen []uint16
}

func (w *usedWatcher) Slice(addr, n uint64) ([]byte, bool) {
	if w.ring != nil && addr == *w.ring {
		w.seen = append(w.seen, le.Uint16(w.testMem[addr+2:]))
	}
	return w.testMem.Slice(addr, n)
}

// A frame spread over several receive chains must reach the guest whole: a
// guest polling the ring that sees the first chain, whose header counts
// three, before the other two drops the frame as missing buffers.
func TestNetReceivePublishesFrameWhole(t *testing.T) {
	w := &usedWatcher{}
	dev := &Net{MTU: 65520}
	d := newDriverWrapped(t, dev, func(m testMem) Memory { w.testMem = m; return w })
	postRx(d, 4, 1000)
	d.kick(netRxQueue)
	w.ring = &d.queues[netRxQueue].used

	dev.Deliver(pattern(2500, 5))
	for i, idx := range w.seen {
		if idx != 0 {
			t.Fatalf("used index was %d while the frame's chains were being returned (look %d of %d)", idx, i, len(w.seen))
		}
	}
	if len(w.seen) == 0 {
		t.Fatal("the used ring was never touched")
	}
	if u := d.collect(netRxQueue); len(u) != 3 {
		t.Fatalf("used = %+v", u)
	}
}
