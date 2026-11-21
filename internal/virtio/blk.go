package virtio

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// Block device feature bits, request types and statuses (virtio spec 5.2).
const (
	blkFeatSegMax      = 1 << 2
	blkFeatRO          = 1 << 5
	blkFeatFlush       = 1 << 9
	blkFeatDiscard     = 1 << 13
	blkFeatWriteZeroes = 1 << 14

	blkIn          = 0
	blkOut         = 1
	blkFlush       = 4
	blkGetID       = 8
	blkDiscard     = 11
	blkWriteZeroes = 13

	blkOK          = 0
	blkErr         = 1
	blkUnsupported = 2

	blkIDLen = 20 // VIRTIO_BLK_ID_BYTES

	blkSegLen    = 16 // struct virtio_blk_discard_write_zeroes
	blkSegUnmap  = 1
	blkMaxSegs   = 64
	blkMaxZeroes = 1<<22 - 1 // sectors per discard or write-zeroes segment, just under 2 GiB

	// DiscardAlignment is the granularity, in bytes, the guest is asked to
	// discard in. NTFS gives a sparse file's space back only in whole 64 KiB
	// units, ReFS in clusters, which divide it.
	DiscardAlignment = 64 << 10

	defaultBlkWorkers = 16
)

// BlockFile backs a block device. ReadAt and WriteAt are called from several
// goroutines at once.
type BlockFile interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// Zeroer is a BlockFile that can make a range read as zeros and give its
// space back to the host.
type Zeroer interface {
	Zero(off, n int64) error
}

// Blk is a virtio block device. The vCPU that kicks it serves one request
// itself, without holding the transport's lock, and goroutines of the
// device's own serve any others it made available at once, so requests from
// several vCPUs, or several at once from one, are served in parallel and
// complete in whatever order they finish.
type Blk struct {
	File     BlockFile
	Sectors  uint64 // capacity in 512-byte sectors
	ReadOnly bool
	// Flush offers the guest cache flushes, each of which costs a Sync. A
	// disk without it is write-through to the guest, so fsync never waits
	// for the host's disk: right for a disk thrown away after the run.
	Flush bool
	// Discard offers DISCARD and WRITE_ZEROES, which free the host's space.
	// File must be a Zeroer, and the disk writable.
	Discard bool
	Serial  string // returned by VIRTIO_BLK_T_GET_ID; at most 20 bytes
	// Workers caps the requests served at once. Zero means 16.
	Workers int

	Requests, BytesRead, BytesWritten, Flushes, Discards atomic.Uint64
	// Inline counts requests served by the vCPU that asked.
	Inline atomic.Uint64
	// MaxInflight is the most requests the device had at once.
	MaxInflight atomic.Int64

	once     sync.Once
	work     chan blkReq
	stop     chan struct{}
	stopOnce sync.Once
	inflight sync.WaitGroup // requests not yet returned to the driver
	active   atomic.Int64

	gen int // guarded by the transport's lock; bumped by Reset
}

// blkReq is one request. Its header and segments are copies, taken once,
// so the guest cannot change them after they were checked; its data buffers
// and status byte are guest memory.
type blkReq struct {
	t      *Transport
	gen    int
	head   uint16
	kind   uint32
	offset int64
	data   []Buffer
	segs   []blkSeg
	status []byte
}

type blkSeg struct {
	sector uint64
	num    uint32
	flags  uint32
}

func (d *Blk) DeviceID() uint32 { return 2 }
func (d *Blk) NumQueues() int   { return 1 }
func (d *Blk) Reset()           { d.gen++ }

func (d *Blk) discards() bool {
	_, ok := d.File.(Zeroer)
	return d.Discard && ok && !d.ReadOnly
}

func (d *Blk) Features() uint64 {
	f := uint64(FeatVersion1 | blkFeatSegMax)
	if d.ReadOnly {
		f |= blkFeatRO
	}
	if d.Flush {
		f |= blkFeatFlush
	}
	if d.discards() {
		f |= blkFeatDiscard | blkFeatWriteZeroes
	}
	return f
}

func (d *Blk) Config() []byte {
	cfg := make([]byte, 64)
	le.PutUint64(cfg[0:], d.Sectors)
	le.PutUint32(cfg[12:], MaxQueueSize-2) // seg_max
	if d.discards() {
		le.PutUint32(cfg[36:], blkMaxZeroes)         // max_discard_sectors
		le.PutUint32(cfg[40:], blkMaxSegs)           // max_discard_seg
		le.PutUint32(cfg[44:], DiscardAlignment/512) // discard_sector_alignment
		le.PutUint32(cfg[48:], blkMaxZeroes)         // max_write_zeroes_sectors
		le.PutUint32(cfg[52:], blkMaxSegs)           // max_write_zeroes_seg
		cfg[56] = 1                                  // write_zeroes_may_unmap
	}
	return cfg
}

