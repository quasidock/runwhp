package boot

import (
	"bytes"
	"fmt"
	"strconv"
	"testing"
)

// fakeKernel returns a minimal image that ParseKernel accepts.
func fakeKernel(t *testing.T, version string) []byte {
	t.Helper()
	const setupSects = 2
	img := make([]byte, (setupSects+1)*512+4096)
	img[0x1f1] = setupSects
	le.PutUint16(img[0x1fe:], 0xaa55)
	img[0x201] = 0x66 // setup header ends at 0x268
	copy(img[0x202:], "HdrS")
	le.PutUint16(img[0x206:], 0x020f)
	le.PutUint32(img[0x22c:], 0x7fffffff) // initrd_addr_max
	le.PutUint16(img[0x236:], 1)          // XLF_KERNEL_64
	le.PutUint32(img[0x238:], 2047)       // cmdline_size
	le.PutUint64(img[0x258:], 0x1000000)  // pref_address
	le.PutUint32(img[0x260:], 0x800000)   // init_size
	if version != "" {
		le.PutUint16(img[0x20e:], 0x300-0x200)
		copy(img[0x300:], version+"\x00")
	}
	for i := (setupSects + 1) * 512; i < len(img); i++ {
		img[i] = byte(i)
	}
	return img
}

func TestParseKernel(t *testing.T) {
	k, err := ParseKernel(fakeKernel(t, "6.6.114.1-microsoft-standard-WSL2 (root@build) #1 SMP"))
	if err != nil {
		t.Fatal(err)
	}
	if k.Version != "6.6.114.1-microsoft-standard-WSL2" {
		t.Errorf("Version = %q", k.Version)
	}
	if k.MinMemory() != 0x1800000 {
		t.Errorf("MinMemory = %#x", k.MinMemory())
	}
	if _, err := ParseKernel(make([]byte, 4096)); err == nil {
		t.Error("accepted an image without a boot signature")
	}
	img := fakeKernel(t, "")
	le.PutUint16(img[0x236:], 0)
	if _, err := ParseKernel(img); err == nil {
		t.Error("accepted a kernel without a 64-bit entry point")
	}
}

func TestLoad(t *testing.T) {
	img := fakeKernel(t, "6.6")
	k, err := ParseKernel(img)
	if err != nil {
		t.Fatal(err)
	}
	mem := make([]byte, 64<<20)
	initrd := bytes.Repeat([]byte{0xab}, 10000)
	l, err := Load(mem, k, initrd, "console=ttyS0 quiet", testDevices(3))
	if err != nil {
		t.Fatal(err)
	}
	if l.Entry != 0x1000200 || !bytes.Equal(mem[0x1000000:0x1000000+4096], img[3*512:]) {
		t.Error("kernel payload not at pref_address, or wrong entry point")
	}
	zp := mem[ZeroPage:]
	if got := le.Uint32(zp[0x228:]); string(mem[got:got+20]) != "console=ttyS0 quiet\x00" {
		t.Errorf("cmd_line_ptr %#x does not point at the command line", got)
	}
	at, size := le.Uint32(zp[0x218:]), le.Uint32(zp[0x21c:])
	if size != 10000 || at%4096 != 0 || !bytes.Equal(mem[at:at+size], initrd) || uint64(at+size) > uint64(len(mem)) {
		t.Errorf("initramfs at %#x size %d is wrong", at, size)
	}
	if zp[0x1e8] != 3 || le.Uint64(zp[0x2d0+2*20+8:]) != uint64(len(mem))-highMem {
		t.Error("e820 map is wrong")
	}
	if le.Uint64(zp[0x70:]) == 0 {
		t.Error("acpi_rsdp_addr not set")
	}
	// The last 2 MiB page of the first GiB maps itself.
	pd := le.Uint64(mem[PML4Addr+0x2000+511*8:])
	if pd != 511<<21|0x83 {
		t.Errorf("page directory entry = %#x", pd)
	}

	small := make([]byte, 16<<20)
	if _, err := Load(small, k, nil, "", nil); err == nil {
		t.Error("loaded a kernel into too little memory")
	}
}

