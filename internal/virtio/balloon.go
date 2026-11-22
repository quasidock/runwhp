package virtio

import (
	"fmt"
	"sync/atomic"
)

// Balloon is a virtio memory balloon that never inflates. It offers only
// free page reporting (VIRTIO_BALLOON_F_REPORTING): the guest's kernel hands
// it ranges of RAM it has had free for a while, and the balloon gives them
// back to the host through guest memory's Discarder, before returning them
// to the guest. The guest uses that RAM again by touching it.
//
// With only that feature, the driver sets up three queues: inflate,
// deflate and reporting. num_pages, the size the host asks the balloon to
// be, stays 0, so the first two stay idle.
type Balloon struct {
	// Reports counts the chains the guest reported, Ranges the free ranges
	// in them and Bytes their size; BadChains counts chains that named
	// memory that is not RAM, and were returned without giving anything
	// back.
	Reports, Ranges, Bytes, BadChains atomic.Uint64
}

const (
	balloonFeatReporting = 1 << 5

	balloonInflateQueue   = 0
	balloonDeflateQueue   = 1
	balloonReportingQueue = 2
)

func (b *Balloon) DeviceID() uint32 { return 5 }
func (b *Balloon) Features() uint64 { return FeatVersion1 | balloonFeatReporting }
func (b *Balloon) NumQueues() int   { return 3 }
func (b *Balloon) Reset()           {}

// Config is struct virtio_balloon_config's first two fields: num_pages,
// which stays 0, and actual, which the driver writes and the transport
// drops.
func (b *Balloon) Config() []byte { return make([]byte, 8) }

// Notify serves a queue on the vCPU that kicked it. The driver waits for
// each report to come back before it lets the guest use that RAM again,
// so giving it back cannot wait for another goroutine.
func (b *Balloon) Notify(t *Transport, q int) {
	qu := t.Queue(q)
	if qu == nil {
		return
	}
	mem := t.Memory()
	discarder, _ := mem.(Discarder)
	reporting := q == balloonReportingQueue && t.DriverFeatures()&balloonFeatReporting != 0
	served := false
	for {
		head, ranges, ok, err := qu.PopRanges(mem)
		if !ok {
			break
		}
		served = true
		switch {
		case !reporting:
			// Nothing to inflate or deflate: return whatever came.
		case err != nil:
			b.BadChains.Add(1)
		default:
			b.Reports.Add(1)
			for _, r := range ranges {
				b.Ranges.Add(1)
				b.Bytes.Add(uint64(r.Len))
				if discarder != nil {
					discarder.Discard(r.Addr, uint64(r.Len))
				}
			}
		}
		qu.Push(mem, head, 0)
	}
	if served {
		t.Interrupt(q)
	}
}

// Stats summarizes the balloon's work for the VMM's statistics.
func (b *Balloon) Stats() string {
	return fmt.Sprintf("balloon: %d reports of %d free ranges (%d MiB), %d bad",
		b.Reports.Load(), b.Ranges.Load(), b.Bytes.Load()>>20, b.BadChains.Load())
}