func (d *Blk) init() {
	d.once.Do(func() {
		n := d.Workers
		if n <= 0 {
			n = defaultBlkWorkers
		}
		// The queue holds at most MaxQueueSize chains; the spare room covers
		// requests from before a reset still waiting to be served. So the
		// channel never fills and Notify never blocks the vCPU.
		d.work = make(chan blkReq, 4*MaxQueueSize)
		d.stop = make(chan struct{})
		for range n {
			go d.worker()
		}
	})
}

// Notify hands every request the driver made available to the workers. The
// transport calls NotifyDeferred instead; Notify is there for the Device
// interface.
func (d *Blk) Notify(t *Transport, qi int) {
	for _, r := range d.take(t, qi) {
		d.dispatch(r)
	}
}

// NotifyDeferred takes every request the driver made available. The vCPU
// that kicked serves the first itself, once the transport's lock is
// released, and the workers serve the rest. Handing every request to a
// worker cost more than most requests take: the vCPU halts while it waits,
// and has to be woken by the completion, which made a single reader of
// cached files 40% slower. Without the lock, other vCPUs submit meanwhile,
// and serve their own requests in parallel.
func (d *Blk) NotifyDeferred(t *Transport, qi int) func() {
	pending := d.take(t, qi)
	if len(pending) == 0 {
		return nil
	}
	first := pending[0]
	d.track()
	for _, r := range pending[1:] {
		d.dispatch(r)
	}
	return func() {
		d.Inline.Add(1)
		d.run(first)
	}
}

// take pops every chain the driver made available, answers those that need
// no I/O, and returns the rest. The transport's lock is held.
func (d *Blk) take(t *Transport, qi int) []blkReq {
	d.init()
	q := t.Queue(qi)
	if q == nil {
		return nil
	}
	done := false
	var pending []blkReq
	for {
		c, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		r, status, ready := d.parse(c, err)
		if ready {
			// Answered here: a malformed chain, a bad request, or GET_ID.
			written := uint32(0)
			if r.status != nil {
				r.status[0] = status
				written = 1
				if r.kind == blkGetID && status == blkOK {
					written += blkIDLen
				}
			}
			q.Push(t.Memory(), c.Head, written)
			d.Requests.Add(1)
			done = true
			continue
		}
		r.t, r.gen = t, d.gen
		pending = append(pending, r)
	}
	if done {
		t.Interrupt(qi)
	}
	return pending
}

// track counts a request as in flight until complete returns it.
func (d *Blk) track() {
	d.inflight.Add(1)
	n := d.active.Add(1)
	for m := d.MaxInflight.Load(); n > m && !d.MaxInflight.CompareAndSwap(m, n); m = d.MaxInflight.Load() {
	}
}

func (d *Blk) dispatch(r blkReq) {
	d.track()
	d.work <- r
}

// run serves a tracked request and returns it to the driver.
func (d *Blk) run(r blkReq) {
	status, written := d.serve(&r)
	r.status[0] = status
	d.Requests.Add(1)
	d.complete(r, written+1)
}

