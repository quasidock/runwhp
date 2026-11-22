package vm

import (
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

func TestGuestMemoryRanges(t *testing.T) {
	// 1 MiB below the hole and 1 MiB above 4 GiB. Contains never touches
	// host memory, so the model needs none.
	const low, high = 1 << 20, 1 << 20
	g := &guestMemory{low: low, high: high}
	for _, c := range []struct {
		addr, n uint64
		ok      bool
	}{
		{0, low, true},
		{low - 8, 8, true},
		{low - 8, 9, false}, // runs into the hole
		{low, 0, true},
		{low, 1, false},
		{boot.HighMemoryBase - 1, 2, false}, // straddles the start of high RAM
		{boot.HighMemoryBase, high, true},
		{boot.HighMemoryBase + high - 1, 1, true},
		{boot.HighMemoryBase + high, 1, false},
		{^uint64(0) - 2, 4, false}, // wraps
	} {
		if got := g.Contains(c.addr, c.n); got != c.ok {
			t.Errorf("Contains(%#x, %d) = %v, want %v", c.addr, c.n, got, c.ok)
		}
	}

	// Without high RAM, nothing above the hole is RAM, not even empty.
	g = &guestMemory{host: make([]byte, low), low: low}
	if g.Contains(boot.HighMemoryBase, 0) {
		t.Error("an empty span at 4 GiB counts as RAM in a guest without high RAM")
	}
	b, ok := g.Slice(low-8, 8)
	if !ok || len(b) != 8 || cap(b) != 8 || &b[0] != &g.host[low-8] {
		t.Errorf("Slice(low-8, 8) = %d bytes (cap %d), %v", len(b), cap(b), ok)
	}
	if _, ok := g.Slice(low-8, 9); ok {
		t.Error("Slice ran past the end of RAM")
	}
}
