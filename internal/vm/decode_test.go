package vm

import "testing"

func TestOpcode(t *testing.T) {
	for _, c := range []struct {
		code []byte
		want uint16
	}{
		{[]byte{0x8b, 0x47, 0x60}, 0x8b},       // mov 0x60(%rdi),%eax
		{[]byte{0x44, 0x89, 0x40, 0x50}, 0x89}, // mov %r8d,0x50(%rax)
		{[]byte{0x66, 0x89, 0x07}, 0x89},       // mov %ax,(%rdi)
		{[]byte{0x0f, 0xb6, 0x07}, 0x0fb6},     // movzbl (%rdi),%eax
		{[]byte{0xf3, 0x48, 0xa5}, 0xa5},       // rep movsq
		{[]byte{0x66, 0x41, 0x0f, 0xb7, 0x00}, 0x0fb7},
		{nil, noOpcode},
		{[]byte{0x66, 0x48}, noOpcode},
		{[]byte{0x0f}, noOpcode},
	} {
		if got := opcode(c.code); got != c.want {
			t.Errorf("opcode(% x) = %#x, want %#x", c.code, got, c.want)
		}
	}
}

func TestTopOpcodes(t *testing.T) {
	got := topOpcodes(map[uint16]int{0x89: 40, 0x8b: 120, 0x0fb6: 3, noOpcode: 3, 0xa5: 1}, 4)
	if want := "8b=120 89=40 fb6=3 none=3"; got != want {
		t.Errorf("topOpcodes = %q, want %q", got, want)
	}
}
