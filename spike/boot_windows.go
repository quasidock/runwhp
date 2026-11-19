//go:build windows

package main

// Guest image construction: Linux 64-bit boot protocol, identity page tables,
// minimal hardware-reduced ACPI tables, and a newc cpio initramfs.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
)

var le = binary.LittleEndian

// Guest physical layout. Everything below 1 MiB except the tables is free RAM.
const (
	gdtAddr     = 0x1000
	zeroPage    = 0x7000
	pml4Addr    = 0x9000 // PML4, PDPT, then four page directories
	cmdlineAddr = 0x20000
	lowMemEnd   = 0x9fc00
	acpiAddr    = 0xe0000
	highMem     = 0x100000
	apicBase    = 0xfee00000
)

// Segment attributes: G | D/L | P | S | type.
const (
	attrCode64 = 0xa09b
	attrData   = 0xc093
)

type bootInfo struct {
	entry     uint64
	kernelAt  uint64
	initSize  uint32
	initrdAt  uint64
	protoVers uint16
}

// loadLinux places a bzImage, optional initramfs, command line, boot_params,
// page tables, GDT and ACPI tables into guest memory.
func loadLinux(mem, kernel, initrd []byte, cmdline string, virtioDevices int) (bootInfo, error) {
	var bi bootInfo
	if len(kernel) < 0x268 || le.Uint16(kernel[0x1fe:]) != 0xaa55 || string(kernel[0x202:0x206]) != "HdrS" {
		return bi, fmt.Errorf("not a bzImage (missing boot signature)")
	}
	bi.protoVers = le.Uint16(kernel[0x206:])
	if bi.protoVers < 0x020c || le.Uint16(kernel[0x236:])&1 == 0 {
		return bi, fmt.Errorf("kernel has no 64-bit entry point (protocol %#x)", bi.protoVers)
	}
	setupSects := int(kernel[0x1f1])
	if setupSects == 0 {
		setupSects = 4
	}
	payload := kernel[(setupSects+1)*512:]
	bi.kernelAt = le.Uint64(kernel[0x258:]) // pref_address
	bi.initSize = le.Uint32(kernel[0x260:])
	ram := uint64(len(mem))
	if bi.kernelAt+uint64(bi.initSize) > ram || uint64(len(payload)) > uint64(bi.initSize) {
		return bi, fmt.Errorf("kernel needs %d MiB at %#x; guest has %d MiB", bi.initSize>>20, bi.kernelAt, ram>>20)
	}
	copy(mem[bi.kernelAt:], payload)
	bi.entry = bi.kernelAt + 0x200 // startup_64

	// boot_params: copy the setup header, then fill in loader fields.
	zp := mem[zeroPage : zeroPage+0x1000]
	hdrEnd := 0x202 + int(kernel[0x201])
	copy(zp[0x1f1:hdrEnd], kernel[0x1f1:hdrEnd])
	zp[0x210] = 0xff  // type_of_loader: undefined
	zp[0x211] |= 0x01 // loadflags: LOADED_HIGH
	if len(cmdline)+1 > int(le.Uint32(kernel[0x238:])) {
		return bi, fmt.Errorf("command line too long")
	}
	copy(mem[cmdlineAddr:], cmdline)
	mem[cmdlineAddr+len(cmdline)] = 0
	le.PutUint32(zp[0x228:], cmdlineAddr)

	if len(initrd) > 0 {
		top := min(ram, uint64(le.Uint32(kernel[0x22c:]))+1)
		bi.initrdAt = (top - uint64(len(initrd))) &^ 0xfff
		if bi.initrdAt < bi.kernelAt+uint64(bi.initSize) {
			return bi, fmt.Errorf("initramfs does not fit above the kernel")
		}
		copy(mem[bi.initrdAt:], initrd)
		le.PutUint32(zp[0x218:], uint32(bi.initrdAt))
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

	le.PutUint64(zp[0x70:], writeACPI(mem[acpiAddr:], acpiAddr, virtioDevices)) // acpi_rsdp_addr

	// Identity-map the first 4 GiB with 2 MiB pages.
	const present, writable, huge = 1, 2, 0x80
	pdpt, pd := uint64(pml4Addr+0x1000), uint64(pml4Addr+0x2000)
	le.PutUint64(mem[pml4Addr:], pdpt|present|writable)
	for i := uint64(0); i < 4; i++ {
		le.PutUint64(mem[pdpt+i*8:], (pd+i*0x1000)|present|writable)
		for j := uint64(0); j < 512; j++ {
			le.PutUint64(mem[pd+i*0x1000+j*8:], (i<<30|j<<21)|present|writable|huge)
		}
	}

	// GDT: null, null, __BOOT_CS (0x10), __BOOT_DS (0x18).
	le.PutUint64(mem[gdtAddr+0x10:], 0x00af9b000000ffff)
	le.PutUint64(mem[gdtAddr+0x18:], 0x00cf93000000ffff)
	return bi, nil
}

// bootRegisters returns the vCPU state required by the 64-bit boot protocol.
func bootRegisters(bi bootInfo) ([]uint32, []regValue) {
	data := segment(0, 0xffffffff, 0x18, attrData)
	return []uint32{regRip, regRsi, regRflags, regCs, regDs, regEs, regSs, regFs, regGs, regGdtr, regCr3, regCr4, regEfer, regCr0},
		[]regValue{
			reg64(bi.entry), reg64(zeroPage), reg64(2),
			segment(0, 0xffffffff, 0x10, attrCode64), data, data, data, data, data,
			table(gdtAddr, 0x1f),
			reg64(pml4Addr),
			reg64(0x20),       // CR4.PAE
			reg64(0x500),      // EFER.LME | LMA
			reg64(0x80050033), // CR0: PG WP NE ET MP PE
		}
}

// writeACPI emits RSDP -> XSDT -> {FADT -> DSDT, MADT} and returns the RSDP
// address. The FADT declares hardware-reduced ACPI, so Linux assumes no PIC,
// PIT, RTC or i8042. The MADT lists one local APIC and one I/O APIC; the DSDT
// declares the serial port and the virtio-mmio devices with their interrupts.
func writeACPI(dst []byte, base uint64, virtioDevices int) uint64 {
	var buf bytes.Buffer
	place := func(tbl []byte) uint64 {
		for buf.Len()%16 != 0 {
			buf.WriteByte(0)
		}
		at := base + uint64(buf.Len())
		buf.Write(tbl)
		return at
	}
	sdt := func(sig string, rev byte, body []byte) []byte {
		t := make([]byte, 36+len(body))
		copy(t[0:], sig)
		le.PutUint32(t[4:], uint32(len(t)))
		t[8] = rev
		copy(t[10:], "runwhp  ")
		copy(t[16:], "runwhpSPIK")
		le.PutUint32(t[24:], 1)
		copy(t[28:], "runwhp")
		le.PutUint32(t[32:], 1)
		copy(t[36:], body)
		t[9] = checksum(t)
		return t
	}

	dsdt := place(sdt("DSDT", 2, dsdtAML(virtioDevices)))

	fadt := make([]byte, 276-36)
	f := func(off int) []byte { return fadt[off-36:] }
	le.PutUint16(f(109), 0x24)        // IAPC_BOOT_ARCH: no VGA, no CMOS RTC
	le.PutUint32(f(112), 1<<20|1<<10) // HW_REDUCED_ACPI | RESET_REG_SUP
	f(116)[0], f(116)[1] = 1, 8       // reset register: system I/O, 8 bits
	le.PutUint64(f(120), 0x64)        //   port 0x64
	f(128)[0] = 0xfe                  // reset value
	f(131)[0] = 5                     // FADT minor version
	le.PutUint64(f(140), dsdt)        // X_DSDT
	fadtAt := place(sdt("FACP", 6, fadt))

	madt := make([]byte, 8+8+12)
	le.PutUint32(madt[0:], apicBase)
	madt[8], madt[9] = 0, 8 // processor local APIC entry
	madt[10], madt[11] = 0, 0
	le.PutUint32(madt[12:], 1) // enabled
	madt[16], madt[17] = 1, 12 // I/O APIC entry
	madt[18] = 1               // I/O APIC id
	le.PutUint32(madt[20:], ioapicBase)
	le.PutUint32(madt[24:], 0) // first global system interrupt
	madtAt := place(sdt("APIC", 5, madt))

	xsdtBody := make([]byte, 16)
	le.PutUint64(xsdtBody[0:], fadtAt)
	le.PutUint64(xsdtBody[8:], madtAt)
	xsdtAt := place(sdt("XSDT", 1, xsdtBody))

	rsdp := make([]byte, 36)
	copy(rsdp[0:], "RSD PTR ")
	copy(rsdp[9:], "runwhp  ")
	rsdp[15] = 2
	le.PutUint32(rsdp[20:], 36)
	le.PutUint64(rsdp[24:], xsdtAt)
	rsdp[8] = checksum(rsdp[:20])
	rsdp[32] = checksum(rsdp)
	rsdpAt := place(rsdp)

	copy(dst, buf.Bytes())
	return rsdpAt
}

func checksum(b []byte) byte {
	var sum byte
	for _, c := range b {
		sum += c
	}
	return -sum
}

// cpioArchive builds an uncompressed newc initramfs holding /init and any
// kernel modules for it to load.
func cpioArchive(init []byte, modules map[string][]byte) []byte {
	var buf bytes.Buffer
	ino := 0
	add := func(name string, mode uint32, data []byte) {
		ino++
		nlink := 1
		if mode&0o170000 == 0o040000 {
			nlink = 2
		}
		fmt.Fprintf(&buf, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		buf.WriteString(name)
		buf.WriteByte(0)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
		buf.Write(data)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	add("proc", 0o040755, nil)
	add("sys", 0o040755, nil)
	add("init", 0o100755, init)
	add("modules", 0o040755, nil)
	for _, name := range slices.Sorted(maps.Keys(modules)) {
		add("modules/"+name, 0o100644, modules[name])
	}
	ino = -1
	add("TRAILER!!!", 0, nil)
	for buf.Len()%512 != 0 {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// dsdtAML hand-assembles the ACPI bytecode equivalent of:
//
//	Scope (\_SB) {
//	    Device (COM1) { _HID PNP0501; _CRS { IO 0x3f8/8; Interrupt 4 } }
//	    Device (VR0n) { _HID "LNRO0005"; _CRS { Memory32Fixed; Interrupt 5+n } }
//	}
//
// Interrupts are edge-triggered, active high.
func dsdtAML(virtioDevices int) []byte {
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	pkg := func(op []byte, body []byte) []byte { // op, PkgLength (counting itself), body
		if n := len(body) + 1; n < 64 {
			return cat(op, []byte{byte(n)}, body)
		}
		n := len(body) + 2
		return cat(op, []byte{0x40 | byte(n&0x0f), byte(n >> 4)}, body)
	}
	integer := func(v int) []byte { return []byte{0x0a, byte(v)} }
	name := func(seg string, value []byte) []byte { return cat([]byte{0x08}, []byte(seg), value) }
	buffer := func(data []byte) []byte { return pkg([]byte{0x11}, cat(integer(len(data)), data)) }
	device := func(seg string, body ...[]byte) []byte {
		return pkg([]byte{0x5b, 0x82}, cat(append([][]byte{[]byte(seg)}, body...)...))
	}
	interrupt := func(gsi int) []byte { // extended interrupt: consumer, edge, active high, exclusive
		d := []byte{0x89, 6, 0, 0x03, 1, 0, 0, 0, 0}
		le.PutUint32(d[5:], uint32(gsi))
		return d
	}
	endTag := []byte{0x79, 0}

	devices := [][]byte{device("COM1",
		name("_HID", []byte{0x0c, 0x41, 0xd0, 0x05, 0x01}), // EisaId("PNP0501")
		name("_UID", integer(0)),
		name("_CRS", buffer(cat(
			[]byte{0x47, 1, 0xf8, 0x03, 0xf8, 0x03, 1, 8}, // IO(Decode16, 0x3f8, 0x3f8, 1, 8)
			interrupt(uartIRQ), endTag))),
	)}
	for i := 0; i < virtioDevices; i++ {
		window := []byte{0x86, 9, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0} // Memory32Fixed(ReadWrite, base, 0x1000)
		le.PutUint32(window[4:], virtioBase+uint32(i)<<12)
		le.PutUint32(window[8:], 0x1000)
		devices = append(devices, device(fmt.Sprintf("VR%02d", i),
			name("_HID", cat([]byte{0x0d}, []byte("LNRO0005"), []byte{0})),
			name("_UID", integer(i)),
			name("_CRS", buffer(cat(window, interrupt(virtioIRQ0+i), endTag))),
		))
	}
	return pkg([]byte{0x10}, cat(append([][]byte{[]byte(`\_SB_`)}, devices...)...))
}