// parse reads a chain's header and checks the request. ready means it has
// been answered with status, and needs no worker.
func (d *Blk) parse(c Chain, chainErr error) (r blkReq, status byte, ready bool) {
	r.head = c.Head
	if chainErr != nil || len(c.Buffers) < 2 || len(c.Buffers[0].Data) < 16 || c.Buffers[0].Writable {
		return r, 0, true
	}
	st := c.Buffers[len(c.Buffers)-1]
	if !st.Writable || len(st.Data) < 1 {
		return r, 0, true
	}
	r.status = st.Data[:1]
	hdr := c.Buffers[0].Data
	r.kind = le.Uint32(hdr[0:])
	sector := le.Uint64(hdr[8:])
	r.data = c.Buffers[1 : len(c.Buffers)-1]

	var total uint64
	for _, b := range r.data {
		total += uint64(len(b.Data))
	}
	switch r.kind {
	case blkIn, blkOut:
		want := r.kind == blkIn // in: the device writes into the buffers
		for _, b := range r.data {
			if b.Writable != want {
				return r, blkErr, true
			}
		}
		if r.kind == blkOut && d.ReadOnly {
			return r, blkErr, true
		}
		if total%512 != 0 || sector > d.Sectors || total/512 > d.Sectors-sector {
			return r, blkErr, true
		}
		r.offset = int64(sector * 512)
		return r, 0, false
	case blkFlush:
		return r, 0, false
	case blkGetID:
		if len(r.data) != 1 || !r.data[0].Writable || len(r.data[0].Data) < blkIDLen {
			return r, blkErr, true
		}
		id := make([]byte, blkIDLen)
		copy(id, d.Serial)
		copy(r.data[0].Data, id)
		return r, blkOK, true
	case blkDiscard, blkWriteZeroes:
		if !d.discards() {
			return r, blkUnsupported, true
		}
		if total == 0 || total%blkSegLen != 0 || total/blkSegLen > blkMaxSegs {
			return r, blkErr, true
		}
		raw := make([]byte, 0, total)
		for _, b := range r.data {
			if b.Writable {
				return r, blkErr, true
			}
			raw = append(raw, b.Data...)
		}
		r.data = nil
		for i := 0; i < len(raw); i += blkSegLen {
			s := blkSeg{le.Uint64(raw[i:]), le.Uint32(raw[i+8:]), le.Uint32(raw[i+12:])}
			if s.flags&^blkSegUnmap != 0 || (r.kind == blkDiscard && s.flags != 0) {
				return r, blkUnsupported, true
			}
			if s.num > blkMaxZeroes || s.sector > d.Sectors || uint64(s.num) > d.Sectors-s.sector {
				return r, blkErr, true
			}
			r.segs = append(r.segs, s)
		}
		return r, 0, false
	}
	return r, blkUnsupported, true
}

func (d *Blk) worker() {
	for {
		select {
		case r := <-d.work:
			d.run(r)
		case <-d.stop:
			return
		}
	}
}

// zeros is written where the guest asks for zeros it means to keep.
var zeros = make([]byte, 1<<20)

// serve performs one request and returns its status and the bytes written
// into the guest's buffers.
func (d *Blk) serve(r *blkReq) (byte, uint32) {
	switch r.kind {
	case blkIn:
		var written uint32
		off := r.offset
		for _, b := range r.data {
			n, err := d.File.ReadAt(b.Data, off)
			written += uint32(n)
			d.BytesRead.Add(uint64(n))
			if err != nil || n < len(b.Data) {
				return blkErr, written
			}
			off += int64(len(b.Data))
		}
		return blkOK, written
	case blkOut:
		off := r.offset
		for _, b := range r.data {
			n, err := d.File.WriteAt(b.Data, off)
			d.BytesWritten.Add(uint64(n))
			if err != nil {
				return blkErr, 0
			}
			off += int64(len(b.Data))
		}
		return blkOK, 0
	case blkFlush:
		d.Flushes.Add(1)
		if d.Flush {
			if err := d.File.Sync(); err != nil {
				return blkErr, 0
			}
		}
		return blkOK, 0
	case blkDiscard, blkWriteZeroes:
		z := d.File.(Zeroer)
		for _, s := range r.segs {
			d.Discards.Add(1)
			off, n := int64(s.sector*512), int64(s.num)*512
			var err error
			if r.kind == blkWriteZeroes && s.flags&blkSegUnmap == 0 {
				// The guest wants the space kept: write real zeros.
				for done := int64(0); done < n && err == nil; done += int64(len(zeros)) {
					_, err = d.File.WriteAt(zeros[:min(int64(len(zeros)), n-done)], off+done)
				}
			} else {
				err = z.Zero(off, n)
			}
			if err != nil {
				return blkErr, 0
			}
		}
		return blkOK, 0
	}
	return blkUnsupported, 0
}

func (d *Blk) complete(r blkReq, written uint32) {
	r.t.Do(func() {
		if r.gen != d.gen {
			return
		}
		if q := r.t.Queue(0); q != nil {
			q.Push(r.t.Memory(), r.head, written)
			r.t.Interrupt(0)
		}
	})
	d.active.Add(-1)
	d.inflight.Done()
}

// Drain is called once the guest has stopped. It waits until every request
// has been served, or until ctx is done, and reports whether the device is
// idle, that is, no longer touching guest memory or its file.
func (d *Blk) Drain(ctx context.Context) bool {
	d.init()
	done := make(chan struct{})
	go func() {
		d.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		default:
			return false
		}
	}
	d.stopOnce.Do(func() { close(d.stop) })
	return true
}
