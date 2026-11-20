// Package boot lays out a guest for direct Linux boot: the 64-bit boot
// protocol, identity page tables, hardware-reduced ACPI tables and a newc
// initramfs. It only writes bytes into a slice, so it builds and tests on any
// operating system.
package boot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

var le = binary.LittleEndian

// Guest physical layout. Everything below 1 MiB except the tables is free RAM.
const (
	GDTAddr     = 0x1000
	ZeroPage    = 0x7000
	PML4Addr    = 0x9000 // PML4, PDPT, then four page directories
	cmdlineAddr = 0x20000
	lowMemEnd   = 0x9fc00
	acpiAddr    = 0xe0000
	acpiEnd     = 0x100000
	highMem     = 0x100000

	IOAPICBase = 0xfec00000
	LAPICBase  = 0xfee00000
)

// Segment selectors and attributes (G | D/L | P | S | type) the boot protocol
// expects: __BOOT_CS and __BOOT_DS.
const (
	SelCode    = 0x10
	SelData    = 0x18
	AttrCode64 = 0xa09b
	AttrData   = 0xc093
)

// Kernel is a parsed bzImage.
type Kernel struct {
	// Version is the first word of the image's version string, which is
	// what uname -r reports in the guest.
	Version  string
	Protocol uint16

	image       []byte
	payload     []byte
	prefAddress uint64
	initSize    uint32
	cmdlineMax  uint32
	initrdMax   uint32
}

// ParseKernel validates a bzImage and reads its setup header.
func ParseKernel(image []byte) (*Kernel, error) {
	if len(image) < 0x268 || le.Uint16(image[0x1fe:]) != 0xaa55 || string(image[0x202:0x206]) != "HdrS" {
		return nil, fmt.Errorf("not a bzImage (missing boot signature)")
	}
	k := &Kernel{image: image, Protocol: le.Uint16(image[0x206:])}
	if k.Protocol < 0x020c || le.Uint16(image[0x236:])&1 == 0 {
		return nil, fmt.Errorf("kernel has no 64-bit entry point (boot protocol %#x)", k.Protocol)
	}
	setupSects := int(image[0x1f1])
	if setupSects == 0 {
		setupSects = 4
	}
	if (setupSects+1)*512 >= len(image) {
		return nil, fmt.Errorf("bzImage is truncated")
	}
	k.payload = image[(setupSects+1)*512:]
	k.prefAddress = le.Uint64(image[0x258:])
	k.initSize = le.Uint32(image[0x260:])
	k.cmdlineMax = le.Uint32(image[0x238:])
	k.initrdMax = le.Uint32(image[0x22c:])
	if off := int(le.Uint16(image[0x20e:])); off != 0 && off+0x200 < len(image) {
		s := image[off+0x200:]
		if i := bytes.IndexByte(s, 0); i >= 0 {
			s = s[:i]
		}
		if f := strings.Fields(string(s)); len(f) > 0 {
			k.Version = f[0]
		}
	}
	return k, nil
}

// MinMemory is the smallest guest RAM the kernel can decompress into.
func (k *Kernel) MinMemory() uint64 { return k.prefAddress + uint64(k.initSize) }

// Layout records where Load placed things.
type Layout struct {
	Entry    uint64
	KernelAt uint64
	InitSize uint32
	InitrdAt uint64
}

