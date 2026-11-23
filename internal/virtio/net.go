package virtio

import (
	"context"
	"sync/atomic"
)

// Network device feature bits (virtio spec 5.1.3).
const (
	netFeatCsum      = 1 << 0
	netFeatGuestCsum = 1 << 1
	netFeatMTU       = 1 << 3
	netFeatMAC       = 1 << 5
	netFeatMrgRxbuf  = 1 << 15
)

const (
	netRxQueue = 0
	netTxQueue = 1

	// NetHeaderLen is the size of struct virtio_net_hdr, which with
	// VIRTIO_F_VERSION_1 always includes num_buffers.
	NetHeaderLen = 12

	// netHdrDataValid is VIRTIO_NET_HDR_F_DATA_VALID: the frame's
	// checksums need no checking.
	netHdrDataValid = 2

	defaultNetBacklog = 4 << 20
)

// Net is a virtio network device with one queue pair and no control queue.
// It moves Ethernet frames between the guest and the host. It offers
// checksum offload both ways, since every frame ends in the host's stack:
// the guest may send frames with partial checksums, and frames for the
// guest come marked valid, whatever their checksums say, when the guest
// takes that. It offers no segmentation offload; the link's MTU is large
// enough as it is.
//
// Transmitted frames are copied out on the vCPU thread and handed to Recv.
// Frames for the guest go through Deliver from any goroutine; while the
// guest has no receive buffers posted they wait in a bounded backlog.
type Net struct {
	MAC [6]byte
	MTU uint16
	// Recv receives each frame the guest transmits, without its virtio
	// header. The slice is Recv's to keep. Recv is called with the transport
	// locked, on the vCPU thread, so it must not block and must not call
	// Deliver.
	Recv func(frame []byte)
	// Backlog caps the bytes held for the guest while it has no room to
	// receive them; frames beyond it are dropped. Zero means 4 MiB.
	Backlog int

	// TxFrames and TxBytes count frames from the guest, RxFrames and RxBytes
	// frames delivered to it, and RxDropped frames for it that were thrown
	// away.
	TxFrames, TxBytes, RxFrames, RxBytes, RxDropped atomic.Uint64

	t atomic.Pointer[Transport]

	// guestChecksums is set while the driver takes frames whose checksums
	// were not filled in, marked valid.
	guestChecksums atomic.Bool

	// Guarded by the transport's lock.
	pending      [][]byte
	pendingBytes int
	stopped      bool
}

func (n *Net) DeviceID() uint32 { return 1 }
func (n *Net) NumQueues() int   { return 2 }

func (n *Net) Features() uint64 {
	return FeatVersion1 | netFeatCsum | netFeatGuestCsum | netFeatMAC | netFeatMTU | netFeatMrgRxbuf
}

// GuestChecksums reports whether the guest's driver takes frames whose
// checksums were left out, which the device marks valid: the host's stack
// need not compute them. It turns true when the driver first notifies the
// device after taking the feature, which it does before the first frame
// is due to it, and false when the driver resets the device, which drops
// the frames waiting for it.
func (n *Net) GuestChecksums() bool { return n.guestChecksums.Load() }

func (n *Net) Config() []byte {
	cfg := make([]byte, 12) // mac, status, max_virtqueue_pairs, mtu
	copy(cfg, n.MAC[:])
	le.PutUint16(cfg[8:], 1)
	le.PutUint16(cfg[10:], n.MTU)
	return cfg
}

// Attach implements Attacher.
func (n *Net) Attach(t *Transport) { n.t.Store(t) }

func (n *Net) Reset() {
	n.guestChecksums.Store(false)
	n.dropPending()
}

func (n *Net) Notify(t *Transport, q int) {
	n.guestChecksums.Store(t.DriverFeatures()&netFeatGuestCsum != 0)
	switch q {
	case netTxQueue:
		n.transmit(t)
	case netRxQueue:
		n.flush(t)
	}
}

// Deliver sends one Ethernet frame to the guest, taking ownership of it. It
// never blocks: a frame the guest has no room for waits in the backlog, or is
// dropped when the backlog is full.
func (n *Net) Deliver(frame []byte) {
	t := n.t.Load()
	if t == nil || !t.Do(func() { n.deliver(t, frame) }) {
		n.RxDropped.Add(1)
	}
}

// Drain is called once the guest has stopped. Transmit never holds on to
// guest memory, so all it does is drop what is waiting and refuse more.
func (n *Net) Drain(ctx context.Context) bool {
	if t := n.t.Load(); t != nil {
		t.Do(func() {
			n.stopped = true
			n.dropPending()
		})
	}
	return true
}

