package virtio

import (
	"testing"
)

// ringDev is a device with one queue. Kicked, it takes every chain, unless
// lazy is set, in which case it takes chains only through take, as a
// receive queue is used. Each chain goes back with an interrupt of its own,
// unless batch is set, in which case one interrupt covers a kick's chains.
type ringDev struct {
	t     *Transport
	lazy  bool
	batch bool
	heads []uint16
}

func (r *ringDev) DeviceID() uint32    { return 0x7f }
func (r *ringDev) Features() uint64    { return FeatVersion1 }
func (r *ringDev) Config() []byte      { return nil }
func (r *ringDev) NumQueues() int      { return 1 }
func (r *ringDev) Reset()              {}
func (r *ringDev) Attach(t *Transport) { r.t = t }
func (r *ringDev) Notify(t *Transport, q int) {
	if !r.lazy {
		r.takeLocked(-1)
	}
}

// take returns up to n chains, or all there are if n is negative, and
// reports how many it took.
func (r *ringDev) take(n int) int {
	got := 0
	r.t.Do(func() { got = r.takeLocked(n) })
	return got
}

func (r *ringDev) takeLocked(n int) int {
	q := r.t.Queue(0)
	got := 0
	for n < 0 || got < n {
		c, ok, _ := q.Pop(r.t.Memory())
		if !ok {
			break
		}
		r.heads = append(r.heads, c.Head)
		q.Push(r.t.Memory(), c.Head, 0)
		if !r.batch {
			r.t.Interrupt(0)
		}
		got++
	}
	if r.batch && got > 0 {
		r.t.Interrupt(0)
	}
	return got
}

func setUsedEvent(d *driver, qi int, v uint16) {
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
	q := d.queues[qi]
	le.PutUint16(d.mem[q.avail+4+2*uint64(q.size):], v)
}

func availEvent(d *driver, qi int) uint16 {
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
	q := d.queues[qi]
	return le.Uint16(d.mem[q.used+4+8*uint64(q.size):])
}

func TestEventIdxOffered(t *testing.T) {
	for _, dev := range []Device{&Console{}, &Net{}, &Blk{}, &Fs{}, &Vsock{}, &Balloon{}} {
		d := newDriverOpts(t, dev, nil, true)
		if d.tr.DriverFeatures()&FeatRingEventIdx == 0 {
			t.Errorf("device %d: EVENT_IDX not taken", dev.DeviceID())
		}
		for i := 0; i < dev.NumQueues(); i++ {
			if !d.tr.queues[i].eventIdx {
				t.Errorf("device %d: queue %d does not use EVENT_IDX", dev.DeviceID(), i)
			}
		}
	}
	d := newDriverOpts(t, &ringDev{}, nil, false)
	if d.tr.queues[0].eventIdx {
		t.Error("a queue uses EVENT_IDX the driver declined")
	}
}

// The device interrupts only once the used index passes used_event.
func TestEventIdxInterrupts(t *testing.T) {
	r := &ringDev{}
	d := newDriverOpts(t, r, nil, true)
	setUsedEvent(d, 0, 5)
	for range 10 {
		d.add(0, buf{d.alloc(8), 8, false})
	}
	d.kick(0)
	// The first check always interrupts; then only the push that takes the
	// used index past 5, to 6.
	if n := d.interrupts(); n != 2 {
		t.Errorf("%d interrupts for 10 chains with used_event 5, want 2", n)
	}
	if u := d.collect(0); len(u) != 10 {
		t.Fatalf("collected %d", len(u))
	}
	// used_event is now 10, the driver having taken everything: the next
	// push interrupts, and those after it do not.
	for range 3 {
		d.add(0, buf{d.alloc(8), 8, false})
	}
	d.kick(0)
	if n := d.interrupts(); n != 3 {
		t.Errorf("%d interrupts after three more chains, want 3", n)
	}

	// One interrupt covering several chains interrupts as well.
	r.batch = true
	d.collect(0)
	for range 3 {
		d.add(0, buf{d.alloc(8), 8, false})
	}
	d.kick(0)
	if n := d.interrupts(); n != 4 {
		t.Errorf("%d interrupts after a batch, want 4", n)
	}
	// Nothing new: no interrupt.
	r.t.Do(func() { r.t.Interrupt(0) })
	if n := d.interrupts(); n != 4 {
		t.Errorf("%d interrupts with nothing new, want 4", n)
	}
}

