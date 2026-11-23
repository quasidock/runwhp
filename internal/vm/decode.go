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
