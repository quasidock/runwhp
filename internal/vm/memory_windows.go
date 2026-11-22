//go:build windows

package vm

import "github.com/quasidock/runwhp/internal/whp"

// hostBacking backs guest RAM with memory of this process, a chunk at a
// time.
type hostBacking struct {
	part *whp.Partition
	base uintptr // where the reservation starts: guest address 0
}

func (b *hostBacking) mapChunk(gpa uint64) error {
	addr := b.base + uintptr(gpa)
	if err := whp.CommitMemory(addr, ChunkSize); err != nil {
		return err
	}
	if err := b.part.MapGpa(addr, gpa, ChunkSize, whp.MapRead|whp.MapWrite|whp.MapExecute); err != nil {
		whp.DecommitMemory(addr, ChunkSize)
		return err
	}
	// Without this, the hypervisor fills in its page tables one 4 KiB page
	// at a time, as the guest first touches each, at several microseconds
	// a page: 4 ms for a chunk the guest uses all of, against 0.2 ms to do
	// the whole chunk at once. It is only advice, so a failure costs only
	// time.
	b.part.PopulateGpa(gpa, ChunkSize)
	return nil
}

func (b *hostBacking) unmapChunk(gpa uint64) error {
	if err := b.part.UnmapGpa(gpa, ChunkSize); err != nil {
		return err
	}
	return whp.DecommitMemory(b.base+uintptr(gpa), ChunkSize)
}

func (b *hostBacking) discard(gpa, n uint64) error {
	return whp.DiscardMemory(b.base+uintptr(gpa), n)
}
