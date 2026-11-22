package virtio

import (
	"sync"
	"testing"
)

// reportMem is guest memory that gives RAM back by recording it. It also
// records every slice taken of it, so a test can tell that the balloon
// never touched what the guest reported free.
type reportMem struct {
	testMem
	mu        sync.Mutex
	sliced    [][2]uint64
	discarded [][2]uint64
}

func (m *reportMem) Slice(addr, n uint64) ([]byte, bool) {
	m.mu.Lock()
	m.sliced = append(m.sliced, [2]uint64{addr, n})
	m.mu.Unlock()
	return m.testMem.Slice(addr, n)
}

func (m *reportMem) Contains(addr, n uint64) bool {
	_, ok := m.testMem.Slice(addr, n)
	return ok
}

func (m *reportMem) Discard(addr, n uint64) bool {
	m.mu.Lock()
	m.discarded = append(m.discarded, [2]uint64{addr, n})
	m.mu.Unlock()
	return m.Contains(addr, n)
}

func newBalloonDriver(t *testing.T) (*driver, *Balloon, *reportMem) {
	b := &Balloon{}
	var rm *reportMem
	d := newDriverWrapped(t, b, func(m testMem) Memory { rm = &reportMem{testMem: m}; return rm })
	return d, b, rm
}

func TestBalloonIdentity(t *testing.T) {
	d, b, _ := newBalloonDriver(t)
	if b.DeviceID() != 5 || b.NumQueues() != 3 || b.Features() != FeatVersion1|1<<5 {
		t.Errorf("device %d, %d queues, features %#x", b.DeviceID(), b.NumQueues(), b.Features())
	}
	// num_pages and actual both read 0, and the balloon never inflates.
	if d.tr.Read(regConfig, 4) != 0 || d.tr.Read(regConfig+4, 4) != 0 {
		t.Error("config is not zero")
	}
	d.tr.Write(regQueueSel, 4, 3)
	if d.tr.Read(regQueueNumMax, 4) != 0 {
		t.Error("a fourth queue exists")
	}
}

func TestBalloonReports(t *testing.T) {
	d, b, rm := newBalloonDriver(t)
	free1, free2, free3 := d.alloc(0x2000), d.alloc(0x1000), d.alloc(0x3000)
	first := d.add(balloonReportingQueue, buf{free1, 0x2000, true}, buf{free2, 0x1000, true})
	second := d.add(balloonReportingQueue, buf{free3, 0x3000, true})
	d.kick(balloonReportingQueue)

	u := d.collect(balloonReportingQueue)
	if len(u) != 2 || u[0] != (usedElem{first, 0}) || u[1] != (usedElem{second, 0}) {
		t.Errorf("used %+v", u)
	}
	if d.interrupts() != 1 {
		t.Errorf("%d interrupts for one kick", d.interrupts())
	}
	want := [][2]uint64{{free1, 0x2000}, {free2, 0x1000}, {free3, 0x3000}}
	if len(rm.discarded) != 3 || rm.discarded[0] != want[0] || rm.discarded[1] != want[1] || rm.discarded[2] != want[2] {
		t.Errorf("discarded %x, want %x", rm.discarded, want)
	}
	// Slicing reported RAM would make the host commit it, only to give it
	// back at once.
	for _, s := range rm.sliced {
		if s[0] < free3+0x3000 && s[0]+s[1] > free1 {
			t.Errorf("the balloon touched reported memory at %#x+%#x", s[0], s[1])
		}
	}
	if b.Reports.Load() != 2 || b.Ranges.Load() != 3 || b.Bytes.Load() != 0x6000 || b.BadChains.Load() != 0 {
		t.Errorf("stats: %s", b.Stats())
	}
}

func TestBalloonBadChain(t *testing.T) {
	d, b, rm := newBalloonDriver(t)
	good := d.alloc(0x1000)
	head := d.add(balloonReportingQueue, buf{good, 0x1000, true}, buf{uint64(len(rm.testMem)) - 0x800, 0x1000, true})
	d.kick(balloonReportingQueue)
	if u := d.collect(balloonReportingQueue); len(u) != 1 || u[0].head != head {
		t.Errorf("used %+v", u)
	}
	if len(rm.discarded) != 0 || b.BadChains.Load() != 1 {
		t.Errorf("discarded %x from a chain running past RAM; %s", rm.discarded, b.Stats())
	}
}

// TestBalloonIdleQueues returns what comes on the inflate and deflate
// queues, and on the reporting queue of a driver that did not accept
// reporting, without giving anything back.
func TestBalloonIdleQueues(t *testing.T) {
	d, b, rm := newBalloonDriver(t)
	for _, q := range []int{balloonInflateQueue, balloonDeflateQueue} {
		head := d.add(q, buf{d.alloc(64), 64, false})
		d.kick(q)
		if u := d.collect(q); len(u) != 1 || u[0].head != head {
			t.Errorf("queue %d: used %+v", q, u)
		}
	}
	d.tr.Write(regDrvFeatSel, 4, 0)
	d.tr.Write(regDrvFeatures, 4, 0) // only VERSION_1, in the high word
	head := d.add(balloonReportingQueue, buf{d.alloc(0x1000), 0x1000, true})
	d.kick(balloonReportingQueue)
	if u := d.collect(balloonReportingQueue); len(u) != 1 || u[0].head != head {
		t.Errorf("reporting without the feature: used %+v", u)
	}
	if len(rm.discarded) != 0 || b.Reports.Load() != 0 {
		t.Errorf("discarded %x; %s", rm.discarded, b.Stats())
	}
}

func TestBalloonReset(t *testing.T) {
	d, b, rm := newBalloonDriver(t)
	d.tr.Write(regStatus, 4, 0)
	d.tr.Write(regDrvFeatSel, 4, 0)
	d.tr.Write(regDrvFeatures, 4, 1<<5)
	d.setupQueue(balloonReportingQueue, 64)
	free := d.alloc(0x1000)
	d.add(balloonReportingQueue, buf{free, 0x1000, true})
	d.kick(balloonReportingQueue)
	if u := d.collect(balloonReportingQueue); len(u) != 1 || len(rm.discarded) != 1 || b.Reports.Load() != 1 {
		t.Errorf("after a reset: used %+v, discarded %x", u, rm.discarded)
	}
}

// TestPopRangesWithoutDiscarder checks that memory which cannot give RAM
// back still has a chain's ranges checked, through Slice.
func TestPopRangesWithoutDiscarder(t *testing.T) {
	d := newDriver(t, &Balloon{})
	q := &d.tr.queues[balloonReportingQueue]
	at := d.alloc(0x1000)
	d.add(balloonReportingQueue, buf{at, 0x1000, true}, buf{at + 0x1000, 8, false})
	d.add(balloonReportingQueue, buf{uint64(len(d.mem)), 1, true})
	head, ranges, ok, err := q.PopRanges(d.mem)
	if !ok || err != nil || head != 0 || len(ranges) != 2 || ranges[0] != (Range{at, 0x1000, true}) || ranges[1] != (Range{at + 0x1000, 8, false}) {
		t.Errorf("PopRanges = %d, %+v, %v, %v", head, ranges, ok, err)
	}
	if _, _, ok, err := q.PopRanges(d.mem); !ok || err == nil {
		t.Errorf("a range past RAM: %v, %v", ok, err)
	}
	if _, _, ok, _ := q.PopRanges(d.mem); ok {
		t.Error("popped from an empty ring")
	}
}
