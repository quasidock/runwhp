package virtio

import (
	"encoding/binary"
	"errors"
	"sync/atomic"
	"unsafe"
)

// MaxQueueSize is the largest queue a driver may configure.
const MaxQueueSize = 256

// Descriptor flags.
const (
	descNext  = 1
	descWrite = 2
)

// Queue is the device's view of one split virtqueue.
type Queue struct {
	Size  uint32 // entries, as set by the driver
	Ready bool
	Desc  uint64 // descriptor table
	Avail uint64 // driver ("available") ring
	Used  uint64 // device ("used") ring

	lastAvail   uint16
	unpublished uint16 // chains added but not yet published

	// With VIRTIO_RING_F_EVENT_IDX, the driver says through the rings when
	// it wants interrupts, and the device when it wants notifications.
	eventIdx       bool
	signalled      uint16 // the used index at the last interrupt check
	signalledValid bool
}

func (q *Queue) ready() bool { return q.Ready && q.Size > 0 && q.Size <= MaxQueueSize }

// Buffer is one descriptor of a chain.
type Buffer struct {
	Data     []byte
	Writable bool // device-writable (driver reads it)
}

// Chain is one request: a descriptor chain popped from the available ring.
type Chain struct {
	Head    uint16
	Buffers []Buffer
}

var errBadChain = errors.New("virtio: malformed descriptor chain")

// Pop takes the next chain the driver made available. ok is false when the
// ring is empty. A chain that points outside guest RAM or loops is returned
// with err set; it must still be completed with Push.
func (q *Queue) Pop(mem Memory) (c Chain, ok bool, err error) {
	c.Head, ok = q.next(mem)
	if !ok {
		return c, false, nil
	}
	err = q.walk(mem, c.Head, func(addr uint64, n uint32, writable bool) bool {
		data, ok := mem.Slice(addr, uint64(n))
		if ok {
			c.Buffers = append(c.Buffers, Buffer{data, writable})
		}
		return ok
	})
	return c, true, err
}

// Range is where one buffer of a chain lies in guest memory.
type Range struct {
	Addr     uint64
	Len      uint32
	Writable bool
}

// PopRanges takes the next chain as Pop does, but returns where its
// buffers lie instead of the buffers themselves, so the guest RAM they name
// is left untouched. The balloon uses it: its buffers are the RAM the
// guest has free. Each range must be RAM, as Contains reports when mem
// implements Discarder, or as Slice does otherwise.
func (q *Queue) PopRanges(mem Memory) (head uint16, ranges []Range, ok bool, err error) {
	head, ok = q.next(mem)
	if !ok {
		return 0, nil, false, nil
	}
	contains := func(addr, n uint64) bool { _, ok := mem.Slice(addr, n); return ok }
	if d, ok := mem.(Discarder); ok {
		contains = d.Contains
	}
	err = q.walk(mem, head, func(addr uint64, n uint32, writable bool) bool {
		ranges = append(ranges, Range{addr, n, writable})
		return contains(addr, uint64(n))
	})
	return head, ranges, true, err
}

// next takes the head of the next chain from the available ring. Finding
// it empty, with EVENT_IDX, it asks the driver to notify the device of the
// next chain it adds, and looks once more, for a chain the driver added
// before it could see the request. Until a device runs out, the driver adds
// chains without notifying it: a guest refills a receive queue, say,
// without an exit.
func (q *Queue) next(mem Memory) (head uint16, ok bool) {
	n := uint64(q.Size)
	avail, okA := mem.Slice(q.Avail, 4+2*n)
	if !okA {
		return 0, false
	}
	if q.lastAvail == loadIdx(avail) && (!q.eventIdx || !q.askForNotify(mem) || q.lastAvail == loadIdx(avail)) {
		return 0, false
	}
	head = le.Uint16(avail[4+2*(uint64(q.lastAvail)%n):])
	q.lastAvail++
	return head, true
}

// walk calls f with each descriptor of the chain starting at head, until f
// returns false, which makes the chain bad.
func (q *Queue) walk(mem Memory, head uint16, f func(addr uint64, n uint32, writable bool) bool) error {
	n := uint64(q.Size)
	next := uint64(head)
	for i := uint64(0); ; i++ {
		if i == n || next >= n {
			return errBadChain
		}
		desc, okD := mem.Slice(q.Desc+16*next, 16)
		if !okD {
			return errBadChain
		}
		flags := le.Uint16(desc[12:])
		if !f(le.Uint64(desc[0:]), le.Uint32(desc[8:]), flags&descWrite != 0) {
			return errBadChain
		}
		if flags&descNext == 0 {
			return nil
		}
		next = uint64(le.Uint16(desc[14:]))
	}
}

