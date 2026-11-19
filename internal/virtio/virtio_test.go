package virtio

import (
	"bytes"
	"testing"
)

type testMem []byte

func (m testMem) Slice(addr, n uint64) ([]byte, bool) {
	if addr+n < addr || addr+n > uint64(len(m)) {
		return nil, false
	}
	return m[addr : addr+n], true
}

// driver plays the guest's side of the transport.
type driver struct {
	t      *testing.T
	tr     *Transport
	mem    testMem
	next   uint64 // bump allocator for rings and buffers
	queues map[int]*driverQueue
	irqs   int
}

type driverQueue struct {
	size              uint16
	desc, avail, used uint64
	nextDesc          uint16
	availIdx, usedIdx uint16
}

type usedElem struct {
	head uint16
	len  uint32
}

func newDriver(t *testing.T, dev Device) *driver {
	d := &driver{t: t, mem: make(testMem, 1<<20), next: 0x1000, queues: map[int]*driverQueue{}}
	d.tr = NewTransport(dev, d.mem, func() { d.irqs++ })
	if d.tr.Read(regMagic, 4) != 0x74726976 || d.tr.Read(regVersion, 4) != 2 || d.tr.Read(regDeviceID, 4) != uint64(dev.DeviceID()) {
		t.Fatal("bad virtio-mmio identification registers")
	}
	d.tr.Write(regStatus, 4, 1|2) // ACKNOWLEDGE | DRIVER
	d.tr.Write(regDevFeatSel, 4, 1)
	hi := d.tr.Read(regDevFeatures, 4)
	d.tr.Write(regDevFeatSel, 4, 0)
	lo := d.tr.Read(regDevFeatures, 4)
	if hi<<32|lo != dev.Features() {
		t.Fatalf("features read back as %#x, want %#x", hi<<32|lo, dev.Features())
	}
	d.tr.Write(regDrvFeatSel, 4, 1)
	d.tr.Write(regDrvFeatures, 4, hi)
	d.tr.Write(regDrvFeatSel, 4, 0)
	d.tr.Write(regDrvFeatures, 4, lo)
	d.tr.Write(regStatus, 4, 1|2|8) // FEATURES_OK
	for i := 0; i < dev.NumQueues(); i++ {
		d.setupQueue(i, 16)
	}
	d.tr.Write(regStatus, 4, 1|2|8|4) // DRIVER_OK
	return d
}

func (d *driver) alloc(n uint64) uint64 {
	at := d.next
	d.next = (d.next + n + 15) &^ 15
	return at
}

func (d *driver) setupQueue(i int, size uint16) {
	q := &driverQueue{size: size}
	q.desc = d.alloc(16 * uint64(size))
	q.avail = d.alloc(6 + 2*uint64(size))
	q.used = d.alloc(6 + 8*uint64(size))
	tr := d.tr
	tr.Write(regQueueSel, 4, uint64(i))
	if tr.Read(regQueueNumMax, 4) != MaxQueueSize {
		d.t.Fatalf("queue %d: bad QueueNumMax", i)
	}
	tr.Write(regQueueNum, 4, uint64(size))
	tr.Write(regQueueDescLo, 4, q.desc)
	tr.Write(regQueueDescHi, 4, 0)
	tr.Write(regQueueDriverLo, 4, q.avail)
	tr.Write(regQueueDeviceLo, 4, q.used)
	tr.Write(regQueueReady, 4, 1)
	d.queues[i] = q
}

type buf struct {
	addr  uint64
	n     uint32
	write bool
}

// add makes a chain available on queue qi without kicking.
func (d *driver) add(qi int, bufs ...buf) uint16 {
	q := d.queues[qi]
	head := q.nextDesc
	for i, b := range bufs {
		idx := q.nextDesc % q.size
		q.nextDesc++
		e := d.mem[q.desc+16*uint64(idx):]
		le.PutUint64(e[0:], b.addr)
		le.PutUint32(e[8:], b.n)
		var flags uint16
		if b.write {
			flags |= descWrite
		}
		if i < len(bufs)-1 {
			flags |= descNext
			le.PutUint16(e[14:], q.nextDesc%q.size)
		}
		le.PutUint16(e[12:], flags)
	}
	le.PutUint16(d.mem[q.avail+4+2*uint64(q.availIdx%q.size):], head%q.size)
	q.availIdx++
	le.PutUint16(d.mem[q.avail+2:], q.availIdx)
	return head % q.size
}

func (d *driver) kick(qi int) { d.tr.Write(regQueueNotify, 4, uint64(qi)) }

