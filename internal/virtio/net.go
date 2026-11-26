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

	// netHdrNeedsCsum is VIRTIO_NET_HDR_F_NEEDS_CSUM: the frame's checksum
	// is partial, and csum_start and csum_offset say where to finish it.
	netHdrNeedsCsum = 1
	// netHdrDataValid is VIRTIO_NET_HDR_F_DATA_VALID: the frame's
	// checksums need no checking.
	netHdrDataValid = 2

	defaultNetBacklog = 4 << 20
)

// Offload is what a frame's virtio-net header says of its checksum. With
// NeedsCsum, the checksum of the bytes from Start to the end of the frame
// is yet to be folded in and stored at Start+Offset, where the sum of the
// pseudo-header already is, as Linux leaves a checksum it offloads.
type Offload struct {
	NeedsCsum     bool
	Start, Offset uint16
}

// Fits reports whether o can be applied to a frame of n bytes: a frame with
// a partial checksum must hold the checksum field where o puts it.
func (o Offload) Fits(n int) bool {
	return !o.NeedsCsum || int(o.Start)+int(o.Offset)+2 <= n
}

// Complete finishes a partial checksum in frame, which o must fit, and
// returns the offload the frame then has: none.
func (o Offload) Complete(frame []byte) Offload {
	if !o.NeedsCsum {
		return o
	}
	var sum uint32
	b := frame[o.Start:]
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	csum := ^uint16(sum)
	if csum == 0 {
		csum = 0xffff // a UDP checksum of 0 means none; 0xffff is the same sum
	}
	at := int(o.Start) + int(o.Offset)
	frame[at], frame[at+1] = byte(csum>>8), byte(csum)
	return Offload{}
}

// Net is a virtio network device with one queue pair and no control queue.
// It moves Ethernet frames between the guest and the host. It offers
// checksum offload both ways: the guest may send frames with partial
// checksums, which Recv learns of with each frame, and frames for the guest
// come marked valid, whatever their checksums say, when the guest takes
// that, or with their partial checksums left to it when DeliverWith says
// so. It offers no segmentation offload; the link's MTU is large enough as
// it is.
//
// Transmitted frames are copied out on the vCPU thread and handed to Recv.
// Frames for the guest go through Deliver from any goroutine; while the
// guest has no receive buffers posted they wait in a bounded backlog.
type Net struct {
	MAC [6]byte
	MTU uint16
	// Recv receives each frame the guest transmits, without its virtio
	// header, and what the header said of its checksum. The slice is Recv's
	// to keep. Recv is called with the transport locked, on the vCPU thread,
	// so it must not block and must not call Deliver.
	Recv func(frame []byte, off Offload)
	// Backlog caps the bytes held for the guest while it has no room to
	// receive them; frames beyond it are dropped. Zero means 4 MiB.
	Backlog int

	// TxFrames and TxBytes count frames from the guest, and TxDropped those
	// it sent that were not passed on; RxFrames and RxBytes count frames
	// delivered to it, and RxDropped frames for it that were thrown away.
	TxFrames, TxBytes, TxDropped, RxFrames, RxBytes, RxDropped atomic.Uint64

	t atomic.Pointer[Transport]

	// guestChecksums is set while the driver takes frames whose checksums
	// were not filled in, marked valid.
	guestChecksums atomic.Bool

	// Guarded by the transport's lock.
	pending      []netFrame
	pendingBytes int
	stopped      bool
}

// netFrame is a frame waiting for the guest.
type netFrame struct {
	data []byte
	off  Offload
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
func (n *Net) Deliver(frame []byte) { n.DeliverWith(frame, Offload{}) }

// DeliverWith is Deliver for a frame whose checksum may be partial, as
// another guest sent it. A guest that takes frames without checksums gets
// it as it is, with the header saying where to finish it; for one that
// does not, the device finishes it. A frame off does not fit is dropped.
func (n *Net) DeliverWith(frame []byte, off Offload) {
	t := n.t.Load()
	if t == nil || !off.Fits(len(frame)) || !t.Do(func() { n.deliver(t, netFrame{frame, off}) }) {
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

func (n *Net) deliver(t *Transport, f netFrame) {
	limit := n.Backlog
	if limit == 0 {
		limit = defaultNetBacklog
	}
	if n.stopped || n.pendingBytes+len(f.data) > limit {
		n.RxDropped.Add(1)
		return
	}
	n.pending = append(n.pending, f)
	n.pendingBytes += len(f.data)
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
			if frame, off, ok := txFrame(gather(ch.Buffers, 0)); !ok {
				n.TxDropped.Add(1)
			} else if frame != nil {
				n.TxFrames.Add(1)
				n.TxBytes.Add(uint64(len(frame)))
				if n.Recv != nil {
					n.Recv(frame, off)
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

// txFrame splits what a transmit chain holds into its header's offload and
// the frame after it. A chain with no frame after its header gives none;
// one asking for segmentation, which the device does not offer, or whose
// offload does not fit its frame, is not ok.
func txFrame(b []byte) (frame []byte, off Offload, ok bool) {
	if len(b) <= NetHeaderLen {
		return nil, Offload{}, true
	}
	hdr, frame := b[:NetHeaderLen], b[NetHeaderLen:]
	if hdr[1] != 0 { // gso_type
		return nil, Offload{}, false
	}
	if hdr[0]&netHdrNeedsCsum != 0 {
		off = Offload{NeedsCsum: true, Start: le.Uint16(hdr[6:]), Offset: le.Uint16(hdr[8:])}
	}
	return frame, off, off.Fits(len(frame))
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
		n.pendingBytes -= len(n.pending[0].data)
		n.pending[0] = netFrame{}
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
func (n *Net) fill(t *Transport, q *Queue, f netFrame, merge bool) (used, ok bool) {
	mem := t.Memory()
	frame := f.data
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
	switch {
	case t.DriverFeatures()&netFeatGuestCsum == 0:
		f.off.Complete(frame)
	case f.off.NeedsCsum:
		// Left partial, as the guest's own stack would leave it for a
		// device: the guest finishes it only if it sends the frame on.
		hdr[0] = netHdrNeedsCsum
		le.PutUint16(hdr[6:], f.off.Start)
		le.PutUint16(hdr[8:], f.off.Offset)
	default:
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