// A device asks to be notified only once it has run out, so a driver
// refilling a queue the device has not emptied notifies nobody.
func TestEventIdxNotifications(t *testing.T) {
	r := &ringDev{lazy: true}
	d := newDriverOpts(t, r, nil, true)
	for range 8 {
		d.add(0, buf{d.alloc(8), 8, true})
	}
	if !d.kickIfNeeded(0) {
		t.Fatal("the first chains did not notify the device")
	}
	if got := r.take(3); got != 3 {
		t.Fatalf("took %d", got)
	}
	d.add(0, buf{d.alloc(8), 8, true})
	if d.kickIfNeeded(0) {
		t.Error("the driver notified a device that still had chains")
	}
	// Running out, the device asks for the next chain.
	if got := r.take(-1); got != 6 {
		t.Fatalf("took %d, want the 6 left", got)
	}
	if e := availEvent(d, 0); e != 9 {
		t.Errorf("avail_event = %d, want 9, the next chain", e)
	}
	d.add(0, buf{d.alloc(8), 8, true})
	if !d.kickIfNeeded(0) {
		t.Error("the driver did not notify a device that had run out")
	}

	// Without EVENT_IDX the driver notifies every time, as before, and
	// avail_event is not touched.
	r2 := &ringDev{lazy: true}
	d2 := newDriverOpts(t, r2, nil, false)
	d2.add(0, buf{d2.alloc(8), 8, true})
	r2.take(-1)
	if e := availEvent(d2, 0); e != 0 {
		t.Errorf("avail_event = %d without EVENT_IDX", e)
	}
}

// availRacer adds a chain to the available ring the moment the device
// writes avail_event: as a driver on another vCPU may, having read the
// old value, and so without notifying.
type availRacer struct {
	testMem
	d    *driver
	once func()
}

func (m *availRacer) Slice(addr, n uint64) ([]byte, bool) {
	if m.once != nil && m.d != nil {
		if q := m.d.queues[0]; addr == q.used && n == 6+8*uint64(q.size) {
			f := m.once
			m.once = nil
			f()
		}
	}
	return m.testMem.Slice(addr, n)
}

func TestEventIdxRace(t *testing.T) {
	r := &ringDev{lazy: true}
	m := &availRacer{}
	d := newDriverOpts(t, r, func(tm testMem) Memory { m.testMem = tm; return m }, true)
	m.d = d
	d.add(0, buf{d.alloc(8), 8, true})
	var late uint16
	m.once = func() {
		// The transport's lock is held: add the chain by hand.
		q := d.queues[0]
		late = q.nextDesc % q.size
		e := d.mem[q.desc+16*uint64(late):]
		le.PutUint64(e[0:], d.alloc(8))
		le.PutUint32(e[8:], 8)
		le.PutUint16(e[12:], descWrite)
		q.nextDesc++
		le.PutUint16(d.mem[q.avail+4+2*uint64(q.availIdx%q.size):], late)
		q.availIdx++
		le.PutUint16(d.mem[q.avail+2:], q.availIdx)
	}
	if got := r.take(-1); got != 2 {
		t.Fatalf("took %d chains, want 2: the one added while avail_event was being set is lost", got)
	}
	if r.heads[1] != late {
		t.Errorf("second chain %d, want %d", r.heads[1], late)
	}
}

// Indices wrap at 65536, used_event and avail_event with them.
func TestEventIdxWrap(t *testing.T) {
	r := &ringDev{}
	d := newDriverOpts(t, r, nil, true)
	buffer := d.alloc(8)
	for i := 0; i < 70000; i++ {
		d.add(0, buf{buffer, 8, false})
		if !d.kickIfNeeded(0) {
			t.Fatalf("chain %d: the driver did not notify a device that had run out", i)
		}
		if n := d.interrupts(); n != i+1 {
			t.Fatalf("chain %d: %d interrupts, want %d", i, n, i+1)
		}
		if u := d.collect(0); len(u) != 1 {
			t.Fatalf("chain %d: collected %d", i, len(u))
		}
	}
}

// Without EVENT_IDX, VIRTQ_AVAIL_F_NO_INTERRUPT still holds interrupts back.
func TestNoInterruptFlag(t *testing.T) {
	r := &ringDev{}
	d := newDriverOpts(t, r, nil, false)
	d.tr.mu.Lock()
	le.PutUint16(d.mem[d.queues[0].avail:], 1)
	d.tr.mu.Unlock()
	d.add(0, buf{d.alloc(8), 8, false})
	d.kick(0)
	if n := d.interrupts(); n != 0 {
		t.Errorf("%d interrupts with VIRTQ_AVAIL_F_NO_INTERRUPT set", n)
	}
}

