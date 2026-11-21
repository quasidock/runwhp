package virtio

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memFile is an in-memory BlockFile and Zeroer, safe for the device's
// workers. A read of the sector in block, if set, waits until block is
// closed; a read of the sector in fail fails.
type memFile struct {
	mu     sync.Mutex
	data   []byte
	synced int
	zeroed [][2]int64

	block      chan struct{}
	blockAt    int64
	failAt     int64
	writeSeen  chan int64 // offsets written, if set
	readsSeen  int
	writeCalls int
}

func newMemFile(size int) *memFile {
	return &memFile{data: make([]byte, size), blockAt: -1, failAt: -1}
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if f.block != nil && off == f.blockAt {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readsSeen++
	if off == f.failAt {
		return 0, errors.New("injected read error")
	}
	return copy(p, f.data[off:]), nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	if f.block != nil && off == f.blockAt {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCalls++
	if f.writeSeen != nil {
		f.writeSeen <- off
	}
	return copy(f.data[off:], p), nil
}

func (f *memFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced++
	return nil
}

func (f *memFile) Zero(off, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.data[off : off+n])
	f.zeroed = append(f.zeroed, [2]int64{off, n})
	return nil
}

func (f *memFile) get(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// blkReq builds a request on queue 0 without kicking: header, data buffers,
// status byte. It returns the head and the status byte's address.
func addBlk(d *driver, kind uint32, sector uint64, data ...buf) (head uint16, hdr, st uint64) {
	hdr = d.alloc(16)
	le.PutUint32(d.mem[hdr:], kind)
	le.PutUint64(d.mem[hdr+8:], sector)
	st = d.alloc(1)
	d.mem[st] = 0xff
	bufs := append([]buf{{hdr, 16, false}}, data...)
	bufs = append(bufs, buf{st, 1, true})
	return d.add(0, bufs...), hdr, st
}

func blkRequest(d *driver, kind uint32, sector uint64, data ...buf) (status byte, used usedElem) {
	head, _, st := addBlk(d, kind, sector, data...)
	d.kick(0)
	u := d.collectN(0, 1)
	if len(u) != 1 || u[0].head != head {
		d.t.Fatalf("used ring = %+v, want one entry for head %d", u, head)
	}
	return d.mem[st], u[0]
}

func segments(d *driver, segs ...blkSeg) buf {
	at := d.alloc(uint64(len(segs)) * blkSegLen)
	for i, s := range segs {
		p := d.mem[at+uint64(i)*blkSegLen:]
		le.PutUint64(p[0:], s.sector)
		le.PutUint32(p[8:], s.num)
		le.PutUint32(p[12:], s.flags)
	}
	return buf{at, uint32(len(segs)) * blkSegLen, false}
}

func TestBlk(t *testing.T) {
	file := newMemFile(8 * 512)
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
	if n := d.interrupts(); n != 1 {
		t.Errorf("irqs = %d after one request", n)
	}

	status, _ = blkRequest(d, blkOut, 0, buf{data, 512, false})
	if status != blkErr || file.data[0] != 0 {
		t.Error("write to a read-only disk succeeded")
	}

	id := d.alloc(blkIDLen)
	copy(d.mem[id:], bytes.Repeat([]byte{'x'}, blkIDLen))
	status, u = blkRequest(d, blkGetID, 0, buf{id, blkIDLen, true})
	if status != blkOK || u.len != blkIDLen+1 || string(d.mem[id:id+blkIDLen]) != "layer3\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" {
		t.Errorf("GET_ID = %q, len %d", d.mem[id:id+blkIDLen], u.len)
	}

	status, u = blkRequest(d, 99, 0)
	if status != blkUnsupported || u.len != 1 {
		t.Errorf("unknown request: status %d, len %d", status, u.len)
	}
	if got := dev.Requests.Load(); got != 4 {
		t.Errorf("Requests = %d", got)
	}
}

func TestBlkWriteAndFlush(t *testing.T) {
	for _, flush := range []bool{false, true} {
		file := newMemFile(4 * 512)
		dev := &Blk{File: file, Sectors: 4, Flush: flush}
		d := newDriver(t, dev)
		if got := dev.Features()&blkFeatFlush != 0; got != flush {
			t.Errorf("Flush %v: FLUSH offered = %v", flush, got)
		}
		data := d.alloc(512)
		copy(d.mem[data:], bytes.Repeat([]byte{7}, 512))
		if status, _ := blkRequest(d, blkOut, 3, buf{data, 512, false}); status != blkOK || file.data[3*512] != 7 {
			t.Error("write failed")
		}
		status, _ := blkRequest(d, blkFlush, 0)
		var synced int
		file.get(func() { synced = file.synced })
		want := 0
		if flush {
			want = 1
		}
		if status != blkOK || synced != want {
			t.Errorf("Flush %v: flush status %d, %d syncs, want %d", flush, status, synced, want)
		}
	}
}

func TestBlkBounds(t *testing.T) {
	file := newMemFile(8 * 512)
	d := newDriver(t, &Blk{File: file, Sectors: 8})
	data := d.alloc(1024)
	for _, c := range []struct {
		name   string
		kind   uint32
		sector uint64
		b      buf
	}{
		{"read past the end", blkIn, 8, buf{data, 512, true}},
		{"read straddling the end", blkIn, 7, buf{data, 1024, true}},
		{"write straddling the end", blkOut, 7, buf{data, 1024, false}},
		{"write at a huge sector", blkOut, 1 << 62, buf{data, 512, false}},
		{"read of part of a sector", blkIn, 0, buf{data, 100, true}},
		{"read into a read-only buffer", blkIn, 0, buf{data, 512, false}},
	} {
		if status, _ := blkRequest(d, c.kind, c.sector, c.b); status != blkErr {
			t.Errorf("%s: status %d, want IOERR", c.name, status)
		}
	}
	var reads, writes int
	file.get(func() { reads, writes = file.readsSeen, file.writeCalls })
	if reads != 0 || writes != 0 {
		t.Errorf("bad requests reached the file: %d reads, %d writes", reads, writes)
	}
	if status, _ := blkRequest(d, blkIn, 6, buf{data, 1024, true}); status != blkOK {
		t.Errorf("read of the last two sectors: status %d", status)
	}
}

func TestBlkReadError(t *testing.T) {
	file := newMemFile(8 * 512)
	file.failAt = 1024
	d := newDriver(t, &Blk{File: file, Sectors: 8})
	a, b := d.alloc(512), d.alloc(512)
	status, u := blkRequest(d, blkIn, 1, buf{a, 512, true}, buf{b, 512, true})
	if status != blkErr || u.len != 512+1 {
		t.Errorf("read failing in its second buffer: status %d, len %d; want IOERR, 513", status, u.len)
	}
}

func TestBadChainIsReturned(t *testing.T) {
	d := newDriver(t, &Blk{File: newMemFile(512), Sectors: 1})
	head := d.add(0, buf{uint64(len(d.mem)) - 4, 16, false}, buf{0, 1, true})
	d.kick(0)
	if u := d.collect(0); len(u) != 1 || u[0].head != head || u[0].len != 0 {
		t.Errorf("used = %+v", u)
	}
}

func TestResetClearsQueues(t *testing.T) {
	d := newDriver(t, &Blk{File: newMemFile(512), Sectors: 1})
	d.tr.Write(regStatus, 4, 0)
	if d.tr.Queue(0) != nil || d.tr.Read(regStatus, 4) != 0 {
		t.Error("reset left the queue ready")
	}
}

func TestBlkOutOfOrder(t *testing.T) {
	file := newMemFile(8 * 512)
	file.block, file.blockAt = make(chan struct{}), 0
	d := newDriver(t, &Blk{File: file, Sectors: 8})
	a, b := d.alloc(512), d.alloc(512)
	slow, _, _ := addBlk(d, blkIn, 0, buf{a, 512, true})
	fast, _, _ := addBlk(d, blkIn, 4, buf{b, 512, true})
	d.kick(0) // returns at once: the vCPU never waits for the slow read
	if u := d.collectN(0, 1); u[0].head != fast {
		t.Fatalf("first completion = %+v, want the fast read %d", u, fast)
	}
	close(file.block)
	if u := d.collectN(0, 1); u[0].head != slow {
		t.Fatalf("second completion = %+v, want the slow read %d", u, slow)
	}
}

func TestBlkResetWithRequestsInFlight(t *testing.T) {
	file := newMemFile(8 * 512)
	file.block, file.blockAt = make(chan struct{}), 0
	dev := &Blk{File: file, Sectors: 8}
	d := newDriver(t, dev)
	addBlk(d, blkIn, 0, buf{d.alloc(512), 512, true})
	d.kick(0)
	d.tr.Write(regStatus, 4, 0) // reset while the read waits
	d.setupQueue(0, 16)
	close(file.block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !dev.Drain(ctx) {
		t.Fatal("Drain gave up")
	}
	if u := d.collect(0); len(u) != 0 {
		t.Errorf("a request from before the reset completed on the new queue: %+v", u)
	}
}

func TestBlkDrain(t *testing.T) {
	file := newMemFile(8 * 512)
	file.block, file.blockAt = make(chan struct{}), 0
	dev := &Blk{File: file, Sectors: 8}
	d := newDriver(t, dev)
	addBlk(d, blkIn, 0, buf{d.alloc(512), 512, true})
	d.kick(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if dev.Drain(ctx) {
		t.Error("Drain reported idle with a read in progress")
	}
	cancel()
	close(file.block)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !dev.Drain(ctx) {
		t.Error("Drain gave up after the read finished")
	}
	if got := dev.MaxInflight.Load(); got != 1 {
		t.Errorf("MaxInflight = %d", got)
	}
}

// TestBlkHeaderCopied changes a request's header after the device checked
// it, as a hostile guest could, and expects the device to go on using what
// it checked.
func TestBlkHeaderCopied(t *testing.T) {
	file := newMemFile(8 * 512)
	file.block, file.blockAt = make(chan struct{}), 2*512
	file.writeSeen = make(chan int64, 4)
	d := newDriver(t, &Blk{File: file, Sectors: 8})
	data := d.alloc(512)
	_, hdr, st := addBlk(d, blkOut, 2, buf{data, 512, false})
	d.kick(0)
	le.PutUint64(d.mem[hdr+8:], 1<<40) // far outside the disk
	close(file.block)
	d.collectN(0, 1)
	if off := <-file.writeSeen; off != 2*512 || d.mem[st] != blkOK {
		t.Errorf("write went to offset %d with status %d, want 1024, OK", off, d.mem[st])
	}
}

func TestBlkDiscard(t *testing.T) {
	file := newMemFile(64 * 512)
	for i := range file.data {
		file.data[i] = 0xaa
	}
	dev := &Blk{File: file, Sectors: 64, Discard: true}
	d := newDriver(t, dev)

	status, _ := blkRequest(d, blkDiscard, 0, segments(d, blkSeg{0, 8, 0}, blkSeg{16, 8, 0}))
	if status != blkOK {
		t.Fatalf("discard: status %d", status)
	}
	var zeroed [][2]int64
	file.get(func() { zeroed = append(zeroed, file.zeroed...) })
	if len(zeroed) != 2 || zeroed[0] != [2]int64{0, 4096} || zeroed[1] != [2]int64{8192, 4096} {
		t.Errorf("discard zeroed %v", zeroed)
	}
	if status, _ := blkRequest(d, blkDiscard, 0, segments(d, blkSeg{0, 8, blkSegUnmap})); status != blkUnsupported {
		t.Errorf("discard with the unmap flag: status %d, want UNSUPP", status)
	}
	if status, _ := blkRequest(d, blkWriteZeroes, 0, segments(d, blkSeg{0, 8, 2})); status != blkUnsupported {
		t.Errorf("write zeroes with an unknown flag: status %d, want UNSUPP", status)
	}
	if status, _ := blkRequest(d, blkDiscard, 0, segments(d, blkSeg{60, 8, 0})); status != blkErr {
		t.Errorf("discard straddling the end: status %d, want IOERR", status)
	}

	// Write zeroes with unmap may give the space back; without, it must
	// write zeros.
	if status, _ := blkRequest(d, blkWriteZeroes, 0, segments(d, blkSeg{32, 8, blkSegUnmap})); status != blkOK {
		t.Errorf("write zeroes with unmap: status %d", status)
	}
	var writes int
	file.get(func() { zeroed, writes = append(zeroed[:0], file.zeroed...), file.writeCalls })
	if len(zeroed) != 3 || zeroed[2] != [2]int64{32 * 512, 4096} || writes != 0 {
		t.Errorf("write zeroes with unmap: zeroed %v, %d writes", zeroed, writes)
	}
	if status, _ := blkRequest(d, blkWriteZeroes, 0, segments(d, blkSeg{48, 8, 0})); status != blkOK {
		t.Errorf("write zeroes without unmap: status %d", status)
	}
	file.get(func() { zeroed, writes = append(zeroed[:0], file.zeroed...), file.writeCalls })
	if len(zeroed) != 3 || writes != 1 || !bytes.Equal(file.data[48*512:56*512], make([]byte, 4096)) || file.data[56*512] != 0xaa {
		t.Errorf("write zeroes without unmap: zeroed %v, %d writes", zeroed, writes)
	}
	if got := dev.Discards.Load(); got != 4 {
		t.Errorf("Discards = %d, want 4 segments", got)
	}
}

func TestBlkFeatures(t *testing.T) {
	file := newMemFile(512)
	for i, c := range []struct {
		dev  *Blk
		want uint64
	}{
		{&Blk{ReadOnly: true}, blkFeatRO},
		{&Blk{ReadOnly: true, Discard: true}, blkFeatRO},
		{&Blk{Flush: true}, blkFeatFlush},
		{&Blk{Discard: true}, blkFeatDiscard | blkFeatWriteZeroes},
	} {
		c.dev.File = file
		if got := c.dev.Features() &^ (FeatVersion1 | blkFeatSegMax); got != c.want {
			t.Errorf("case %d: features %#x, want %#x", i, got, c.want)
		}
	}
	// A file that cannot zero gets no discard, whatever is asked.
	plain := struct{ BlockFile }{file}
	if (&Blk{File: plain, Discard: true}).Features()&blkFeatDiscard != 0 {
		t.Error("discard offered by a file without Zero")
	}

	cfg := (&Blk{File: file, Sectors: 1 << 20, Discard: true}).Config()
	for _, c := range []struct {
		name string
		off  int
		want uint32
	}{
		{"max_discard_sectors", 36, blkMaxZeroes},
		{"max_discard_seg", 40, blkMaxSegs},
		{"discard_sector_alignment", 44, 128},
		{"max_write_zeroes_sectors", 48, blkMaxZeroes},
		{"max_write_zeroes_seg", 52, blkMaxSegs},
	} {
		if got := le.Uint32(cfg[c.off:]); got != c.want {
			t.Errorf("%s = %d, want %d", c.name, got, c.want)
		}
	}
	if cfg[56] != 1 {
		t.Error("write_zeroes_may_unmap not set")
	}
}
