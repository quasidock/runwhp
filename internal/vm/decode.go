package vm

import (
	"fmt"
	"sort"
	"strings"
)

// isPrefix reports whether b is a legacy instruction prefix: operand or
// address size, lock, rep, or a segment override.
func isPrefix(b byte) bool {
	switch b {
	case 0x66, 0x67, 0xf0, 0xf2, 0xf3, 0x2e, 0x36, 0x3e, 0x26, 0x64, 0x65:
		return true
	}
	return false
}

// opcode returns an instruction's opcode past its prefixes: one byte, or
// 0x0F and the byte after it as 0x0Fxx. It returns noOpcode when the bytes
// run out first.
func opcode(code []byte) uint16 {
	i := 0
	for i < len(code) && (isPrefix(code[i]) || code[i]&0xf0 == 0x40) {
		i++
	}
	switch {
	case i >= len(code):
		return noOpcode
	case code[i] != 0x0f:
		return uint16(code[i])
	case i+1 < len(code):
		return 0x0f00 | uint16(code[i+1])
	}
	return noOpcode
}

const noOpcode = 0xffff

// topOpcodes lists the n most frequent opcodes in counts, as "8b=120 89=40".
func topOpcodes(counts map[uint16]int, n int) string {
	type count struct {
		op uint16
		n  int
	}
	var all []count
	for op, c := range counts {
		all = append(all, count{op, c})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n || all[i].n == all[j].n && all[i].op < all[j].op })
	var parts []string
	for i, c := range all {
		if i == n {
			break
		}
		if c.op == noOpcode {
			parts = append(parts, fmt.Sprintf("none=%d", c.n))
		} else {
			parts = append(parts, fmt.Sprintf("%x=%d", c.op, c.n))
		}
	}
	return strings.Join(parts, " ")
}

// movOp is a mov between a register and memory, the instruction Linux's
// readl, writel and their kin compile to.
type movOp struct {
	write  bool // from the register to memory
	size   int  // bytes moved to or from memory: 1, 2, 4 or 8
	reg    int  // the register, 0 to 15 in x86's order
	merge  bool // a read into the low byte or word: the rest of the register stays
	length int  // the instruction's length in bytes
}

// decodeMov decodes the instruction at the start of code if it is a mov
// between a general-purpose register and memory: 88, 89, 8A or 8B, or
// movzx, 0F B6 or 0F B7, with an operand-size prefix, a REX prefix or both,
// and any memory operand. It refuses everything else, and the forms it
// cannot place exactly: other prefixes, a register operand in place of the
// memory one, AH to BH, and movzx into a 16-bit register.
//
// The address the instruction touched comes with the exit, so the memory
// operand needs only its length.
func decodeMov(code []byte) (movOp, bool) {
	var op movOp
	i := 0
	opsize := false
	var rex byte
	for ; i < len(code); i++ {
		if b := code[i]; b == 0x66 && !opsize && rex == 0 {
			opsize = true
		} else if b&0xf0 == 0x40 && rex == 0 {
			rex = b
		} else {
			break
		}
	}
	// Any other prefix, or one after the REX, which voids it, is refused
	// with the opcodes this does not know.
	if i >= len(code) {
		return op, false
	}
	size := 4
	switch {
	case rex&0x08 != 0: // REX.W
		size = 8
	case opsize:
		size = 2
	}
	movzx := false
	switch code[i] {
	case 0x88, 0x8a:
		op.write, op.size, op.merge = code[i] == 0x88, 1, code[i] == 0x8a
	case 0x89, 0x8b:
		op.write, op.size, op.merge = code[i] == 0x89, size, code[i] == 0x8b && size == 2
	case 0x0f:
		i++
		if i >= len(code) || opsize || code[i] != 0xb6 && code[i] != 0xb7 {
			return op, false
		}
		movzx, op.size = true, 1
		if code[i] == 0xb7 {
			op.size = 2
		}
	default:
		return op, false
	}
	i++
	if i >= len(code) {
		return op, false
	}
	modrm := code[i]
	i++
	mod, rm := modrm>>6, modrm&7
	op.reg = int(modrm>>3&7) | int(rex&0x04)<<1 // REX.R
	if mod == 3 {
		return op, false // a register, not memory
	}
	if op.size == 1 && !movzx && rex == 0 && op.reg >= 4 {
		return op, false // AH, CH, DH or BH
	}
	disp := 0
	switch mod {
	case 1:
		disp = 1
	case 2:
		disp = 4
	}
	switch {
	case rm == 4: // a SIB byte follows
		if i >= len(code) {
			return op, false
		}
		if code[i]&7 == 5 && mod == 0 { // no base: a 32-bit displacement
			disp = 4
		}
		i++
	case rm == 5 && mod == 0: // RIP-relative
		disp = 4
	}
	op.length = i + disp
	if op.length > len(code) {
		return op, false
	}
	return op, true
}
