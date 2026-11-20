package virtio

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
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

// add makes a chain available on queue qi without kicking. Like collect, it
// holds the transport's lock, because device goroutines use the rings too.
func (d *driver) add(qi int, bufs ...buf) uint16 {
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
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
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
	q := d.queues[qi]
	var out []usedElem
	for q.usedIdx != le.Uint16(d.mem[q.used+2:]) {
		e := d.mem[q.used+4+8*uint64(q.usedIdx%q.size):]
		out = append(out, usedElem{uint16(le.Uint32(e[0:])), le.Uint32(e[4:])})
		q.usedIdx++
	}
	return out
}

// interrupts returns how many interrupts the device has raised.
func (d *driver) interrupts() int {
	d.tr.mu.Lock()
	defer d.tr.mu.Unlock()
	return d.irqs
}

// collectN waits until queue qi has returned n more chains.
func (d *driver) collectN(qi, n int) []usedElem {
	var out []usedElem
	waitFor(d.t, "used buffers", func() bool {
		out = append(out, d.collect(qi)...)
		return len(out) >= n
	})
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// conDriver plays the guest's console driver: it keeps receive buffers posted
// on the control queue and speaks the control protocol.
type conDriver struct {
	*driver
	rx map[uint16]uint64 // control receive buffers by head
}

func newConDriver(t *testing.T, con *Console) *conDriver {
	c := &conDriver{driver: newDriver(t, con), rx: map[uint16]uint64{}}
	for i := 0; i < 4; i++ {
		c.post()
	}
	c.kick(ctrlRxQueue)
	return c
}

func (c *conDriver) post() {
	at := c.alloc(64)
	c.rx[c.add(ctrlRxQueue, buf{at, 64, true})] = at
}

func (c *conDriver) send(id uint32, event, value uint16) {
	at := c.alloc(8)
	le.PutUint32(c.mem[at:], id)
	le.PutUint16(c.mem[at+4:], event)
	le.PutUint16(c.mem[at+6:], value)
	c.add(ctrlTxQueue, buf{at, 8, false})
	c.kick(ctrlTxQueue)
	if u := c.collect(ctrlTxQueue); len(u) != 1 {
		c.t.Fatalf("control message not consumed: %+v", u)
	}
}

func (c *conDriver) receive() []ctrlMsg {
	var msgs []ctrlMsg
	for _, u := range c.collect(ctrlRxQueue) {
		b := c.mem[c.rx[u.head] : c.rx[u.head]+uint64(u.len)]
		msgs = append(msgs, ctrlMsg{le.Uint32(b[0:]), le.Uint16(b[4:]), le.Uint16(b[6:]), string(b[8:])})
		delete(c.rx, u.head)
		c.post()
	}
	if len(msgs) > 0 {
		c.kick(ctrlRxQueue)
	}
	return msgs
}

// ready adds every port, the way the Linux driver does at probe.
func (c *conDriver) ready(ports int) {
	c.send(0xffffffff, ctrlDeviceReady, 1)
	c.receive()
	for id := 1; id <= ports; id++ {
		c.send(uint32(id), ctrlPortReady, 1)
	}
	c.receive()
}

func TestConsole(t *testing.T) {
	var stdout, stderr syncBuffer
	con := &Console{Ports: []Port{{Name: "stdout", Out: &stdout}, {Name: "stderr", Out: &stderr}, {Name: "control"}}}
	d := newConDriver(t, con)
	if n := d.tr.Read(regConfig+4, 4); n != 4 {
		t.Fatalf("max_nr_ports = %d", n)
	}
	if con.NumQueues() != 10 {
		t.Fatalf("NumQueues = %d", con.NumQueues())
	}

	d.send(0xffffffff, ctrlDeviceReady, 1)
	want := []ctrlMsg{{1, ctrlPortAdd, 1, ""}, {2, ctrlPortAdd, 1, ""}, {3, ctrlPortAdd, 1, ""}}
	if got := d.receive(); !equalMsgs(got, want) {
		t.Fatalf("after DEVICE_READY got %+v, want %+v", got, want)
	}

	d.send(1, ctrlPortReady, 1)
	if got := d.receive(); !equalMsgs(got, []ctrlMsg{{1, ctrlPortName, 1, "stdout"}, {1, ctrlPortOpen, 1, ""}}) {
		t.Fatalf("after PORT_READY got %+v", got)
	}

	// A repeated PORT_READY is not answered twice.
	d.send(1, ctrlPortReady, 1)
	if got := d.receive(); len(got) != 0 {
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
	d.collectN(5, 1)
	d.collectN(7, 1)
	if stdout.String() != "hello\n" || stderr.String() != "oops" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if !con.Drain(context.Background()) {
		t.Error("Drain reported the console busy")
	}
	if con.BytesWritten.Load() != 10 {
		t.Errorf("BytesWritten = %d", con.BytesWritten.Load())
	}
}

// gate is a Writer that blocks until opened.
type gate struct {
	open chan struct{}
	syncBuffer
}

func (g *gate) Write(p []byte) (int, error) {
	<-g.open
	return g.syncBuffer.Write(p)
}

// TestConsoleOutputIsAsync checks that a blocked Out holds up neither the
// guest's kick nor other ports, that its chains come back once Out moves,
// and that Drain waits for them.
func TestConsoleOutputIsAsync(t *testing.T) {
	slow := &gate{open: make(chan struct{})}
	var fast syncBuffer
	con := &Console{Ports: []Port{{Name: "slow", Out: slow}, {Name: "fast", Out: &fast}}}
	d := newConDriver(t, con)
	d.ready(2)

	data := d.alloc(16)
	copy(d.mem[data:], "ab")
	for i := 0; i < 3; i++ {
		d.add(5, buf{data, 1, false})
	}
	d.kick(5) // returns although Out is blocked
	d.add(7, buf{data + 1, 1, false})
	d.kick(7)
	d.collectN(7, 1)
	if fast.String() != "b" {
		t.Errorf("fast port got %q", fast.String())
	}
	if u := d.collect(5); len(u) != 0 {
		t.Fatalf("chains returned before Out took them: %+v", u)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if con.Drain(ctx) {
		t.Fatal("Drain reported idle while Out was blocked")
	}
	close(slow.open)
	if !con.Drain(context.Background()) {
		t.Fatal("Drain failed after Out unblocked")
	}
	if u := d.collect(5); len(u) != 3 || slow.String() != "aaa" {
		t.Errorf("after drain: used %+v, output %q", u, slow.String())
	}
	if d.interrupts() == 0 {
		t.Error("no interrupt for returned chains")
	}
}

// TestConsoleInput sends a reader's contents to a port across several
// receive buffers, only once the guest has opened it, and closes the host
// side after the last byte.
func TestConsoleInput(t *testing.T) {
	input := strings.Repeat("0123456789", 1000) // 10000 bytes
	con := &Console{Ports: []Port{{Name: "stdin", In: strings.NewReader(input)}}}
	d := newConDriver(t, con)
	d.ready(1)

	const rxq, size = 4, 4096
	var got []byte
	bufs := map[uint16]uint64{}
	postData := func() {
		at := d.alloc(size)
		bufs[d.add(rxq, buf{at, size, true})] = at
	}
	take := func(n int) {
		for _, u := range d.collectN(rxq, n) {
			got = append(got, d.mem[bufs[u.head]:bufs[u.head]+uint64(u.len)]...)
		}
	}
	postData()
	postData()
	d.kick(rxq)
	time.Sleep(20 * time.Millisecond)
	if u := d.collect(rxq); len(u) != 0 {
		t.Fatalf("data sent before the guest opened the port: %+v", u)
	}

	d.send(1, ctrlPortOpen, 1)
	take(2)
	if !con.Connected("stdin") || con.Connected("other") {
		t.Error("Connected is wrong")
	}
	if msgs := d.receive(); len(msgs) != 0 {
		t.Fatalf("control messages with input pending: %+v", msgs)
	}
	postData()
	d.kick(rxq)
	take(1)
	if string(got) != input {
		t.Fatalf("received %d bytes, want %d", len(got), len(input))
	}
	var msgs []ctrlMsg
	waitFor(t, "end of input", func() bool { msgs = append(msgs, d.receive()...); return len(msgs) > 0 })
	if !equalMsgs(msgs, []ctrlMsg{{1, ctrlPortOpen, 0, ""}}) {
		t.Errorf("after input ended got %+v", msgs)
	}
	if con.BytesRead.Load() != uint64(len(input)) {
		t.Errorf("BytesRead = %d", con.BytesRead.Load())
	}
	con.Drain(context.Background())
	if con.Connected("stdin") {
		t.Error("Connected after Drain")
	}
}

// TestConsoleInputWaitsForOpen holds input while the guest has the port
// closed and resumes when it reopens.
func TestConsoleInputWaitsForOpen(t *testing.T) {
	r, w := io.Pipe()
	con := &Console{Ports: []Port{{Name: "in", In: r}}}
	d := newConDriver(t, con)
	d.ready(1)
	at := d.alloc(64)
	d.add(4, buf{at, 64, true})
	d.kick(4)
	d.send(1, ctrlPortOpen, 1)
	d.send(1, ctrlPortOpen, 0)
	go w.Write([]byte("hi"))
	time.Sleep(20 * time.Millisecond)
	if u := d.collect(4); len(u) != 0 {
		t.Fatalf("data sent to a closed port: %+v", u)
	}
	d.send(1, ctrlPortOpen, 1)
	if u := d.collectN(4, 1); u[0].len != 2 || string(d.mem[at:at+2]) != "hi" {
		t.Errorf("used %+v, data %q", u, d.mem[at:at+2])
	}
	w.Close()
	con.Drain(context.Background())
}

// TestConsoleConcurrent runs input, output and register traffic at once, for
// the race detector.
func TestConsoleConcurrent(t *testing.T) {
	const rounds, size, window = 200, 512, 8
	var out syncBuffer
	con := &Console{Ports: []Port{{Name: "io", In: strings.NewReader(strings.Repeat("x", rounds*size)), Out: &out}}}
	d := newConDriver(t, con)
	d.ready(1)
	d.send(1, ctrlPortOpen, 1)

	rx := d.alloc(size)
	tx := d.alloc(1)
	d.mem[tx] = 'y'
	received, pendingRx, pendingTx := 0, 0, 0
	reap := func() {
		for _, u := range d.collect(4) {
			received += int(u.len)
			pendingRx--
		}
		pendingTx -= len(d.collect(5))
	}
	for i := 0; i < rounds; i++ {
		// Keep well under the 16-entry queues the test driver sets up.
		waitFor(t, "free descriptors", func() bool { reap(); return pendingRx < window && pendingTx < window })
		d.add(4, buf{rx, size, true})
		d.kick(4)
		d.add(5, buf{tx, 1, false})
		d.kick(5)
		pendingRx++
		pendingTx++
		d.tr.Read(regIntStatus, 4)
	}
	waitFor(t, "all buffers", func() bool { reap(); return pendingRx == 0 && pendingTx == 0 })
	if received != rounds*size || len(out.String()) != rounds {
		t.Errorf("received %d bytes, wrote %d", received, len(out.String()))
	}
	if !con.Drain(context.Background()) {
		t.Error("Drain failed")
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
