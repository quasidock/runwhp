package virtio

import (
	"bytes"
	"context"
	"testing"
)

// netFrames collects what a Net hands to Recv. Recv runs with the transport
// locked, and the tests read it after their kick returns, so no lock is needed.
type netFrames struct {
	got  [][]byte
	offs []Offload
}

func (f *netFrames) recv(frame []byte, off Offload) {
	f.got = append(f.got, frame)
	f.offs = append(f.offs, off)
}

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
	if want := uint64(netFeatMrgRxbuf | netFeatMTU | netFeatMAC | netFeatCsum | netFeatGuestCsum); dev.Features()&want != want {
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

// usedWatcher records a ring's used index each time the device slices the
// whole ring, as it does to return each chain.
type usedWatcher struct {
	testMem
	ring *uint64
	size uint64 // the ring's entries
	seen []uint16
}

func (w *usedWatcher) Slice(addr, n uint64) ([]byte, bool) {
	if w.ring != nil && addr == *w.ring && n == 4+8*w.size {
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
	w.size = uint64(d.queues[netRxQueue].size)

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

// Frames for a guest that takes them without checksums come marked valid,
// and GuestChecksums says so from the driver's first notification until it
// resets the device.
func TestNetChecksumOffload(t *testing.T) {
	dev, d, _ := newNet(t)
	if dev.GuestChecksums() {
		t.Error("GuestChecksums before the driver notified the device")
	}
	rx := postRx(d, 2, 1000)
	d.kick(netRxQueue)
	if !dev.GuestChecksums() {
		t.Error("GuestChecksums false for a driver that took VIRTIO_NET_F_GUEST_CSUM")
	}
	dev.Deliver(pattern(100, 1))
	if flags := d.mem[rx[0]]; flags != netHdrDataValid {
		t.Errorf("header flags %#x, want DATA_VALID", flags)
	}
	d.tr.Write(regStatus, 4, 0)
	if dev.GuestChecksums() {
		t.Error("GuestChecksums still true after a reset")
	}

	// A driver that does not take them gets frames with no flags.
	dev2, d2, _ := newNet(t)
	d2.tr.Write(regDrvFeatSel, 4, 0)
	d2.tr.Write(regDrvFeatures, 4, uint64(uint32(dev2.Features()&^netFeatGuestCsum)))
	rx2 := postRx(d2, 1, 1000)
	d2.kick(netRxQueue)
	if dev2.GuestChecksums() {
		t.Error("GuestChecksums true for a driver that did not take VIRTIO_NET_F_GUEST_CSUM")
	}
	dev2.Deliver(pattern(100, 2))
	if flags := d2.mem[rx2[0]]; flags != 0 {
		t.Errorf("header flags %#x for a driver without GUEST_CSUM", flags)
	}
}

// udpFrame is an Ethernet frame holding an IPv4 UDP datagram whose
// checksum is partial, as Linux leaves one it offloads: the field holds the
// pseudo-header's sum, and the offload says where.
func udpFrame(payload []byte) ([]byte, Offload) {
	f := make([]byte, 14+20+8+len(payload))
	f[12], f[13] = 0x08, 0x00
	ip := f[14:]
	ip[0] = 0x45
	ulen := 8 + len(payload)
	ip[9] = 17
	copy(ip[12:], []byte{172, 18, 0, 2})
	copy(ip[16:], []byte{172, 18, 0, 3})
	u := ip[20:]
	u[0], u[1], u[2], u[3] = 0x30, 0x39, 0x00, 0x35
	u[4], u[5] = byte(ulen>>8), byte(ulen)
	copy(u[8:], payload)
	pseudo := uint32(172<<8|18) + uint32(0<<8|2) + uint32(172<<8|18) + uint32(0<<8|3) + 17 + uint32(ulen)
	for pseudo>>16 != 0 {
		pseudo = pseudo&0xffff + pseudo>>16
	}
	u[6], u[7] = byte(pseudo>>8), byte(pseudo)
	return f, Offload{NeedsCsum: true, Start: 34, Offset: 6}
}

// udpValid reports whether a frame from udpFrame now has a good checksum.
func udpValid(f []byte) bool {
	ip := f[14:]
	u := ip[20:]
	sum := uint32(ip[12])<<8 | uint32(ip[13])
	sum += uint32(ip[14])<<8 | uint32(ip[15])
	sum += uint32(ip[16])<<8 | uint32(ip[17])
	sum += uint32(ip[18])<<8 | uint32(ip[19])
	sum += 17 + uint32(len(u))
	for i := 0; i < len(u); i += 2 {
		w := uint32(u[i]) << 8
		if i+1 < len(u) {
			w |= uint32(u[i+1])
		}
		sum += w
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return sum == 0xffff
}

func TestOffloadComplete(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 100, 1001} {
		f, off := udpFrame(pattern(n, byte(n)))
		if udpValid(f) && n > 0 {
			t.Fatalf("%d bytes: a partial checksum already checks", n)
		}
		if got := off.Complete(f); got != (Offload{}) {
			t.Errorf("Complete returned %+v", got)
		}
		if !udpValid(f) {
			t.Errorf("%d bytes: checksum wrong after Complete", n)
		}
	}
	if (Offload{NeedsCsum: true, Start: 34, Offset: 6}).Fits(41) {
		t.Error("an offload past the frame fits")
	}
	if !(Offload{}).Fits(0) {
		t.Error("no offload does not fit")
	}
}

// A frame's partial checksum reaches Recv with it, as the header says.
func TestNetTransmitOffload(t *testing.T) {
	dev, d, frames := newNet(t)
	f, off := udpFrame(pattern(30, 1))
	send := func(flags, gso byte, start, offset uint16) {
		hdr := d.alloc(NetHeaderLen)
		d.mem[hdr], d.mem[hdr+1] = flags, gso
		le.PutUint16(d.mem[hdr+6:], start)
		le.PutUint16(d.mem[hdr+8:], offset)
		// The frame split across two descriptors after the header's own.
		a, b := d.alloc(20), d.alloc(uint64(len(f)-20))
		copy(d.mem[a:], f[:20])
		copy(d.mem[b:], f[20:])
		d.add(netTxQueue, buf{hdr, NetHeaderLen, false}, buf{a, 20, false}, buf{b, uint32(len(f) - 20), false})
	}
	send(netHdrNeedsCsum, 0, off.Start, off.Offset)
	send(0, 0, 0, 0)
	send(netHdrNeedsCsum, 0, uint16(len(f)), 0) // past the frame
	send(0, 1, 0, 0)                            // GSO, which the device never offered
	d.kick(netTxQueue)
	if u := d.collect(netTxQueue); len(u) != 4 {
		t.Fatalf("used = %+v", u)
	}
	if len(frames.got) != 2 || !bytes.Equal(frames.got[0], f) || frames.offs[0] != off || frames.offs[1] != (Offload{}) {
		t.Fatalf("got %d frames, offloads %+v", len(frames.got), frames.offs)
	}
	if dev.TxDropped.Load() != 2 || dev.TxFrames.Load() != 2 {
		t.Errorf("dropped %d passed %d", dev.TxDropped.Load(), dev.TxFrames.Load())
	}
}

// A frame with a partial checksum reaches a guest that takes them as it
// is, with the header saying where to finish it, and one that does not
// finished.
func TestNetDeliverOffload(t *testing.T) {
	dev, d, _ := newNet(t)
	rx := postRx(d, 1, 2048)
	d.kick(netRxQueue)
	f, off := udpFrame(pattern(51, 3))
	sent := bytes.Clone(f)
	dev.DeliverWith(f, off)
	u := d.collect(netRxQueue)
	if len(u) != 1 {
		t.Fatalf("used = %+v", u)
	}
	h := d.mem[rx[0]:]
	if h[0] != netHdrNeedsCsum || le.Uint16(h[6:]) != off.Start || le.Uint16(h[8:]) != off.Offset {
		t.Errorf("header % x", h[:NetHeaderLen])
	}
	if !bytes.Equal(h[NetHeaderLen:NetHeaderLen+len(sent)], sent) {
		t.Error("frame changed for a guest that finishes checksums itself")
	}

	dev2, d2, _ := newNet(t)
	d2.tr.Write(regDrvFeatSel, 4, 0)
	d2.tr.Write(regDrvFeatures, 4, uint64(uint32(dev2.Features()&^netFeatGuestCsum)))
	// Delivered before the guest has room: it waits, and is finished once,
	// when it goes.
	f2, off2 := udpFrame(pattern(51, 3))
	dev2.DeliverWith(f2, off2)
	rx2 := postRx(d2, 1, 2048)
	d2.kick(netRxQueue)
	if u := d2.collect(netRxQueue); len(u) != 1 {
		t.Fatalf("used = %+v", u)
	}
	h2 := d2.mem[rx2[0]:]
	if h2[0] != 0 {
		t.Errorf("header flags %#x for a guest without GUEST_CSUM", h2[0])
	}
	if !udpValid(h2[NetHeaderLen : NetHeaderLen+len(f2)]) {
		t.Error("checksum not finished for a guest without GUEST_CSUM")
	}

	dev.DeliverWith(make([]byte, 20), Offload{NeedsCsum: true, Start: 14, Offset: 6})
	if dev.RxDropped.Load() != 1 {
		t.Error("a frame its offload does not fit was not dropped")
	}
}
