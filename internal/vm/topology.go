package vm

import "math/bits"

// cpuidResult overrides bits of one CPUID leaf, the way
// WHV_X64_CPUID_RESULT2 does: where a bit of Mask is set, the guest reads
// the bit of Output instead of the hypervisor's.
type cpuidResult struct {
	Function, Index, VpIndex, Flags uint32
	Output, Mask                    [4]uint32 // eax, ebx, ecx, edx
}

const (
	cpuidSubleaf = 1 // WHvX64CpuidResult2FlagSubleafSpecific
	cpuidPerVP   = 2 // WHvX64CpuidResult2FlagVpSpecific
)

const (
	eax = iota
	ebx
	ecx
	edx
)

// cpuidTopology describes n vCPUs, whose APIC ids are 0 to n-1, as one
// package of n cores with one thread each, all sharing the last-level cache.
//
// WHP leaves the topology leaves empty, so without this Linux sees n
// single-core packages, each with a cache of its own. Its scheduler then
// never looks past a task's own CPU when placing woken tasks, and every
// secondary CPU calibrates its delay loop afresh, because it has no sibling
// to copy the boot CPU's from, which takes about 80 ms each.
//
// The leaves set are the ones Linux reads: 0xB, the extended topology leaf
// that Intel and AMD (with TOPOEXT) parse, and 0x1F, which Intel parses
// first where it exists; the processor counts in leaf 1 and in AMD's
// 0x80000008; and the number of threads sharing the L3 cache, in leaf 4 for
// Intel and 0x8000001D for AMD, assumed to be subleaf 3 on both.
func cpuidTopology(n int) []cpuidResult {
	shift := uint32(bits.Len(uint(n - 1))) // APIC id bits that number the cores
	all := func(function, index uint32, out, mask [4]uint32) cpuidResult {
		return cpuidResult{Function: function, Index: index, Flags: cpuidSubleaf, Output: out, Mask: mask}
	}
	const full = 0xffffffff
	htt := uint32(0)
	if n > 1 {
		htt = 1 << 28
	}
	out := []cpuidResult{
		// Leaf 1: EBX[23:16] logical processors per package, EDX[28] HTT.
		all(1, 0, [4]uint32{0, uint32(n) << 16, 0, htt}, [4]uint32{0, 0xff << 16, 0, 1 << 28}),
		// AMD 0x80000008: ECX[7:0] cores per package - 1, ECX[15:12]
		// APIC id bits for the core.
		all(0x80000008, 0, [4]uint32{0, 0, shift<<12 | uint32(n-1), 0}, [4]uint32{0, 0, 0xf0ff, 0}),
		// The L3 cache: EAX[25:14] threads sharing it - 1.
		all(4, 3, [4]uint32{uint32(n-1) << 14, 0, 0, 0}, [4]uint32{0xfff << 14, 0, 0, 0}),
		all(0x8000001d, 3, [4]uint32{uint32(n-1) << 14, 0, 0, 0}, [4]uint32{0xfff << 14, 0, 0, 0}),
	}
	// Leaves 0xB and 0x1F, per vCPU, since EDX is its x2APIC id. Subleaf 0
	// is the thread level: one thread per core, no id bits. Subleaf 1 is
	// the core level: n cores, shift bits. Subleaf 2 ends the list.
	for _, leaf := range []uint32{0xb, 0x1f} {
		for vp := range uint32(n) {
			levels := [][4]uint32{
				{0, 1, 1 << 8, vp},
				{shift, uint32(n), 2<<8 | 1, vp},
				{0, 0, 2, vp},
			}
			for sub, regs := range levels {
				out = append(out, cpuidResult{Function: leaf, Index: uint32(sub), VpIndex: vp,
					Flags: cpuidSubleaf | cpuidPerVP, Output: regs, Mask: [4]uint32{full, full, full, full}})
			}
		}
	}
	return out
}
