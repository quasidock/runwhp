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

func TestDecodeMov(t *testing.T) {
	type op = movOp
	for _, c := range []struct {
		asm  string
		code []byte
		want op
		ok   bool
	}{
		{"mov 0x60(%rdi),%eax", []byte{0x8b, 0x47, 0x60}, op{size: 4, reg: 0, length: 3}, true},
		{"mov %esi,0x50(%rdi)", []byte{0x89, 0x77, 0x50}, op{write: true, size: 4, reg: 6, length: 3}, true},
		{"mov %r8d,0x50(%rax)", []byte{0x44, 0x89, 0x40, 0x50}, op{write: true, size: 4, reg: 8, length: 4}, true},
		{"mov (%rdi),%rax", []byte{0x48, 0x8b, 0x07}, op{size: 8, reg: 0, length: 3}, true},
		{"mov 0x60(%rdi),%r9", []byte{0x4c, 0x8b, 0x4f, 0x60}, op{size: 8, reg: 9, length: 4}, true},
		{"mov %ax,(%rdi)", []byte{0x66, 0x89, 0x07}, op{write: true, size: 2, reg: 0, length: 3}, true},
		{"mov 0x2(%rdi),%ax", []byte{0x66, 0x8b, 0x47, 0x02}, op{size: 2, reg: 0, merge: true, length: 4}, true},
		{"mov (%rdi),%al", []byte{0x8a, 0x07}, op{size: 1, reg: 0, merge: true, length: 2}, true},
		{"mov %al,0x10(%rdi)", []byte{0x88, 0x47, 0x10}, op{write: true, size: 1, reg: 0, length: 3}, true},
		{"mov %sil,0x10(%rdi)", []byte{0x40, 0x88, 0x77, 0x10}, op{write: true, size: 1, reg: 6, length: 4}, true},
		{"mov %spl,0x10(%rdi)", []byte{0x40, 0x88, 0x67, 0x10}, op{write: true, size: 1, reg: 4, length: 4}, true},
		{"movzbl (%rdi),%eax", []byte{0x0f, 0xb6, 0x07}, op{size: 1, reg: 0, length: 3}, true},
		{"movzwl 0x4(%rdi),%eax", []byte{0x0f, 0xb7, 0x47, 0x04}, op{size: 2, reg: 0, length: 4}, true},
		{"movzbl 0x1(%rdi),%r8d", []byte{0x44, 0x0f, 0xb6, 0x47, 0x01}, op{size: 1, reg: 8, length: 5}, true},
		{"movzbl (%rdi),%esp", []byte{0x0f, 0xb6, 0x27}, op{size: 1, reg: 4, length: 3}, true},
		{"mov (%rsp),%eax", []byte{0x8b, 0x04, 0x24}, op{size: 4, length: 3}, true},
		{"mov 0x8(%rsp),%eax", []byte{0x8b, 0x44, 0x24, 0x08}, op{size: 4, length: 4}, true},
		{"mov 0xfee00000,%eax", []byte{0x8b, 0x04, 0x25, 0x00, 0x00, 0xe0, 0xfe}, op{size: 4, length: 7}, true},
		{"mov 0x10(%rip),%eax", []byte{0x8b, 0x05, 0x10, 0x00, 0x00, 0x00}, op{size: 4, length: 6}, true},
		{"mov 0x100(%rdi),%eax", []byte{0x8b, 0x87, 0x00, 0x01, 0x00, 0x00}, op{size: 4, length: 6}, true},
		{"mov 0x100(%rdi,%rcx,4),%ecx", []byte{0x8b, 0x8c, 0x8f, 0x00, 0x01, 0x00, 0x00}, op{size: 4, reg: 1, length: 7}, true},
		{"trailing bytes", []byte{0x8b, 0x47, 0x60, 0x90, 0x90}, op{size: 4, length: 3}, true},

		{"mov %ah,0x10(%rdi)", []byte{0x88, 0x67, 0x10}, op{}, false},
		{"mov (%rdi),%bh", []byte{0x8a, 0x3f}, op{}, false},
		{"mov %eax,%edi", []byte{0x89, 0xc7}, op{}, false},
		{"movzbw (%rdi),%ax", []byte{0x66, 0x0f, 0xb6, 0x07}, op{}, false},
		{"lock", []byte{0xf0, 0x89, 0x07}, op{}, false},
		{"rep movsl", []byte{0xf3, 0xa5}, op{}, false},
		{"fs override", []byte{0x64, 0x8b, 0x07}, op{}, false},
		{"address size", []byte{0x67, 0x8b, 0x07}, op{}, false},
		{"66 after REX", []byte{0x48, 0x66, 0x89, 0x07}, op{}, false},
		{"two REX", []byte{0x48, 0x44, 0x89, 0x07}, op{}, false},
		{"mov $1,(%rdi)", []byte{0xc7, 0x07, 0x01, 0x00, 0x00, 0x00}, op{}, false},
		{"movsbl", []byte{0x0f, 0xbe, 0x07}, op{}, false},
		{"empty", nil, op{}, false},
		{"prefix only", []byte{0x66}, op{}, false},
		{"no modrm", []byte{0x8b}, op{}, false},
		{"no disp8", []byte{0x8b, 0x47}, op{}, false},
		{"no SIB", []byte{0x8b, 0x04}, op{}, false},
		{"short disp32", []byte{0x8b, 0x05, 0x10, 0x00}, op{}, false},
		{"0f alone", []byte{0x0f}, op{}, false},
		{"movzx without modrm", []byte{0x0f, 0xb6}, op{}, false},
	} {
		got, ok := decodeMov(c.code)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("%s (% x): got %+v, %v; want %+v, %v", c.asm, c.code, got, ok, c.want, c.ok)
		}
	}
}
