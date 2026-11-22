package vm

import "github.com/quasidock/runwhp/internal/boot"

// LowMemoryEnd is as far as guest RAM goes below 4 GiB: the virtio windows,
// the I/O APIC and the local APIC lie above it. The rest of the guest's RAM
// starts at boot.HighMemoryBase.
const LowMemoryEnd = 0xd0000000

// splitMemory divides size bytes of guest RAM into what lies below
// LowMemoryEnd and what lies above boot.HighMemoryBase.
func splitMemory(size uint64) (low, high uint64) {
	low = min(size, LowMemoryEnd)
	return low, size - low
}

// guestMemory is guest RAM: low bytes from address 0, and high bytes from
// boot.HighMemoryBase. One reservation of host address space holds both,
// so a guest physical address is also the offset of its byte in host; the
// hole between the two is never backed.
type guestMemory struct {
	host      []byte
	low, high uint64
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

// Slice returns the n bytes of RAM at guest physical address addr.
func (g *guestMemory) Slice(addr, n uint64) ([]byte, bool) {
	if !g.Contains(addr, n) {
		return nil, false
	}
	return g.host[addr : addr+n : addr+n], true
}
