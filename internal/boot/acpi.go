package boot

import (
	"bytes"
	"fmt"
)

// Device is one device declared in the DSDT. Exactly one of IOLen and
// MMIOLen is non-zero.
type Device struct {
	Name     string // four-character ACPI name segment, such as "COM1"
	HID      string // _HID: an EISA id such as "PNP0501", or a string id such as "LNRO0005"
	UID      int
	IOBase   uint16
	IOLen    uint8
	MMIOBase uint32
	MMIOLen  uint32
	GSI      int // edge-triggered, active-high interrupt
}

// writeACPI emits RSDP -> XSDT -> {FADT -> DSDT, MADT} into dst, which sits
// at guest address base, and returns the RSDP address. The FADT declares
// hardware-reduced ACPI, so Linux assumes no PIC, PIT, RTC or i8042. The MADT
// lists one local APIC and one I/O APIC; the DSDT declares the devices.
func writeACPI(dst []byte, base uint64, devices []Device) (uint64, error) {
	var buf bytes.Buffer
	place := func(tbl []byte) uint64 {
		for buf.Len()%16 != 0 {
			buf.WriteByte(0)
		}
		at := base + uint64(buf.Len())
		buf.Write(tbl)
		return at
	}

	aml, err := dsdtAML(devices)
	if err != nil {
		return 0, err
	}
	dsdt := place(sdt("DSDT", 2, aml))

	fadt := make([]byte, 276-36)
	f := func(off int) []byte { return fadt[off-36:] }
	le.PutUint16(f(109), 0x24)        // IAPC_BOOT_ARCH: no VGA, no CMOS RTC
	le.PutUint32(f(112), 1<<20|1<<10) // HW_REDUCED_ACPI | RESET_REG_SUP
	f(116)[0], f(116)[1] = 1, 8       // reset register: system I/O, 8 bits
	le.PutUint64(f(120), ResetPort)   //   port
	f(128)[0] = ResetValue            // reset value
	f(131)[0] = 5                     // FADT minor version
	le.PutUint64(f(140), dsdt)        // X_DSDT
	fadtAt := place(sdt("FACP", 6, fadt))

	madt := make([]byte, 8+8+12)
	le.PutUint32(madt[0:], LAPICBase)
	madt[8], madt[9] = 0, 8 // processor local APIC entry
	madt[10], madt[11] = 0, 0
	le.PutUint32(madt[12:], 1) // enabled
	madt[16], madt[17] = 1, 12 // I/O APIC entry
	madt[18] = 1               // I/O APIC id
	le.PutUint32(madt[20:], IOAPICBase)
	le.PutUint32(madt[24:], 0) // first global system interrupt
	madtAt := place(sdt("APIC", 5, madt))

	xsdtBody := make([]byte, 16)
	le.PutUint64(xsdtBody[0:], fadtAt)
	le.PutUint64(xsdtBody[8:], madtAt)
	xsdtAt := place(sdt("XSDT", 1, xsdtBody))

	rsdp := make([]byte, 36)
	copy(rsdp[0:], "RSD PTR ")
	copy(rsdp[9:], "RUNWHP")
	rsdp[15] = 2
	le.PutUint32(rsdp[20:], 36)
	le.PutUint64(rsdp[24:], xsdtAt)
	rsdp[8] = checksum(rsdp[:20])
	rsdp[32] = checksum(rsdp)
	rsdpAt := place(rsdp)

	if buf.Len() > len(dst) {
		return 0, fmt.Errorf("ACPI tables need %d bytes; %d available", buf.Len(), len(dst))
	}
	copy(dst, buf.Bytes())
	return rsdpAt, nil
}

// The FADT's reset register. Writing ResetValue to ResetPort is how the guest
// asks to be reset; the VMM treats it as the guest stopping.
const (
	ResetPort  = 0x64
	ResetValue = 0xfe
)

func sdt(sig string, rev byte, body []byte) []byte {
	t := make([]byte, 36+len(body))
	copy(t[0:], sig)
	le.PutUint32(t[4:], uint32(len(t)))
	t[8] = rev
	copy(t[10:16], "RUNWHP")
	copy(t[16:24], "RUNWHP01")
	le.PutUint32(t[24:], 1)
	copy(t[28:32], "RWHP")
	le.PutUint32(t[32:], 1)
	copy(t[36:], body)
	t[9] = checksum(t)
	return t
}

func checksum(b []byte) byte {
	var sum byte
	for _, c := range b {
		sum += c
	}
	return -sum
}