// A reset forgets the interrupt state, so the first check after it
// interrupts whatever used_event says.
func TestEventIdxReset(t *testing.T) {
	r := &ringDev{}
	d := newDriverOpts(t, r, nil, true)
	d.add(0, buf{d.alloc(8), 8, false})
	d.kick(0)
	d.tr.Write(regStatus, 4, 0)
	d.tr.Write(regStatus, 4, 1|2)
	d.tr.Write(regDrvFeatSel, 4, 0)
	d.tr.Write(regDrvFeatures, 4, FeatRingEventIdx)
	d.tr.Write(regDrvFeatSel, 4, 1)
	d.tr.Write(regDrvFeatures, 4, FeatVersion1>>32)
	d.tr.Write(regStatus, 4, 1|2|8)
	d.setupQueue(0, 16)
	d.tr.Write(regStatus, 4, 1|2|8|4)
	setUsedEvent(d, 0, 100)
	before := d.interrupts()
	d.add(0, buf{d.alloc(8), 8, false})
	d.kick(0)
	if n := d.interrupts() - before; n != 1 {
		t.Errorf("%d interrupts for the first chain after a reset, want 1", n)
	}
}

// TestDevicesWithEventIdx runs each device's tests again with a driver that
// takes EVENT_IDX, as Linux does.
func TestDevicesWithEventIdx(t *testing.T) {
	testEventIdx = true
	defer func() { testEventIdx = false }()
	for _, tc := range []struct {
		name string
		f    func(*testing.T)
	}{
		{"BalloonReports", TestBalloonReports}, {"BalloonBadChain", TestBalloonBadChain},
		{"BalloonIdleQueues", TestBalloonIdleQueues}, {"BalloonReset", TestBalloonReset},
		{"Blk", TestBlk}, {"BlkWriteAndFlush", TestBlkWriteAndFlush}, {"BlkBounds", TestBlkBounds},
		{"BlkReadError", TestBlkReadError}, {"BadChainIsReturned", TestBadChainIsReturned},
		{"ResetClearsQueues", TestResetClearsQueues}, {"BlkKickServesOne", TestBlkKickServesOne},
		{"BlkOutOfOrder", TestBlkOutOfOrder}, {"BlkResetWithRequestsInFlight", TestBlkResetWithRequestsInFlight},
		{"BlkDrain", TestBlkDrain}, {"BlkHeaderCopied", TestBlkHeaderCopied}, {"BlkDiscard", TestBlkDiscard},
		{"FsRequest", TestFsRequest}, {"FsOutOfOrder", TestFsOutOfOrder}, {"FsInline", TestFsInline},
		{"FsHiprio", TestFsHiprio}, {"FsResetDropsInflight", TestFsResetDropsInflight}, {"FsDrain", TestFsDrain},
		{"NetTransmit", TestNetTransmit}, {"NetReceiveMergeable", TestNetReceiveMergeable},
		{"NetBacklogKeepsOrder", TestNetBacklogKeepsOrder}, {"NetBacklogLimit", TestNetBacklogLimit},
		{"NetResetAndDrain", TestNetResetAndDrain}, {"NetReceivePublishesFrameWhole", TestNetReceivePublishesFrameWhole},
		{"Console", TestConsole}, {"ConsoleOutputIsAsync", TestConsoleOutputIsAsync},
		{"ConsoleInput", TestConsoleInput}, {"ConsoleInputWaitsForOpen", TestConsoleInputWaitsForOpen},
		{"ConsoleConcurrent", TestConsoleConcurrent}, {"ConsolePendingUntilBuffers", TestConsolePendingUntilBuffers},
		{"VsockRefusedAndTimeout", TestVsockRefusedAndTimeout}, {"VsockReadWrite", TestVsockReadWrite},
		{"VsockCredit", TestVsockCredit}, {"VsockHostCredit", TestVsockHostCredit},
		{"VsockSmallBuffers", TestVsockSmallBuffers}, {"VsockStrayPackets", TestVsockStrayPackets},
		{"VsockClose", TestVsockClose}, {"VsockFairness", TestVsockFairness}, {"VsockResetAndDrain", TestVsockResetAndDrain},
	} {
		t.Run(tc.name, tc.f)
	}
}