func testDevices(virtio int) []Device {
	devs := []Device{{Name: "COM1", HID: "PNP0501", IOBase: 0x3f8, IOLen: 8, GSI: 4}}
	for i := 0; i < virtio; i++ {
		devs = append(devs, Device{Name: fmt.Sprintf("VR%02d", i), HID: "LNRO0005", UID: i,
			MMIOBase: 0xd0000000 + uint32(i)<<12, MMIOLen: 0x1000, GSI: 5 + i})
	}
	return devs
}

// TestACPIChecksums walks RSDP -> XSDT -> FADT -> DSDT and the MADT and
// checks every table's checksum and length.
func TestACPIChecksums(t *testing.T) {
	mem := make([]byte, acpiEnd)
	rsdp, err := writeACPI(mem[acpiAddr:], acpiAddr, testDevices(4))
	if err != nil {
		t.Fatal(err)
	}
	sum := func(b []byte) (s byte) {
		for _, c := range b {
			s += c
		}
		return
	}
	if string(mem[rsdp:rsdp+8]) != "RSD PTR " || sum(mem[rsdp:rsdp+20]) != 0 || sum(mem[rsdp:rsdp+36]) != 0 {
		t.Fatal("bad RSDP")
	}
	table := func(at uint64, sig string) []byte {
		t.Helper()
		n := le.Uint32(mem[at+4:])
		tbl := mem[at : at+uint64(n)]
		if string(tbl[:4]) != sig || sum(tbl) != 0 {
			t.Fatalf("table %q at %#x: signature %q or checksum wrong", sig, at, tbl[:4])
		}
		return tbl
	}
	xsdt := table(le.Uint64(mem[rsdp+24:]), "XSDT")
	fadt := table(le.Uint64(xsdt[36:]), "FACP")
	table(le.Uint64(xsdt[44:]), "APIC")
	table(le.Uint64(fadt[140:]), "DSDT")
}