// collect returns used elements the device has added since the last call.
func (d *driver) collect(qi int) []usedElem {
	q := d.queues[qi]
	var out []usedElem
	for q.usedIdx != le.Uint16(d.mem[q.used+2:]) {
		e := d.mem[q.used+4+8*uint64(q.usedIdx%q.size):]
		out = append(out, usedElem{uint16(le.Uint32(e[0:])), le.Uint32(e[4:])})
		q.usedIdx++
	}
	return out
}

// memFile is an in-memory BlockFile.
type memFile struct {
	data   []byte
	synced int
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error)  { return copy(p, f.data[off:]), nil }
func (f *memFile) WriteAt(p []byte, off int64) (int, error) { return copy(f.data[off:], p), nil }
func (f *memFile) Sync() error                              { f.synced++; return nil }

func blkRequest(d *driver, kind uint32, sector uint64, data buf) (status byte, used usedElem) {
	hdr := d.alloc(16)
	le.PutUint32(d.mem[hdr:], kind)
	le.PutUint64(d.mem[hdr+8:], sector)
	st := d.alloc(1)
	d.mem[st] = 0xff
	bufs := []buf{{hdr, 16, false}}
	if data.n > 0 {
		bufs = append(bufs, data)
	}
	bufs = append(bufs, buf{st, 1, true})
	head := d.add(0, bufs...)
	d.kick(0)
	u := d.collect(0)
	if len(u) != 1 || u[0].head != head {
		d.t.Fatalf("used ring = %+v, want one entry for head %d", u, head)
	}
	return d.mem[st], u[0]
}

func TestBlk(t *testing.T) {
	file := &memFile{data: make([]byte, 8*512)}
	for i := range file.data {
		file.data[i] = byte(i / 512)
	}
	dev := &Blk{File: file, Sectors: 8, ReadOnly: true, Serial: "layer3"}
	d := newDriver(t, dev)

	if cap := d.tr.Read(regConfig, 4) | d.tr.Read(regConfig+4, 4)<<32; cap != 8 {
		t.Errorf("capacity = %d", cap)
	}

	data := d.alloc(1024)
	status, u := blkRequest(d, blkIn, 2, buf{data, 1024, true})
	if status != blkOK || u.len != 1025 || d.mem[data] != 2 || d.mem[data+1023] != 3 {
		t.Errorf("read: status %d, len %d, data %d..%d", status, u.len, d.mem[data], d.mem[data+1023])
	}
	if d.irqs != 1 {
		t.Errorf("irqs = %d after one request", d.irqs)
	}

	status, _ = blkRequest(d, blkOut, 0, buf{data, 512, false})
	if status != blkErr || file.data[0] != 0 {
		t.Error("write to a read-only disk succeeded")
	}

	id := d.alloc(blkIDLen)
	copy(d.mem[id:], bytes.Repeat([]byte{'x'}, blkIDLen))
	status, _ = blkRequest(d, blkGetID, 0, buf{id, blkIDLen, true})
	if status != blkOK || string(d.mem[id:id+blkIDLen]) != "layer3\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" {
		t.Errorf("GET_ID = %q", d.mem[id:id+blkIDLen])
	}

	status, _ = blkRequest(d, 99, 0, buf{})
	if status != blkUnsupported {
		t.Errorf("unknown request status = %d", status)
	}
}

func TestBlkWriteAndFlush(t *testing.T) {
	file := &memFile{data: make([]byte, 4*512)}
	d := newDriver(t, &Blk{File: file, Sectors: 4})
	data := d.alloc(512)
	copy(d.mem[data:], bytes.Repeat([]byte{7}, 512))
	if status, _ := blkRequest(d, blkOut, 3, buf{data, 512, false}); status != blkOK || file.data[3*512] != 7 {
		t.Error("write failed")
	}
	if status, _ := blkRequest(d, blkFlush, 0, buf{}); status != blkOK || file.synced != 1 {
		t.Error("flush failed")
	}
}

func TestBadChainIsReturned(t *testing.T) {
	d := newDriver(t, &Blk{File: &memFile{data: make([]byte, 512)}, Sectors: 1})
	head := d.add(0, buf{uint64(len(d.mem)) - 4, 16, false}, buf{0, 1, true})
	d.kick(0)
	if u := d.collect(0); len(u) != 1 || u[0].head != head || u[0].len != 0 {
		t.Errorf("used = %+v", u)
	}
}

func TestResetClearsQueues(t *testing.T) {
	d := newDriver(t, &Blk{File: &memFile{data: make([]byte, 512)}, Sectors: 1})
	d.tr.Write(regStatus, 4, 0)
	if d.tr.Queue(0) != nil || d.tr.Read(regStatus, 4) != 0 {
		t.Error("reset left the queue ready")
	}
}

type ctrlMsg struct {
	id           uint32
	event, value uint16
	name         string
}

