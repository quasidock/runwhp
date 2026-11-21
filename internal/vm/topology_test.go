package vm

import "testing"

// TestCPUIDTopology decodes the topology leaves the way Linux does and
// checks they describe one package of n single-threaded cores.
func TestCPUIDTopology(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 16, 64} {
		results := cpuidTopology(n)
		type key struct{ function, index, vp uint32 }
		perVP := map[key][4]uint32{}
		shared := map[[2]uint32]cpuidResult{}
		for _, r := range results {
			for i := range r.Output {
				if r.Output[i]&^r.Mask[i] != 0 {
					t.Errorf("%d CPUs: leaf %#x/%d sets bits outside its mask", n, r.Function, r.Index)
				}
			}
			if r.Flags&cpuidSubleaf == 0 {
				t.Errorf("%d CPUs: leaf %#x applies to every subleaf", n, r.Function)
			}
			if r.Flags&cpuidPerVP != 0 {
				if int(r.VpIndex) >= n {
					t.Errorf("%d CPUs: leaf %#x for VP %d", n, r.Function, r.VpIndex)
				}
				perVP[key{r.Function, r.Index, r.VpIndex}] = r.Output
			} else {
				shared[[2]uint32{r.Function, r.Index}] = r
			}
		}

		for _, leaf := range []uint32{0xb, 0x1f} {
			for vp := range uint32(n) {
				smt, core, end := perVP[key{leaf, 0, vp}], perVP[key{leaf, 1, vp}], perVP[key{leaf, 2, vp}]
				// Level type in ECX[15:8], count in EBX[15:0], x2APIC id in EDX.
				if smt[ecx]>>8&0xff != 1 || smt[ebx] != 1 || smt[eax]&0x1f != 0 || smt[edx] != vp {
					t.Errorf("%d CPUs: leaf %#x thread level on VP %d is %x", n, leaf, vp, smt)
				}
				shift := core[eax] & 0x1f
				if core[ecx]>>8&0xff != 2 || core[ebx] != uint32(n) || core[edx] != vp {
					t.Errorf("%d CPUs: leaf %#x core level on VP %d is %x", n, leaf, vp, core)
				}
				if vp>>shift != 0 || uint32(n-1)>>shift != 0 || (n > 1 && uint32(n-1)>>(shift-1) == 0) {
					t.Errorf("%d CPUs: core shift %d does not fit the APIC ids exactly", n, shift)
				}
				if end[ecx]>>8&0xff != 0 || end[ebx] != 0 {
					t.Errorf("%d CPUs: leaf %#x does not end after the core level: %x", n, leaf, end)
				}
			}
		}

		leaf1 := shared[[2]uint32{1, 0}]
		if got := leaf1.Output[ebx] >> 16 & 0xff; got != uint32(n) {
			t.Errorf("%d CPUs: leaf 1 counts %d logical processors", n, got)
		}
		if htt := leaf1.Output[edx]&(1<<28) != 0; htt != (n > 1) {
			t.Errorf("%d CPUs: leaf 1 HTT is %v", n, htt)
		}
		if got := shared[[2]uint32{0x80000008, 0}].Output[ecx] & 0xff; got != uint32(n-1) {
			t.Errorf("%d CPUs: leaf 0x80000008 counts %d cores", n, got+1)
		}
		for _, leaf := range []uint32{4, 0x8000001d} {
			if got := shared[[2]uint32{leaf, 3}].Output[eax]>>14&0xfff + 1; got != uint32(n) {
				t.Errorf("%d CPUs: leaf %#x has the L3 shared by %d", n, leaf, got)
			}
		}
	}
}
