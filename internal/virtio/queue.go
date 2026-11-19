package virtio

import "errors"

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

	lastAvail uint16
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
	n := uint64(q.Size)
	avail, okA := mem.Slice(q.Avail, 4+2*n)
	if !okA || q.lastAvail == le.Uint16(avail[2:]) {
		return c, false, nil
	}
	c.Head = le.Uint16(avail[4+2*(uint64(q.lastAvail)%n):])
	q.lastAvail++
	next := uint64(c.Head)
	for i := uint64(0); ; i++ {
		if i == n || next >= n {
			return c, true, errBadChain
		}
		desc, okD := mem.Slice(q.Desc+16*next, 16)
		if !okD {
			return c, true, errBadChain
		}
		flags := le.Uint16(desc[12:])
		data, okB := mem.Slice(le.Uint64(desc[0:]), uint64(le.Uint32(desc[8:])))
		if !okB {
			return c, true, errBadChain
		}
		c.Buffers = append(c.Buffers, Buffer{data, flags&descWrite != 0})
		if flags&descNext == 0 {
			return c, true, nil
		}
		next = uint64(le.Uint16(desc[14:]))
	}
}

// Push returns a chain to the driver, reporting how many bytes the device
// wrote into its writable buffers.
func (q *Queue) Push(mem Memory, head uint16, written uint32) {
	n := uint64(q.Size)
	used, ok := mem.Slice(q.Used, 4+8*n)
	if !ok {
		return
	}
	idx := le.Uint16(used[2:])
	slot := used[4+8*(uint64(idx)%n):]
	le.PutUint32(slot[0:], uint32(head))
	le.PutUint32(slot[4:], written)
	le.PutUint16(used[2:], idx+1)
}

// interruptsSuppressed reports whether the driver set VIRTQ_AVAIL_F_NO_INTERRUPT.
func (q *Queue) interruptsSuppressed(mem Memory) bool {
	avail, ok := mem.Slice(q.Avail, 2)
	return ok && le.Uint16(avail)&1 != 0
}
