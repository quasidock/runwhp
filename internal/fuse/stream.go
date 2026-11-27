package fuse

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// FUSE over a byte stream.
//
// A guest's FUSE requests can be served by another guest: the guest's VMM
// forwards each request it takes from its virtio-fs queue over a stream,
// and the serving side answers on the same stream. Each message goes in a
// frame:
//
//	u32 length of the message
//	u32 replyMax: in a request, the room the guest gave for its reply;
//	    0 in a reply
//	the FUSE message itself, fuse_in_header or fuse_out_header first
//
// all little-endian. Requests that get no reply, FORGET and BATCH_FORGET,
// get none here either. Replies come in whatever order the requests finish,
// matched to them by the header's unique. A frame longer than MaxFrame ends
// the stream.

// MaxFrame is the longest message a frame may carry: a READ's or WRITE's
// data, at most MaxPages pages, and a page for its headers.
const MaxFrame = MaxPages*pageSize + pageSize

const (
	frameHeaderLen = 8
	streamBuffer   = 256 << 10
	// maxServing caps the requests ServeStream serves at once, SETLKW
	// aside.
	maxServing = 64
)

var errFrameTooLong = errors.New("fuse: frame too long")

// readFrame reads one frame into buf, growing it as needed, and returns
// the message, which aliases buf, and its replyMax.
func readFrame(r *bufio.Reader, buf *[]byte) ([]byte, uint32, error) {
	var h [frameHeaderLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, 0, err
	}
	n := le.Uint32(h[0:])
	replyMax := le.Uint32(h[4:])
	if n > MaxFrame {
		return nil, 0, errFrameTooLong
	}
	if cap(*buf) < int(n) {
		*buf = make([]byte, n)
	}
	msg := (*buf)[:n]
	if _, err := io.ReadFull(r, msg); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
	}
	return msg, replyMax, nil
}

// frameWriter writes frames from many goroutines on one of its own,
// gathering those that come close together into one write.
type frameWriter struct {
	ch   chan outFrame
	stop chan struct{}
	err  atomic.Pointer[error]
}

type outFrame struct {
	msg      []byte
	replyMax uint32
	release  func() // called once msg has been written, or dropped
}

func newFrameWriter(w io.Writer, failed func(error)) *frameWriter {
	fw := &frameWriter{ch: make(chan outFrame, 256), stop: make(chan struct{})}
	go fw.run(w, failed)
	return fw
}

func (fw *frameWriter) run(w io.Writer, failed func(error)) {
	bw := bufio.NewWriterSize(w, streamBuffer)
	var h [frameHeaderLen]byte
	var err error
	for {
		var f outFrame
		select {
		case f = <-fw.ch:
		case <-fw.stop:
			return
		}
		if err == nil {
			le.PutUint32(h[0:], uint32(len(f.msg)))
			le.PutUint32(h[4:], f.replyMax)
			if _, err = bw.Write(h[:]); err == nil {
				_, err = bw.Write(f.msg)
			}
			if err == nil && len(fw.ch) == 0 {
				err = bw.Flush()
			}
			if err != nil {
				fw.err.Store(&err)
				failed(err)
			}
		}
		if f.release != nil {
			f.release()
		}
	}
}

// send queues a frame, and reports false if the writer has failed or
// stopped; release is called either way.
func (fw *frameWriter) send(f outFrame) bool {
	if fw.err.Load() != nil {
		if f.release != nil {
			f.release()
		}
		return false
	}
	select {
	case fw.ch <- f:
		return true
	case <-fw.stop:
		if f.release != nil {
			f.release()
		}
		return false
	}
}

func (fw *frameWriter) close() {
	select {
	case <-fw.stop:
	default:
		close(fw.stop)
	}
}