// spikeDSDT is the encoder from spike/boot_windows.go, kept verbatim as a
// reference: it is known to boot the WSL kernel.
func spikeDSDT(virtioDevices int) []byte {
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	pkg := func(op []byte, body []byte) []byte {
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
	interrupt := func(gsi int) []byte {
		d := []byte{0x89, 6, 0, 0x03, 1, 0, 0, 0, 0}
		le.PutUint32(d[5:], uint32(gsi))
		return d
	}
	endTag := []byte{0x79, 0}
	devices := [][]byte{device("COM1",
		name("_HID", []byte{0x0c, 0x41, 0xd0, 0x05, 0x01}),
		name("_UID", integer(0)),
		name("_CRS", buffer(cat(
			[]byte{0x47, 1, 0xf8, 0x03, 0xf8, 0x03, 1, 8},
			interrupt(4), endTag))),
	)}
	for i := 0; i < virtioDevices; i++ {
		window := []byte{0x86, 9, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
		le.PutUint32(window[4:], 0xd0000000+uint32(i)<<12)
		le.PutUint32(window[8:], 0x1000)
		devices = append(devices, device(fmt.Sprintf("VR%02d", i),
			name("_HID", cat([]byte{0x0d}, []byte("LNRO0005"), []byte{0})),
			name("_UID", integer(i)),
			name("_CRS", buffer(cat(window, interrupt(5+i), endTag))),
		))
	}
	return pkg([]byte{0x10}, cat(append([][]byte{[]byte(`\_SB_`)}, devices...)...))
}

func TestDSDTMatchesSpike(t *testing.T) {
	for _, n := range []int{0, 1, 2, 10} {
		got, err := dsdtAML(testDevices(n))
		if err != nil {
			t.Fatal(err)
		}
		if want := spikeDSDT(n); !bytes.Equal(got, want) {
			t.Errorf("%d virtio devices: DSDT differs from the spike's\n got % x\nwant % x", n, got, want)
		}
	}
}

// decodePkgLength returns the encoded length and the bytes it occupied.
func decodePkgLength(b []byte) (n, size int) {
	extra := int(b[0] >> 6)
	if extra == 0 {
		return int(b[0] & 0x3f), 1
	}
	n = int(b[0] & 0x0f)
	for i := 0; i < extra; i++ {
		n |= int(b[1+i]) << (4 + 8*i)
	}
	return n, 1 + extra
}

func TestPkgLength(t *testing.T) {
	for _, body := range []int{0, 61, 62, 63, 4000, 4092, 4093, 5000, 1 << 19, 1<<20 - 4} {
		enc, err := pkgLength(body)
		if err != nil {
			t.Fatal(err)
		}
		n, size := decodePkgLength(enc)
		if size != len(enc) || n != body+len(enc) {
			t.Errorf("body %d: encoded % x decodes to %d over %d bytes", body, enc, n, size)
		}
	}
}

// TestLargeDSDT checks that a device list too big for two-byte package
// lengths still produces a scope whose length covers exactly its devices.
func TestLargeDSDT(t *testing.T) {
	aml, err := dsdtAML(testDevices(100))
	if err != nil {
		t.Fatal(err)
	}
	if aml[0] != 0x10 {
		t.Fatalf("not a Scope: %#x", aml[0])
	}
	n, size := decodePkgLength(aml[1:])
	if size < 3 || 1+n != len(aml) {
		t.Fatalf("scope length %d (encoded in %d bytes) does not match AML of %d bytes", n, size, len(aml))
	}
	// Walk the devices inside the scope.
	p := 1 + size + 5 // skip \_SB_
	count := 0
	for p < len(aml) {
		if aml[p] != 0x5b || aml[p+1] != 0x82 {
			t.Fatalf("expected a Device at %d, found % x", p, aml[p:p+2])
		}
		n, _ := decodePkgLength(aml[p+2:])
		p += 2 + n
		count++
	}
	if p != len(aml) || count != 101 {
		t.Errorf("walked %d devices ending at %d of %d", count, p, len(aml))
	}
}

func TestHID(t *testing.T) {
	got, _ := hidValue("PNP0501")
	if !bytes.Equal(got, []byte{0x0c, 0x41, 0xd0, 0x05, 0x01}) {
		t.Errorf("PNP0501 = % x", got)
	}
	got, _ = hidValue("LNRO0005")
	if !bytes.Equal(got, append([]byte{0x0d}, "LNRO0005\x00"...)) {
		t.Errorf("LNRO0005 = % x", got)
	}
}

func TestCPIO(t *testing.T) {
	files := []File{
		{Name: "proc", Mode: ModeDir | 0o755},
		{Name: "init", Mode: ModeFile | 0o755, Data: []byte("#!init")},
		{Name: "runwhp", Mode: ModeDir | 0o755},
		{Name: "runwhp/spec.json", Mode: ModeFile | 0o644, Data: []byte(`{"args":["sh"]}`)},
	}
	archive := CPIO(files)
	if len(archive)%512 != 0 {
		t.Errorf("archive length %d not padded to 512", len(archive))
	}
	// Parse it back.
	field := func(b []byte, i int) int {
		v, err := strconv.ParseUint(string(b[6+8*i:14+8*i]), 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		return int(v)
	}
	align := func(n int) int { return (n + 3) &^ 3 }
	var got []File
	for p := 0; ; {
		h := archive[p:]
		if string(h[:6]) != "070701" {
			t.Fatalf("bad magic at %d", p)
		}
		mode, size, nameLen := field(h, 1), field(h, 6), field(h, 11)
		name := string(h[110 : 110+nameLen-1])
		dataAt := align(p+110+nameLen) - p
		data := h[dataAt : dataAt+size]
		p = align(p + dataAt + size)
		if name == "TRAILER!!!" {
			break
		}
		got = append(got, File{name, uint32(mode), data})
	}
	if len(got) != len(files) {
		t.Fatalf("got %d entries, want %d", len(got), len(files))
	}
	for i := range files {
		if got[i].Name != files[i].Name || got[i].Mode != files[i].Mode || !bytes.Equal(got[i].Data, files[i].Data) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], files[i])
		}
	}
}