// Unpop puts back the last n chains taken with Pop, none of which may have
// been pushed, so the next Pop returns them again.
func (q *Queue) Unpop(n int) { q.lastAvail -= uint16(n) }

// Push returns a chain to the driver, reporting how many bytes the device
// wrote into its writable buffers.
func (q *Queue) Push(mem Memory, head uint16, written uint32) {
	q.Add(mem, head, written)
	q.Publish(mem)
}

// Add returns a chain to the driver as Push does, but leaves it unseen
// until Publish, which shows every chain added since at once. A network
// frame spread over several chains has to arrive whole: a guest polling the
// ring that finds the first chain, whose header counts them all, before the
// rest, drops the frame.
func (q *Queue) Add(mem Memory, head uint16, written uint32) {
	n := uint64(q.Size)
	used, ok := mem.Slice(q.Used, 4+8*n)
	if !ok {
		return
	}
	idx := le.Uint16(used[2:]) + q.unpublished
	slot := used[4+8*(uint64(idx)%n):]
	le.PutUint32(slot[0:], uint32(head))
	le.PutUint32(slot[4:], written)
	q.unpublished++
}

// Publish makes the chains added since the last Publish visible to the
// driver.
func (q *Queue) Publish(mem Memory) {
	if q.unpublished == 0 {
		return
	}
	used, ok := mem.Slice(q.Used, 4+8*uint64(q.Size))
	if !ok {
		return
	}
	// The guest may be reading the ring right now: publish the index only
	// after the elements.
	storeIdx(used, le.Uint16(used[2:])+q.unpublished)
	q.unpublished = 0
}

// loadIdx and storeIdx access the index of a ring whose first four bytes
// are {flags, idx}, atomically when that word is aligned, as rings laid out by
// Linux are. The guest runs while devices work on their queues, so plain
// accesses could be reordered around the ring contents they publish.
func loadIdx(ring []byte) uint16 {
	if p, ok := word(ring); ok {
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], atomic.LoadUint32(p))
		return le.Uint16(b[2:])
	}
	return le.Uint16(ring[2:])
}

func storeIdx(ring []byte, idx uint16) {
	if p, ok := word(ring); ok {
		var b [4]byte
		copy(b[:2], ring[:2]) // flags, which the same side owns
		le.PutUint16(b[2:], idx)
		atomic.StoreUint32(p, binary.NativeEndian.Uint32(b[:]))
		return
	}
	le.PutUint16(ring[2:], idx)
}

func word(ring []byte) (*uint32, bool) {
	p := unsafe.Pointer(&ring[0])
	return (*uint32)(p), uintptr(p)%4 == 0
}

// askForNotify sets avail_event, at the end of the used ring, to the next
// chain the device will take: a driver using EVENT_IDX notifies the device
// only when it adds that chain. It reports false if the ring is out of
// reach.
func (q *Queue) askForNotify(mem Memory) bool {
	n := uint64(q.Size)
	used, ok := mem.Slice(q.Used, 6+8*n)
	if !ok {
		return false
	}
	// A 16-bit store, which the processor makes at once: the two bytes
	// after the field may not be the ring's.
	le.PutUint16(used[4+8*n:], q.lastAvail)
	// The driver may add a chain and read avail_event between the two: it
	// would not notify, so the device must see the chain when it looks
	// again. The store has to reach memory before that load.
	fence()
	return true
}

// needInterrupt reports whether the driver wants an interrupt for what was
// pushed since it was last asked. Without EVENT_IDX it does unless it set
// VIRTQ_AVAIL_F_NO_INTERRUPT; with it, once the used index passes the
// used_event at the end of the available ring, as vring_need_event decides.
func (q *Queue) needInterrupt(mem Memory) bool {
	n := uint64(q.Size)
	if !q.eventIdx {
		avail, ok := mem.Slice(q.Avail, 2)
		return !ok || le.Uint16(avail)&1 == 0
	}
	avail, okA := mem.Slice(q.Avail, 6+2*n)
	used, okU := mem.Slice(q.Used, 4)
	if !okA || !okU {
		return true
	}
	// storeIdx published the used index with a locked store, so used_event
	// is read after it, as the driver requires.
	idx, old := loadIdx(used), q.signalled
	q.signalled = idx
	if !q.signalledValid {
		q.signalledValid = true
		return true
	}
	event := le.Uint16(avail[4+2*n:])
	return idx-event-1 < idx-old
}

// fence orders a plain store to guest memory before the loads after it. Any
// locked instruction does that on x86-64, the only processor runwhp runs on.
func fence() { fenceWord.Add(0) }

var fenceWord atomic.Uint32
