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

// FsAsyncHandler is implemented by FsHandlers that pass requests on, to be
// served elsewhere, rather than serve them on the goroutine that calls
// them. ServeAsync takes one request, as Serve does, and returns at once;
// done is called, once, with the length of the reply written into reply.
// The device calls ServeAsync on the vCPU that sent the request, and holds
// no worker while it waits, so requests that wait long, such as a lock
// request, hold up no other.
//
// If the handler also has an Abort method, Drain calls it once the guest
// has stopped: it must answer every request still waiting.
type FsAsyncHandler interface {
	ServeAsync(req []byte, reply [][]byte, done func(written uint32))
}

// InlineHandler is implemented by handlers that know which requests may take
// long. Inline reports whether req may be served on the vCPU that sent it;
// those it refuses go to a worker.
type InlineHandler interface {
	Inline(req []byte) bool
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
	// and BytesOut the bytes of their replies, and Inline those served on the
	// vCPU that sent them.
	Requests, BytesIn, BytesOut, Inline atomic.Uint64

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

// Notify hands every chain the driver made available to the workers. The
// transport calls NotifyDeferred instead; Notify is there for the Device
// interface.
func (f *Fs) Notify(t *Transport, qi int) {
	for _, ch := range f.take(t, qi) {
		f.dispatch(ch)
	}
}

// NotifyDeferred takes every chain the driver made available. The vCPU that
// kicked serves the first itself, once the transport's lock is released,
// and the workers serve the rest, as Blk does: handing a request to a worker
// leaves the vCPU halted until the worker's interrupt wakes it, which costs
// more than most requests take. A request the handler says may take long,
// such as one that would have Windows fetch a file from the network, goes to
// a worker all the same, so as not to hold up the vCPU.
func (f *Fs) NotifyDeferred(t *Transport, qi int) func() {
	chains := f.take(t, qi)
	if _, ok := f.Handler.(FsAsyncHandler); ok {
		// Starting a request only passes it on, so the vCPU starts them
		// all.
		if len(chains) == 0 {
			return nil
		}
		f.inflight.Add(len(chains))
		return func() {
			for _, ch := range chains {
				f.Inline.Add(1)
				f.serve(ch)
			}
		}
	}
	first := -1
	for i, ch := range chains {
		if first < 0 && f.inline(ch.req) {
			first = i
			continue
		}
		f.dispatch(ch)
	}
	if first < 0 {
		return nil
	}
	ch := chains[first]
	f.inflight.Add(1)
	return func() {
		f.Inline.Add(1)
		f.serve(ch)
	}
}

// inline reports whether a request may be served on the vCPU.
func (f *Fs) inline(req []byte) bool {
	h, ok := f.Handler.(InlineHandler)
	return !ok || h.Inline(req)
}

// take pops every chain the driver made available and returns those that
// need serving, answering malformed ones at once. The transport's lock is
// held.
func (f *Fs) take(t *Transport, qi int) []fsChain {
	f.init()
	q := t.Queue(qi)
	if q == nil {
		return nil
	}
	bad := false
	var chains []fsChain
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
		chains = append(chains, ch)
	}
	if bad {
		t.Interrupt(qi)
	}
	return chains
}

func (f *Fs) dispatch(ch fsChain) {
	f.inflight.Add(1)
	f.work <- ch
}

func (f *Fs) worker() {
	for {
		select {
		case ch := <-f.work:
			f.serve(ch)
		case <-f.stop:
			return
		}
	}
}

// serve handles a request counted in flight and returns it to the driver.
func (f *Fs) serve(ch fsChain) {
	if h, ok := f.Handler.(FsAsyncHandler); ok {
		h.ServeAsync(ch.req, ch.reply, func(written uint32) {
			f.Requests.Add(1)
			f.BytesIn.Add(uint64(len(ch.req)))
			f.BytesOut.Add(uint64(written))
			f.complete(ch, written)
		})
		return
	}
	var written uint32
	if f.Handler != nil {
		written = f.Handler.Serve(ch.req, ch.reply)
	}
	f.Requests.Add(1)
	f.BytesIn.Add(uint64(len(ch.req)))
	f.BytesOut.Add(uint64(written))
	f.complete(ch, written)
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
	if a, ok := f.Handler.(interface{ Abort() }); ok {
		a.Abort()
	}
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
