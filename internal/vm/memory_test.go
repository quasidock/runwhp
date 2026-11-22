package vm

import (
	"errors"
	"sync"
	"testing"

	"github.com/quasidock/runwhp/internal/boot"
)

func TestSplitMemory(t *testing.T) {
	for _, c := range []struct{ size, low, high uint64 }{
		{512 << 20, 512 << 20, 0},
		{LowMemoryEnd, LowMemoryEnd, 0},
		{8 << 30, LowMemoryEnd, 8<<30 - LowMemoryEnd},
	} {
		if low, high := splitMemory(c.size); low != c.low || high != c.high {
			t.Errorf("splitMemory(%d MiB) = %d, %d MiB; want %d, %d", c.size>>20, low>>20, high>>20, c.low>>20, c.high>>20)
		}
	}
}

// fakeBacking records what guestMemory asks of the host, and fails the
// chunks in fail.
type fakeBacking struct {
	mu        sync.Mutex
	mapped    map[uint64]bool
	maps      map[uint64]int
	discarded [][2]uint64
	fail      map[uint64]bool
	t         *testing.T
}

func newFakeBacking(t *testing.T) *fakeBacking {
	return &fakeBacking{mapped: map[uint64]bool{}, maps: map[uint64]int{}, fail: map[uint64]bool{}, t: t}
}

func (b *fakeBacking) mapChunk(gpa uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gpa%ChunkSize != 0 || b.mapped[gpa] {
		b.t.Errorf("mapChunk(%#x): misaligned or mapped already", gpa)
	}
	if b.fail[gpa] {
		return errors.New("the paging file is too small")
	}
	b.mapped[gpa] = true
	b.maps[gpa]++
	return nil
}

func (b *fakeBacking) unmapChunk(gpa uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.mapped[gpa] {
		b.t.Errorf("unmapChunk(%#x): not mapped", gpa)
	}
	delete(b.mapped, gpa)
	return nil
}

func (b *fakeBacking) discard(gpa, n uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.discarded = append(b.discarded, [2]uint64{gpa, n})
	return nil
}

// 2 chunks below the hole and 2 above 4 GiB.
const testLow, testHigh = 2 * ChunkSize, 2 * ChunkSize

// testMemory makes a guestMemory whose low RAM is real, so it can be
// sliced, and whose high RAM exists only as far as Contains, Fault and
// Discard are concerned: the model would otherwise need a 4 GiB slice.
func testMemory(t *testing.T) (*guestMemory, *fakeBacking) {
	b := newFakeBacking(t)
	return newGuestMemory(make([]byte, testLow), testLow, testHigh, b), b
}

func TestGuestMemoryRanges(t *testing.T) {
	g, _ := testMemory(t)
	for _, c := range []struct {
		addr, n uint64
		ok      bool
	}{
		{0, testLow, true},
		{testLow - 8, 8, true},
		{testLow - 8, 9, false}, // runs into the hole
		{testLow, 0, true},
		{testLow, 1, false},
		{boot.HighMemoryBase - 1, 2, false}, // straddles the start of high RAM
		{boot.HighMemoryBase, testHigh, true},
		{boot.HighMemoryBase + testHigh - 1, 1, true},
		{boot.HighMemoryBase + testHigh, 1, false},
		{^uint64(0) - 2, 4, false}, // wraps
	} {
		if got := g.Contains(c.addr, c.n); got != c.ok {
			t.Errorf("Contains(%#x, %d) = %v, want %v", c.addr, c.n, got, c.ok)
		}
	}

	// Without high RAM, nothing above the hole is RAM, not even empty.
	low := newGuestMemory(make([]byte, testLow), testLow, 0, newFakeBacking(t))
	if low.Contains(boot.HighMemoryBase, 0) {
		t.Error("an empty span at 4 GiB counts as RAM in a guest without high RAM")
	}

	for i := range 4 {
		if got := g.chunk(g.chunkAddr(i)); got != i {
			t.Errorf("chunk(chunkAddr(%d)) = %d", i, got)
		}
	}
	if a := g.chunkAddr(2); a != boot.HighMemoryBase {
		t.Errorf("chunk 2 is at %#x, want 4 GiB", a)
	}
}

func TestSliceMapsOnce(t *testing.T) {
	g, b := testMemory(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			// Straddles the two low chunks.
			s, ok := g.Slice(ChunkSize-8, 16)
			if !ok || len(s) != 16 || cap(s) != 16 || &s[0] != &g.host[ChunkSize-8] {
				t.Errorf("Slice = %d bytes (cap %d), %v", len(s), cap(s), ok)
			}
		})
	}
	wg.Wait()
	if b.maps[0] != 1 || b.maps[ChunkSize] != 1 || len(b.maps) != 2 {
		t.Errorf("maps = %v; want each low chunk mapped once", b.maps)
	}
	if now, peak := g.Committed(); now != 2*ChunkSize || peak != 2*ChunkSize {
		t.Errorf("committed %d, peak %d", now, peak)
	}
	if _, ok := g.Slice(testLow-8, 9); ok {
		t.Error("Slice ran past the end of RAM")
	}
	// An empty span maps nothing.
	if _, ok := g.Slice(0, 0); !ok {
		t.Error("Slice(0, 0) failed")
	}
}