func TestConsole(t *testing.T) {
	var stdout, stderr bytes.Buffer
	con := &Console{Ports: []Port{{Name: "stdout", Out: &stdout}, {Name: "stderr", Out: &stderr}, {Name: "control"}}}
	d := newDriver(t, con)
	if n := d.tr.Read(regConfig+4, 4); n != 4 {
		t.Fatalf("max_nr_ports = %d", n)
	}
	if con.NumQueues() != 10 {
		t.Fatalf("NumQueues = %d", con.NumQueues())
	}

	// The driver posts control receive buffers, then reports DEVICE_READY.
	rx := map[uint16]uint64{}
	post := func() {
		at := d.alloc(64)
		rx[d.add(ctrlRxQueue, buf{at, 64, true})] = at
	}
	for i := 0; i < 4; i++ {
		post()
	}
	d.kick(ctrlRxQueue)
	send := func(id uint32, event, value uint16) {
		at := d.alloc(8)
		le.PutUint32(d.mem[at:], id)
		le.PutUint16(d.mem[at+4:], event)
		le.PutUint16(d.mem[at+6:], value)
		d.add(ctrlTxQueue, buf{at, 8, false})
		d.kick(ctrlTxQueue)
		if u := d.collect(ctrlTxQueue); len(u) != 1 {
			t.Fatalf("control message not consumed: %+v", u)
		}
	}
	receive := func() []ctrlMsg {
		var msgs []ctrlMsg
		for _, u := range d.collect(ctrlRxQueue) {
			b := d.mem[rx[u.head] : rx[u.head]+uint64(u.len)]
			msgs = append(msgs, ctrlMsg{le.Uint32(b[0:]), le.Uint16(b[4:]), le.Uint16(b[6:]), string(b[8:])})
			delete(rx, u.head)
			post()
		}
		return msgs
	}

	send(0xffffffff, ctrlDeviceReady, 1)
	want := []ctrlMsg{{1, ctrlPortAdd, 1, ""}, {2, ctrlPortAdd, 1, ""}, {3, ctrlPortAdd, 1, ""}}
	if got := receive(); !equalMsgs(got, want) {
		t.Fatalf("after DEVICE_READY got %+v, want %+v", got, want)
	}

	send(1, ctrlPortReady, 1)
	if got := receive(); !equalMsgs(got, []ctrlMsg{{1, ctrlPortName, 1, "stdout"}, {1, ctrlPortOpen, 1, ""}}) {
		t.Fatalf("after PORT_READY got %+v", got)
	}

	// A repeated PORT_READY is not answered twice.
	send(1, ctrlPortReady, 1)
	if got := receive(); len(got) != 0 {
		t.Errorf("repeated PORT_READY answered with %+v", got)
	}

	// Port 1 transmits on queue 5, port 2 on queue 7.
	data := d.alloc(32)
	copy(d.mem[data:], "hello\n")
	d.add(5, buf{data, 6, false})
	d.kick(5)
	copy(d.mem[data+16:], "oops")
	d.add(7, buf{data + 16, 4, false})
	d.kick(7)
	if stdout.String() != "hello\n" || stderr.String() != "oops" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if len(d.collect(5)) != 1 || len(d.collect(7)) != 1 {
		t.Error("transmit buffers not returned")
	}
}

func TestConsolePendingUntilBuffers(t *testing.T) {
	con := &Console{Ports: []Port{{Name: "a"}}}
	d := newDriver(t, con)
	at := d.alloc(8)
	le.PutUint16(d.mem[at+4:], ctrlDeviceReady)
	le.PutUint16(d.mem[at+6:], 1)
	d.add(ctrlTxQueue, buf{at, 8, false})
	d.kick(ctrlTxQueue)
	if len(con.pending) != 1 {
		t.Fatalf("pending = %d, want the PORT_ADD held back", len(con.pending))
	}
	rx := d.alloc(64)
	d.add(ctrlRxQueue, buf{rx, 64, true})
	d.kick(ctrlRxQueue)
	if len(con.pending) != 0 || le.Uint16(d.mem[rx+4:]) != ctrlPortAdd {
		t.Error("PORT_ADD not delivered once a buffer was posted")
	}
}

func TestPortForQueue(t *testing.T) {
	for q, want := range map[int][2]int{0: {0, 0}, 1: {0, 1}, 4: {1, 0}, 5: {1, 1}, 9: {3, 1}} {
		id, tx := portForQueue(q)
		if id != want[0] || tx != (want[1] == 1) {
			t.Errorf("queue %d -> port %d transmit %v", q, id, tx)
		}
	}
}

func equalMsgs(a, b []ctrlMsg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