// Load places the kernel, an optional initramfs, the command line,
// boot_params, page tables, a GDT and the ACPI tables into guest memory,
// which starts at guest physical address 0.
func Load(mem []byte, k *Kernel, initrd []byte, cmdline string, devices []Device) (Layout, error) {
	var l Layout
	ram := uint64(len(mem))
	l.KernelAt, l.InitSize = k.prefAddress, k.initSize
	if l.KernelAt+uint64(l.InitSize) > ram || uint64(len(k.payload)) > uint64(l.InitSize) {
		return l, fmt.Errorf("kernel needs %d MiB at %#x; guest has %d MiB", l.InitSize>>20, l.KernelAt, ram>>20)
	}
	copy(mem[l.KernelAt:], k.payload)
	l.Entry = l.KernelAt + 0x200 // startup_64

	// boot_params: copy the setup header, then fill in loader fields.
	zp := mem[ZeroPage : ZeroPage+0x1000]
	clear(zp)
	hdrEnd := 0x202 + int(k.image[0x201])
	copy(zp[0x1f1:hdrEnd], k.image[0x1f1:hdrEnd])
	zp[0x210] = 0xff  // type_of_loader: undefined
	zp[0x211] |= 0x01 // loadflags: LOADED_HIGH
	if len(cmdline)+1 > int(k.cmdlineMax) || cmdlineAddr+len(cmdline)+1 > lowMemEnd {
		return l, fmt.Errorf("kernel command line too long (%d bytes)", len(cmdline))
	}
	copy(mem[cmdlineAddr:], cmdline)
	mem[cmdlineAddr+len(cmdline)] = 0
	le.PutUint32(zp[0x228:], cmdlineAddr)

	if len(initrd) > 0 {
		top := min(ram, uint64(k.initrdMax)+1)
		if uint64(len(initrd)) > top {
			return l, fmt.Errorf("initramfs (%d MiB) does not fit in guest memory", len(initrd)>>20)
		}
		l.InitrdAt = (top - uint64(len(initrd))) &^ 0xfff
		if l.InitrdAt < l.KernelAt+uint64(l.InitSize) {
			return l, fmt.Errorf("initramfs (%d MiB) does not fit above the kernel", len(initrd)>>20)
		}
		copy(mem[l.InitrdAt:], initrd)
		le.PutUint32(zp[0x218:], uint32(l.InitrdAt))
		le.PutUint32(zp[0x21c:], uint32(len(initrd)))
	}

	// e820 memory map.
	const e820RAM, e820Reserved = 1, 2
	entries := []struct {
		addr, size uint64
		kind       uint32
	}{
		{0, lowMemEnd, e820RAM},
		{lowMemEnd, highMem - lowMemEnd, e820Reserved},
		{highMem, ram - highMem, e820RAM},
	}
	for i, e := range entries {
		p := zp[0x2d0+i*20:]
		le.PutUint64(p[0:], e.addr)
		le.PutUint64(p[8:], e.size)
		le.PutUint32(p[16:], e.kind)
	}
	zp[0x1e8] = byte(len(entries))

	rsdp, err := writeACPI(mem[acpiAddr:acpiEnd], acpiAddr, devices)
	if err != nil {
		return l, err
	}
	le.PutUint64(zp[0x70:], rsdp) // acpi_rsdp_addr

	// Identity-map the first 4 GiB with 2 MiB pages.
	const present, writable, huge = 1, 2, 0x80
	pdpt, pd := uint64(PML4Addr+0x1000), uint64(PML4Addr+0x2000)
	le.PutUint64(mem[PML4Addr:], pdpt|present|writable)
	for i := uint64(0); i < 4; i++ {
		le.PutUint64(mem[pdpt+i*8:], (pd+i*0x1000)|present|writable)
		for j := uint64(0); j < 512; j++ {
			le.PutUint64(mem[pd+i*0x1000+j*8:], (i<<30|j<<21)|present|writable|huge)
		}
	}

	// GDT: null, null, __BOOT_CS (0x10), __BOOT_DS (0x18).
	le.PutUint64(mem[GDTAddr+SelCode:], 0x00af9b000000ffff)
	le.PutUint64(mem[GDTAddr+SelData:], 0x00cf93000000ffff)
	return l, nil
}

// Control register values for the 64-bit entry point.
const (
	CR0  = 0x80050033 // PG WP NE ET MP PE
	CR4  = 0x20       // PAE
	EFER = 0x500      // LME | LMA
)
