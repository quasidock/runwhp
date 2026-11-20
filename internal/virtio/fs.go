package virtio

import (
	"context"
	"sync"
	"sync/atomic"
)

// Queues of a virtio-fs device (virtio spec 5.11.2).
const (
	fsHiprioQueue  = 0 // FORGET requests, which get no reply
	fsRequestQueue = 1

	fsTagLen         = 36
	defaultFsWorkers = 16
)

// FsHandler serves the FUSE requests of an Fs device.
type FsHandler interface {
	// Serve handles one request. req is the driver-readable part of the
	// chain, copied out of guest memory, so Serve may keep it. reply is the
	// driver-writable part, in order, still in guest memory: Serve fills it
	// from the start and returns how many bytes it wrote. Serve is called
	// from several goroutines at once.
	Serve(req []byte, reply [][]byte) uint32
}

// Fs is a virtio-fs device with one request queue. It serves requests on
// goroutines of its own, so the vCPU never waits for the host's file system,
// and requests complete in whatever order they finish.
type Fs struct {
	Tag     string // the mount tag; at most 36 bytes
	Handler FsHandler
	// Workers caps the requests served at once. Zero means 16.
	Workers int

	// Requests counts requests, BytesIn the bytes they carried from the guest
	// and BytesOut the bytes of their replies.
	Requests, BytesIn, BytesOut atomic.Uint64

	once     sync.Once
	work     chan fsChain
	stop     chan struct{}
	stopOnce sync.Once
	inflight sync.WaitGroup // chains not yet returned to the driver

	gen int // guarded by the transport's lock; bumped by Reset
}

type fsChain struct {
	t     *Transport
	gen   int
	q     int
	head  uint16
	req   []byte
	reply [][]byte
}

func (f *Fs) DeviceID() uint32 { return 26 }
func (f *Fs) NumQueues() int   { return 2 }
func (f *Fs) Features() uint64 { return FeatVersion1 }

func (f *Fs) Config() []byte {
	cfg := make([]byte, fsTagLen+4) // tag, num_request_queues
	copy(cfg[:fsTagLen], f.Tag)
	le.PutUint32(cfg[fsTagLen:], 1)
	return cfg
}

func (f *Fs) Reset() { f.gen++ }

func (f *Fs) init() {
	f.once.Do(func() {
		n := f.Workers
		if n <= 0 {
			n = defaultFsWorkers
		}
		// Each of the two queues holds at most MaxQueueSize chains; the
		// spare room covers chains from before a reset still waiting to be
		// served. So the channel never fills and Notify never blocks the vCPU.
		f.work = make(chan fsChain, 4*MaxQueueSize)
		f.stop = make(chan struct{})
		for range n {
			go f.worker()
		}
	})
}

// Notify hands every chain the driver made available to the workers.
func (f *Fs) Notify(t *Transport, qi int) {
	f.init()
	q := t.Queue(qi)
	bad := false
	for {
		c, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		if err != nil {
			q.Push(t.Memory(), c.Head, 0)
			bad = true
			continue
		}
		ch := fsChain{t: t, gen: f.gen, q: qi, head: c.Head}
		n := 0
		for _, b := range c.Buffers {
			if !b.Writable {
				n += len(b.Data)
			}
		}
		ch.req = make([]byte, 0, n)
		for _, b := range c.Buffers {
			if b.Writable {
				ch.reply = append(ch.reply, b.Data)
			} else {
				ch.req = append(ch.req, b.Data...)
			}
		}
		f.inflight.Add(1)
		f.work <- ch
	}
	if bad {
		t.Interrupt(qi)
	}
}

func (f *Fs) worker() {
	for {
		select {
		case ch := <-f.work:
			var written uint32
			if f.Handler != nil {
				written = f.Handler.Serve(ch.req, ch.reply)
			}
			f.Requests.Add(1)
			f.BytesIn.Add(uint64(len(ch.req)))
			f.BytesOut.Add(uint64(written))
			f.complete(ch, written)
		case <-f.stop:
			return
		}
	}
}

func (f *Fs) complete(ch fsChain, written uint32) {
	ch.t.Do(func() {
		if ch.gen != f.gen {
			return
		}
		if q := ch.t.Queue(ch.q); q != nil {
			q.Push(ch.t.Memory(), ch.head, written)
			ch.t.Interrupt(ch.q)
		}
	})
	f.inflight.Done()
}

// Drain is called once the guest has stopped. It waits until every request
// has been served, or until ctx is done, and reports whether the device is
// idle, that is, no longer writing to guest memory.
func (f *Fs) Drain(ctx context.Context) bool {
	f.init()
	done := make(chan struct{})
	go func() {
		f.inflight.Wait()
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
	f.stopOnce.Do(func() { close(f.stop) })
	return true
}