// pkgLength encodes an AML PkgLength for a body of n bytes. The encoded
// length counts its own bytes.
func pkgLength(n int) ([]byte, error) {
	if n+1 < 64 {
		return []byte{byte(n + 1)}, nil
	}
	for extra := 1; extra <= 3; extra++ {
		total := n + 1 + extra
		if total < 1<<(4+8*extra) {
			b := []byte{byte(extra<<6) | byte(total&0x0f)}
			for i := 0; i < extra; i++ {
				b = append(b, byte(total>>(4+8*i)))
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("AML package of %d bytes is too large", n)
}

// dsdtAML hand-assembles the ACPI bytecode equivalent of:
//
//	Scope (\_SB) {
//	    Device (COM1) { _HID PNP0501; _UID 0; _CRS { IO 0x3f8/8; Interrupt 4 } }
//	    Device (VR00) { _HID "LNRO0005"; _UID 0; _CRS { Memory32Fixed; Interrupt 5 } }
//	    ...
//	}
//
// Interrupts are edge-triggered, active high, exclusive.
func dsdtAML(devices []Device) ([]byte, error) {
	var err error
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	pkg := func(op []byte, body []byte) []byte {
		n, e := pkgLength(len(body))
		if e != nil {
			err = e
		}
		return cat(op, n, body)
	}
	integer := func(v int) []byte {
		switch {
		case v < 0x100:
			return []byte{0x0a, byte(v)}
		case v < 0x10000:
			return []byte{0x0b, byte(v), byte(v >> 8)}
		default:
			return []byte{0x0c, byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
		}
	}
	name := func(seg string, value []byte) []byte { return cat([]byte{0x08}, []byte(seg), value) }
	buffer := func(data []byte) []byte { return pkg([]byte{0x11}, cat(integer(len(data)), data)) }
	interrupt := func(gsi int) []byte { // extended interrupt: consumer, edge, active high, exclusive
		d := []byte{0x89, 6, 0, 0x03, 1, 0, 0, 0, 0}
		le.PutUint32(d[5:], uint32(gsi))
		return d
	}
	endTag := []byte{0x79, 0}

	var body [][]byte
	body = append(body, []byte(`\_SB_`))
	for _, d := range devices {
		if len(d.Name) != 4 {
			return nil, fmt.Errorf("ACPI name %q is not four characters", d.Name)
		}
		hid, e := hidValue(d.HID)
		if e != nil {
			return nil, e
		}
		var res []byte
		switch {
		case d.IOLen != 0:
			res = []byte{0x47, 1, byte(d.IOBase), byte(d.IOBase >> 8), byte(d.IOBase), byte(d.IOBase >> 8), 1, d.IOLen}
		case d.MMIOLen != 0:
			res = make([]byte, 12) // Memory32Fixed(ReadWrite, base, length)
			res[0], res[1], res[3] = 0x86, 9, 1
			le.PutUint32(res[4:], d.MMIOBase)
			le.PutUint32(res[8:], d.MMIOLen)
		default:
			return nil, fmt.Errorf("ACPI device %s has no I/O or memory window", d.Name)
		}
		body = append(body, pkg([]byte{0x5b, 0x82}, cat(
			[]byte(d.Name),
			name("_HID", hid),
			name("_UID", integer(d.UID)),
			name("_CRS", buffer(cat(res, interrupt(d.GSI), endTag))),
		)))
	}
	out := pkg([]byte{0x10}, cat(body...))
	return out, err
}

// hidValue encodes a _HID: seven-character ids of the form AAA#### become a
// compressed EISA id; anything else is a string.
func hidValue(hid string) ([]byte, error) {
	isEISA := len(hid) == 7
	for i := 0; isEISA && i < 7; i++ {
		c := hid[i]
		if i < 3 && (c < 'A' || c > 'Z') || i >= 3 && !isHex(c) {
			isEISA = false
		}
	}
	if !isEISA {
		if len(hid) == 0 || len(hid) > 9 {
			return nil, fmt.Errorf("bad _HID %q", hid)
		}
		return append(append([]byte{0x0d}, hid...), 0), nil
	}
	var digits uint16
	fmt.Sscanf(hid[3:], "%04X", &digits)
	a, b, c := hid[0]-0x40, hid[1]-0x40, hid[2]-0x40
	return []byte{0x0c, a<<2 | b>>3, (b&7)<<5 | c, byte(digits >> 8), byte(digits)}, nil
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }
