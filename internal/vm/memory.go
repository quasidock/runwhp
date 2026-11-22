package vm

import (
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/quasidock/runwhp/internal/boot"
)

// LowMemoryEnd is as far as guest RAM goes below 4 GiB: the virtio windows,
// the I/O APIC and the local APIC lie above it. The rest of the guest's RAM
// starts at boot.HighMemoryBase.
const LowMemoryEnd = 0xd0000000

// ChunkSize is the unit in which guest RAM is committed, mapped and given
// back.
const ChunkSize = 2 << 20

// splitMemory divides size bytes of guest RAM into what lies below
// LowMemoryEnd and what lies above boot.HighMemoryBase.
func splitMemory(size uint64) (low, high uint64) {
	low = min(size, LowMemoryEnd)
	return low, size - low
}

// backing commits host memory to chunks of guest RAM and takes it back. It
// is called for distinct chunks from many goroutines at once.
type backing interface {
	// mapChunk commits the chunk at gpa and maps it into the guest.
	mapChunk(gpa uint64) error
	// unmapChunk unmaps the chunk at gpa and decommits it.
	unmapChunk(gpa uint64) error
	// discard tells the host that the n bytes at gpa, inside a mapped
	// chunk, need not be kept.
	discard(gpa, n uint64) error
}

// Chunk states. A chunk is busy while one goroutine maps or unmaps it.
const (
	chunkUnmapped = iota
	chunkBusy
	chunkMapped
)

// guestMemory is guest RAM: low bytes from address 0, and high bytes from
// boot.HighMemoryBase. One reservation of host address space holds both,
// so a guest physical address is also the offset of its byte in host; the
// hole between the two is never backed.
//
// RAM is committed and mapped a chunk at a time, the first time the guest
// or a device touches the chunk, and given back a chunk at a time when the
// guest reports it free. No lock is held across the host's calls: each
// chunk's state changes by compare-and-swap.
type guestMemory struct {
	host      []byte
	low, high uint64
	backing   backing
	chunks    []atomic.Uint32 // low RAM's chunks, then high RAM's

	faults, maps, unmaps, discards atomic.Uint64
	discarded                      atomic.Uint64 // bytes of partial chunks
	mapped, peak                   atomic.Int64  // chunks
}

func newGuestMemory(host []byte, low, high uint64, b backing) *guestMemory {
	return &guestMemory{host: host, low: low, high: high, backing: b,
		chunks: make([]atomic.Uint32, (low+high)/ChunkSize)}
}

// Contains reports whether the n bytes at guest physical address addr are
// all RAM, in one of its two ranges.
func (g *guestMemory) Contains(addr, n uint64) bool {
	end := addr + n
	if end < addr {
		return false
	}
	return end <= g.low || g.high > 0 && addr >= boot.HighMemoryBase && end <= boot.HighMemoryBase+g.high
}

// chunk returns the index of the chunk holding addr, which is RAM.
func (g *guestMemory) chunk(addr uint64) int {
	if addr < g.low {
		return int(addr / ChunkSize)
	}
	return int((g.low + addr - boot.HighMemoryBase) / ChunkSize)
}

// chunkAddr is the guest physical address of chunk i.
func (g *guestMemory) chunkAddr(i int) uint64 {
	if a := uint64(i) * ChunkSize; a < g.low {
		return a
	}
	return boot.HighMemoryBase + uint64(i)*ChunkSize - g.low
}

// Slice returns the n bytes of RAM at guest physical address addr, mapping
// the chunks they lie in first. It fails if they are not all RAM, or if
// the host cannot commit memory for them.
func (g *guestMemory) Slice(addr, n uint64) ([]byte, bool) {
	if !g.Contains(addr, n) {
		return nil, false
	}
	if g.ensure(addr, n) != nil {
		return nil, false
	}
	return g.host[addr : addr+n : addr+n], true
}

// ensure maps every chunk the n bytes of RAM at addr touch.
func (g *guestMemory) ensure(addr, n uint64) error {
	if n == 0 {
		return nil
	}
	for i, last := g.chunk(addr), g.chunk(addr+n-1); i <= last; i++ {
		if g.chunks[i].Load() == chunkMapped {
			continue
		}
		if err := g.mapChunk(i); err != nil {
			return err
		}
	}
	return nil
}

func (g *guestMemory) mapChunk(i int) error {
	for {
		switch g.chunks[i].Load() {
		case chunkMapped:
			return nil
		case chunkUnmapped:
			if !g.chunks[i].CompareAndSwap(chunkUnmapped, chunkBusy) {
				continue
			}
			if err := g.backing.mapChunk(g.chunkAddr(i)); err != nil {
				g.chunks[i].Store(chunkUnmapped)
				return fmt.Errorf("the host is out of memory to commit to the guest (%d MiB committed): %w",
					g.mapped.Load()*ChunkSize>>20, err)
			}
			g.chunks[i].Store(chunkMapped)
			g.maps.Add(1)
			if n := g.mapped.Add(1); n > g.peak.Load() {
				g.raisePeak(n)
			}
			return nil
		default:
			// Another goroutine is mapping or unmapping it.
			runtime.Gosched()
		}
	}
}

func (g *guestMemory) raisePeak(n int64) {
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

// Fault maps the RAM an access at gpa touches, for a vCPU that exited
// because gpa was not mapped. An access can straddle two chunks, so the
// chunk of the access's last possible byte is mapped too. It reports
// whether gpa is RAM; if not, the access is a device's.
func (g *guestMemory) Fault(gpa uint64) (bool, error) {
	if !g.Contains(gpa, 1) {
		return false, nil
	}
	g.faults.Add(1)
	n := uint64(64) // the widest access an instruction makes
	for !g.Contains(gpa, n) {
		n--
	}
	return true, g.ensure(gpa, n)
}

// Discard gives back the RAM in the n bytes at addr, which the guest has
// reported free: whole chunks are unmapped and decommitted, and the
// partial chunks at either end are discarded. It reports false if the
// range is not all RAM.
func (g *guestMemory) Discard(addr, n uint64) bool {
	if !g.Contains(addr, n) {
		return false
	}
	for end := addr + n; addr < end; {
		i := g.chunk(addr)
		start := g.chunkAddr(i)
		stop := min(end, start+ChunkSize)
		if addr == start && stop == start+ChunkSize {
			if g.chunks[i].CompareAndSwap(chunkMapped, chunkBusy) {
				if g.backing.unmapChunk(start) != nil {
					g.chunks[i].Store(chunkMapped)
				} else {
					g.chunks[i].Store(chunkUnmapped)
					g.unmaps.Add(1)
					g.mapped.Add(-1)
				}
			}
		} else if g.chunks[i].Load() == chunkMapped && g.backing.discard(addr, stop-addr) == nil {
			g.discards.Add(1)
			g.discarded.Add(stop - addr)
		}
		addr = stop
	}
	return true
}

// Committed reports how much guest RAM is committed and mapped now, and
// the most there has been.
func (g *guestMemory) Committed() (now, peak uint64) {
	return uint64(g.mapped.Load()) * ChunkSize, uint64(g.peak.Load()) * ChunkSize
}

// stats summarizes the memory's life for the VMM's statistics.
func (g *guestMemory) stats() string {
	now, peak := g.Committed()
	return fmt.Sprintf("memory: %d MiB, committed peak %d MiB, now %d MiB; %d faults, %d chunks mapped, %d given back (%d MiB), %d partial chunks discarded (%d KiB)",
		(g.low+g.high)>>20, peak>>20, now>>20, g.faults.Load(), g.maps.Load(), g.unmaps.Load(), g.unmaps.Load()*ChunkSize>>20,
		g.discards.Load(), g.discarded.Load()>>10)
}