// ServeStream serves the FUSE requests framed on conn with s until conn
// fails or ends. Requests are served at once, each on a goroutine of its
// own, so a lock request that waits holds up no other. Once conn ends,
// ServeStream calls abort, which must end any request still waiting, such
// as a SETLKW, then waits for the requests being served, and returns nil
// for a clean end of the stream.
func ServeStream(conn io.ReadWriter, s *Server, abort func()) error {
	br := bufio.NewReaderSize(conn, streamBuffer)
	fw := newFrameWriter(conn, func(error) {
		// Replies can no longer be sent: end the stream, which ends the
		// reading too.
		if c, ok := conn.(io.Closer); ok {
			c.Close()
		}
	})
	defer fw.close()
	sem := make(chan struct{}, maxServing)
	var wg sync.WaitGroup
	pool := sync.Pool{New: func() any { b := make([]byte, MaxFrame); return &b }}
	var err error
	for {
		var buf []byte
		var msg []byte
		var replyMax uint32
		msg, replyMax, err = readFrame(br, &buf)
		if err != nil {
			break
		}
		op := Opcode(msg)
		if op != OpSetlkw {
			sem <- struct{}{}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if op != OpSetlkw {
				defer func() { <-sem }()
			}
			bp := pool.Get().(*[]byte)
			reply := (*bp)[:min(int(replyMax), MaxFrame)]
			n := s.Serve(msg, [][]byte{reply})
			if n == 0 || replyless(op) {
				pool.Put(bp)
				return
			}
			fw.send(outFrame{msg: reply[:n], release: func() { pool.Put(bp) }})
		}()
	}
	if abort != nil {
		abort()
	}
	wg.Wait()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// Forwarder passes a guest's FUSE requests over a stream to whatever
// serves them, as ServeStream does, and the replies back. It implements
// virtio.FsAsyncHandler: a request is only queued on the vCPU that sent it,
// and completes when its reply comes back, so requests that wait long, such
// as SETLKW, hold up no worker.
//
// Once the stream fails, every request waiting and every later one is
// answered EIO: the server's nodes and file handles were its alone, and
// cannot be taken up again on another stream.
type Forwarder struct {
	// Logf, when set, reports why the stream ended.
	Logf func(format string, a ...any)

	conn io.ReadWriteCloser
	fw   *frameWriter

	mu      sync.Mutex
	pending map[uint64]forwarded
	failed  error

	// Requests counts the requests forwarded, and Failed those answered
	// EIO because the stream had ended.
	Requests, Failed atomic.Uint64
}

type forwarded struct {
	reply [][]byte
	done  func(uint32)
}

// NewForwarder forwards requests over conn, which it owns from then on.
func NewForwarder(conn io.ReadWriteCloser) *Forwarder {
	f := &Forwarder{conn: conn, pending: make(map[uint64]forwarded)}
	f.fw = newFrameWriter(conn, func(err error) { f.fail(err) })
	go f.read()
	return f
}

// ServeAsync forwards one request; done is called with the length of its
// reply, once it has been written into reply.
func (f *Forwarder) ServeAsync(req []byte, reply [][]byte, done func(uint32)) {
	if len(req) < inHeaderLen {
		done(0)
		return
	}
	f.Requests.Add(1)
	op := Opcode(req)
	unique := le.Uint64(req[8:])
	if replyless(op) {
		f.fw.send(outFrame{msg: req})
		done(0)
		return
	}
	f.mu.Lock()
	if err := f.failed; err != nil {
		f.mu.Unlock()
		f.Failed.Add(1)
		done(errorReply(reply, unique, EIO))
		return
	}
	f.pending[unique] = forwarded{reply, done}
	f.mu.Unlock()
	if !f.fw.send(outFrame{msg: req, replyMax: uint32(min(capacity(reply), MaxFrame))}) {
		f.fail(errors.New("fuse: forwarding stopped"))
	}
}

// Serve forwards one request and waits for its reply, as virtio.FsHandler
// asks; the device calls ServeAsync instead.
func (f *Forwarder) Serve(req []byte, reply [][]byte) uint32 {
	got := make(chan uint32, 1)
	f.ServeAsync(req, reply, func(n uint32) { got <- n })
	return <-got
}

func (f *Forwarder) read() {
	br := bufio.NewReaderSize(f.conn, streamBuffer)
	var buf []byte
	for {
		msg, _, err := readFrame(br, &buf)
		if err != nil {
			f.fail(err)
			return
		}
		if len(msg) < outHeaderLen {
			f.fail(fmt.Errorf("fuse: reply of %d bytes is shorter than its header", len(msg)))
			return
		}
		unique := le.Uint64(msg[8:])
		f.mu.Lock()
		p, ok := f.pending[unique]
		delete(f.pending, unique)
		f.mu.Unlock()
		if !ok {
			f.fail(fmt.Errorf("fuse: reply to request %d, which is not waiting", unique))
			return
		}
		if len(msg) > capacity(p.reply) {
			p.done(errorReply(p.reply, unique, ERANGE))
			continue
		}
		p.done(scatter(p.reply, msg))
	}
}

// fail ends the stream: every request waiting is answered EIO.
func (f *Forwarder) fail(err error) {
	f.mu.Lock()
	if f.failed != nil {
		f.mu.Unlock()
		return
	}
	f.failed = err
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	if f.Logf != nil && !errors.Is(err, errAborted) {
		f.Logf("fuse: forwarding ended, %d requests waiting: %v", len(pending), err)
	}
	f.fw.close()
	f.conn.Close()
	for unique, p := range pending {
		f.Failed.Add(1)
		p.done(errorReply(p.reply, unique, EIO))
	}
}

var errAborted = errors.New("fuse: forwarding aborted")

// Abort ends the stream, answering every request still waiting, as
// virtio.Fs asks once its guest has stopped.
func (f *Forwarder) Abort() { f.fail(errAborted) }

// Close ends the stream.
func (f *Forwarder) Close() error {
	f.Abort()
	return nil
}

// Stats summarizes what was forwarded.
func (f *Forwarder) Stats() string {
	f.mu.Lock()
	waiting := len(f.pending)
	f.mu.Unlock()
	return fmt.Sprintf("fuse requests forwarded: %d, %d waiting, %d failed", f.Requests.Load(), waiting, f.Failed.Load())
}

// errorReply writes a reply carrying only an error into dst, and returns
// its length.
func errorReply(dst [][]byte, unique uint64, e Errno) uint32 {
	if capacity(dst) < outHeaderLen {
		return 0
	}
	var h [outHeaderLen]byte
	le.PutUint32(h[0:], outHeaderLen)
	le.PutUint32(h[4:], uint32(-int32(e)))
	le.PutUint64(h[8:], unique)
	return scatter(dst, h[:])
}