func TestFault(t *testing.T) {
	g, b := testMemory(t)
	if ram, err := g.Fault(LowMemoryEnd); ram || err != nil {
		t.Errorf("Fault on a device window = %v, %v", ram, err)
	}
	// An access near the end of a chunk maps the next one too, since it
	// may straddle them; one at the end of RAM maps only its own.
	if ram, err := g.Fault(boot.HighMemoryBase + ChunkSize - 4); !ram || err != nil {
		t.Fatalf("Fault = %v, %v", ram, err)
	}
	if !b.mapped[boot.HighMemoryBase] || !b.mapped[boot.HighMemoryBase+ChunkSize] {
		t.Errorf("mapped %v; want both high chunks", b.mapped)
	}
	if ram, err := g.Fault(testLow - 4); !ram || err != nil || !b.mapped[ChunkSize] || b.mapped[0] {
		t.Errorf("Fault at the end of low RAM = %v, %v; mapped %v", ram, err, b.mapped)
	}
	// Faulting where another vCPU has mapped already is harmless.
	if _, err := g.Fault(testLow - 4); err != nil || b.maps[ChunkSize] != 1 {
		t.Errorf("a second fault: %v, maps %v", err, b.maps)
	}

	// A chunk the host cannot commit fails the fault and the slice.
	b.fail[0] = true
	if _, err := g.Fault(16); err == nil {
		t.Error("Fault succeeded without memory")
	}
	if _, ok := g.Slice(16, 8); ok {
		t.Error("Slice succeeded without memory")
	}
	b.fail[0] = false
	if _, ok := g.Slice(16, 8); !ok {
		t.Error("Slice failed once memory was there")
	}
	if g.faults.Load() != 4 {
		t.Errorf("faults = %d, want 4", g.faults.Load())
	}
}

func TestDiscard(t *testing.T) {
	g, b := testMemory(t)
	if _, ok := g.Slice(0, testLow); !ok {
		t.Fatal("Slice failed")
	}
	if g.Discard(testLow-4096, 8192) {
		t.Error("Discard accepted a range running into the hole")
	}

	// A range covering chunk 1 whole and part of chunk 0: chunk 1 goes
	// back, and the part of chunk 0 is discarded.
	if !g.Discard(ChunkSize-4096, ChunkSize+4096) {
		t.Fatal("Discard refused RAM")
	}
	if b.mapped[ChunkSize] || !b.mapped[0] {
		t.Errorf("mapped %v; want only chunk 0", b.mapped)
	}
	if len(b.discarded) != 1 || b.discarded[0] != [2]uint64{ChunkSize - 4096, 4096} {
		t.Errorf("discarded %v", b.discarded)
	}
	if now, peak := g.Committed(); now != ChunkSize || peak != 2*ChunkSize {
		t.Errorf("committed %d, peak %d", now, peak)
	}

	// Chunks that are not mapped need nothing: neither unmapping nor
	// discarding, whole or in part.
	if !g.Discard(boot.HighMemoryBase, testHigh) || !g.Discard(ChunkSize+4096, 4096) || len(b.discarded) != 1 {
		t.Errorf("discarded %v", b.discarded)
	}

	// A chunk given back is mapped again when touched.
	if _, ok := g.Slice(ChunkSize, 8); !ok || !b.mapped[ChunkSize] || b.maps[ChunkSize] != 2 {
		t.Errorf("chunk 1 after a second touch: mapped %v, maps %v", b.mapped, b.maps)
	}
	if g.unmaps.Load() != 1 || g.discards.Load() != 1 || g.discarded.Load() != 4096 {
		t.Errorf("unmaps %d, discards %d of %d bytes", g.unmaps.Load(), g.discards.Load(), g.discarded.Load())
	}
}

// TestDiscardRacesFaults gives chunks back while other goroutines touch
// other chunks, as the balloon and the vCPUs do; run it with -race.
func TestDiscardRacesFaults(t *testing.T) {
	b := newFakeBacking(t)
	const low = 64 * ChunkSize
	g := newGuestMemory(make([]byte, low), low, 0, b)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for round := range 50 {
				for i := w; i < 64; i += 4 {
					a := uint64(i) * ChunkSize
					if (round+i)%2 == 0 {
						if _, err := g.Fault(a + 100); err != nil {
							t.Error(err)
						}
					} else {
						g.Discard(a, ChunkSize)
					}
				}
			}
		})
	}
	wg.Wait()
	now, _ := g.Committed()
	if int(now/ChunkSize) != len(b.mapped) {
		t.Errorf("committed %d chunks, but the host has %d mapped", now/ChunkSize, len(b.mapped))
	}
}