func (n *Net) deliver(t *Transport, frame []byte) {
	limit := n.Backlog
	if limit == 0 {
		limit = defaultNetBacklog
	}
	if n.stopped || n.pendingBytes+len(frame) > limit {
		n.RxDropped.Add(1)
		return
	}
	n.pending = append(n.pending, frame)
	n.pendingBytes += len(frame)
	n.flush(t)
}

func (n *Net) dropPending() {
	n.RxDropped.Add(uint64(len(n.pending)))
	clear(n.pending)
	n.pending = n.pending[:0]
	n.pendingBytes = 0
}

// transmit hands every frame the guest made available to Recv.
func (n *Net) transmit(t *Transport) {
	q := t.Queue(netTxQueue)
	mem := t.Memory()
	served := false
	for {
		ch, ok, err := q.Pop(mem)
		if !ok {
			break
		}
		if err == nil {
			if frame := gather(ch.Buffers, NetHeaderLen); frame != nil {
				n.TxFrames.Add(1)
				n.TxBytes.Add(uint64(len(frame)))
				if n.Recv != nil {
					n.Recv(frame)
				}
			}
		}
		q.Push(mem, ch.Head, 0)
		served = true
	}
	if served {
		t.Interrupt(netTxQueue)
	}
}

// gather copies the device-readable buffers of a chain, less the first skip
// bytes, into a new slice. It returns nil if nothing is left.
func gather(bufs []Buffer, skip int) []byte {
	size := 0
	for _, b := range bufs {
		if !b.Writable {
			size += len(b.Data)
		}
	}
	if size <= skip {
		return nil
	}
	out := make([]byte, 0, size-skip)
	for _, b := range bufs {
		if b.Writable {
			continue
		}
		d := b.Data
		k := min(skip, len(d))
		skip -= k
		out = append(out, d[k:]...)
	}
	return out
}

// flush delivers waiting frames, in order, while the guest has room for
// them, then raises one interrupt.
func (n *Net) flush(t *Transport) {
	q := t.Queue(netRxQueue)
	if q == nil {
		return
	}
	merge := t.DriverFeatures()&netFeatMrgRxbuf != 0
	served := false
	for len(n.pending) > 0 {
		used, ok := n.fill(t, q, n.pending[0], merge)
		served = served || used
		if !ok {
			break
		}
		n.pendingBytes -= len(n.pending[0])
		n.pending[0] = nil
		n.pending = n.pending[1:]
	}
	if len(n.pending) == 0 {
		n.pending = nil
	}
	if served {
		t.Interrupt(netRxQueue)
	}
}

// fill writes a virtio-net header and one frame into receive chains. With
// mergeable buffers the frame may span several chains, and the header says
// how many. It reports ok false, leaving every chain where it was, when the
// driver has not posted enough room yet; used says whether any chain went
// back to the driver. A frame that can never fit, or meets a malformed
// chain, is dropped.
func (n *Net) fill(t *Transport, q *Queue, frame []byte, merge bool) (used, ok bool) {
	mem := t.Memory()
	total := NetHeaderLen + len(frame)
	var chains []Chain
	room := 0
	for room < total {
		ch, more, err := q.Pop(mem)
		if !more {
			q.Unpop(len(chains))
			return false, false
		}
		chains = append(chains, ch)
		for _, b := range ch.Buffers {
			if b.Writable {
				room += len(b.Data)
			}
		}
		if err != nil || (!merge && room < total) {
			// A malformed chain, or without mergeable buffers one too
			// small to take the frame whole: give everything back unused
			// and drop the frame.
			for _, c := range chains {
				q.Push(mem, c.Head, 0)
			}
			n.RxDropped.Add(1)
			return true, true
		}
	}

	var hdr [NetHeaderLen]byte
	if t.DriverFeatures()&netFeatGuestCsum != 0 {
		hdr[0] = netHdrDataValid
	}
	le.PutUint16(hdr[10:], uint16(len(chains))) // num_buffers
	src := [][]byte{hdr[:], frame}
	for _, c := range chains {
		var written uint32
		for _, b := range c.Buffers {
			if b.Writable {
				written += uint32(copyFrom(&src, b.Data))
			}
		}
		q.Add(mem, c.Head, written)
	}
	// All of the frame's chains at once: the guest drops a frame whose
	// header counts more chains than it finds.
	q.Publish(mem)
	n.RxFrames.Add(1)
	n.RxBytes.Add(uint64(len(frame)))
	return true, true
}

// copyFrom fills dst from the front of src, consuming what it copies, and
// returns the number of bytes copied.
func copyFrom(src *[][]byte, dst []byte) int {
	n := 0
	for n < len(dst) && len(*src) > 0 {
		c := copy(dst[n:], (*src)[0])
		n += c
		(*src)[0] = (*src)[0][c:]
		if len((*src)[0]) == 0 {
			*src = (*src)[1:]
		}
	}
	return n
}
