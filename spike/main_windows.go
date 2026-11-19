//go:build windows

// runwhp-spike boots a Linux kernel on the Windows Hypervisor Platform from an
// unelevated Go process and relays its serial console to stdout.
//
// It is an experiment, not a runtime: one vCPU, no network, no file sharing,
// and all device emulation runs synchronously on the vCPU thread.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"
)

const vp0 = 0

type vm struct {
	part     *partition
	uart     uart
	rtcIndex byte
	start    time.Time
	mem      []byte
	emu      *emulator
	ioapic   *ioapic
	disks    []*virtioBlk

	exits        map[uint32]int
	ioPorts      map[uint16]int
	mmioCount    int
	unknownPages map[uint64]bool
	stopReason   string
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...)
}

func main() {
	kernelPath := flag.String("kernel", `C:\Program Files\WSL\tools\kernel`, "bzImage to boot")
	initPath := flag.String("init", "", "static Linux executable to run as /init (optional)")
	memMiB := flag.Uint64("mem", 512, "guest RAM in MiB")
	cmdline := flag.String("cmdline", "console=ttyS0 earlyprintk=serial,ttyS0,115200 nokaslr panic=-1 reboot=acpi", "kernel command line")
	timeout := flag.Duration("timeout", 30*time.Second, "stop the guest after this long")
	enlighten := flag.Bool("hv", true, "expose Hyper-V enlightenments (clock frequencies) to the guest")
	var disks, modules listFlag
	flag.Var(&disks, "disk", "raw disk image to attach as a virtio block device; append :ro for read-only (repeatable)")
	flag.Var(&modules, "module", "kernel module to place in /modules for /init to load (repeatable)")
	flag.Parse()

	if err := run(*kernelPath, *initPath, *cmdline, *memMiB<<20, *timeout, *enlighten, disks, modules); err != nil {
		logf("error: %v", err)
		os.Exit(1)
	}
}

func run(kernelPath, initPath, cmdline string, ramSize uint64, timeout time.Duration, enlighten bool, diskSpecs, modulePaths []string) error {
	start := time.Now()
	since := func() string { return fmt.Sprintf("%4d ms", time.Since(start).Milliseconds()) }

	kernel, err := os.ReadFile(kernelPath)
	if err != nil {
		return err
	}
	var initrd []byte
	if initPath != "" {
		init, err := os.ReadFile(initPath)
		if err != nil {
			return err
		}
		modules := map[string][]byte{}
		for _, path := range modulePaths {
			if modules[filepath.Base(path)], err = os.ReadFile(path); err != nil {
				return err
			}
		}
		initrd = cpioArchive(init, modules)
	}

	var present uint32
	if err := capability(capHypervisorPresent, unsafe.Pointer(&present), 4); err != nil || present == 0 {
		return fmt.Errorf("Windows Hypervisor Platform is not available (present=%d, err=%v)", present, err)
	}
	var tscHz, apicHz uint64
	capability(capProcessorClockFreq, unsafe.Pointer(&tscHz), 8)
	capability(capInterruptClockFreq, unsafe.Pointer(&apicHz), 8)

	part, err := createPartition()
	if err != nil {
		return err
	}
	defer part.close()

	count := uint32(1)
	if err := part.setProperty(propProcessorCount, unsafe.Pointer(&count), 4); err != nil {
		return err
	}
	apicMode := uint32(apicModeXApic)
	if err := part.setProperty(propLocalApicEmulationMode, unsafe.Pointer(&apicMode), 4); err != nil {
		return err
	}
	msrAction := uint32(msrIgnoreWriteReadZero)
	if err := part.setProperty(propUnimplementedMsrAction, unsafe.Pointer(&msrAction), 4); err != nil {
		logf("unimplemented-MSR action not set: %v", err)
	}
	if enlighten {
		// WHV_SYNTHETIC_PROCESSOR_FEATURES_BANKS {BanksCount, Reserved, Bank0}
		var supported, wanted [2]uint64
		if err := capability(capSyntheticFeaturesBanks, unsafe.Pointer(&supported), 16); err != nil {
			return err
		}
		wanted[0] = 1
		wanted[1] = supported[1] & (synHypervisorPresent | synHv1 | synVpRunTime | synPartitionRefCount |
			synIntrCtrlRegs | synHypercallRegs | synVpIndex | synPartitionRefTsc | synFrequencyRegs)
		if err := part.setProperty(propSyntheticFeaturesBanks, unsafe.Pointer(&wanted), 16); err != nil {
			return err
		}
		logf("synthetic features: supported %#x, enabled %#x", supported[1], wanted[1])
	}
	if err := part.setup(); err != nil {
		return err
	}

	host, mem, err := allocGuestMemory(ramSize)
	if err != nil {
		return err
	}
	if err := part.mapGpa(host, 0, ramSize, mapRead|mapWrite|mapEx); err != nil {
		return err
	}
	bi, err := loadLinux(mem, kernel, initrd, cmdline, len(diskSpecs))
	if err != nil {
		return err
	}
	if err := part.createVP(vp0); err != nil {
		return err
	}
	defer part.deleteVP(vp0)
	names, values := bootRegisters(bi)
	if err := part.setRegs(vp0, names, values); err != nil {
		return err
	}
	logf("%s  ready: %d MiB RAM, kernel %d MiB at %#x (init_size %d MiB), initramfs %d KiB, tsc %d MHz, apic %d MHz",
		since(), ramSize>>20, len(kernel)>>20, bi.kernelAt, bi.initSize>>20, len(initrd)>>10, tscHz/1e6, apicHz/1e6)

	v := &vm{part: part, start: start, mem: mem, ioapic: &ioapic{part: part},
		exits: map[uint32]int{}, ioPorts: map[uint16]int{}, unknownPages: map[uint64]bool{}}
	if v.emu, err = newEmulator(v); err != nil {
		return err
	}
	for i, spec := range diskSpecs {
		disk, err := openDisk(v, i, spec)
		if err != nil {
			return err
		}
		defer disk.file.Close()
		v.disks = append(v.disks, disk)
	}
	out := bufio.NewWriterSize(os.Stdout, 4096)
	defer out.Flush()
	v.uart.out = out
	v.uart.iir = 0x01
	v.uart.raise = func() { v.ioapic.pulse(uartIRQ) }
	v.uart.onLine = func(line string) {
		switch {
		case v.uart.lines == 1:
			logf("%s  first console line", since())
		case strings.Contains(line, "Run /init as init process"):
			logf("%s  kernel hands over to /init", since())
		case strings.Contains(line, "runwhp-init: hello"):
			logf("%s  userspace is running", since())
		}
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-interrupt:
			v.stopReason = "interrupted"
		case <-time.After(timeout):
			v.stopReason = "timeout after " + timeout.String()
		case <-done:
			return
		}
		part.cancel(vp0)
	}()

	runtime.LockOSThread()
	err = v.loop()
	close(done)
	out.Flush()

	logf("%s  stopped: %s", since(), v.stopReason)
	v.report()
	return err
}

func (v *vm) loop() error {
	var ctx exitContext
	for {
		if err := v.part.run(vp0, &ctx); err != nil {
			return err
		}
		reason := ctx.reason()
		v.exits[reason]++
		switch reason {
		case exitIoPort:
			stop, err := v.handleIO(&ctx)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		case exitHalt:
			if ctx.rflags()&0x200 == 0 {
				v.stopReason = "guest halted with interrupts disabled"
				return nil
			}
		case exitCanceled:
			return nil
		case exitUnrecoverableException:
			v.stopReason = "triple fault"
			v.dump()
			return nil
		case exitMemoryAccess:
			if err := v.emu.mmio(&ctx); err != nil {
				v.dump()
				return err
			}
		case exitApicEoi:
			// Only level-triggered interrupts report EOI; none are used.
		default:
			v.dump()
			return fmt.Errorf("unhandled exit reason %#x at rip %#x", reason, ctx.rip())
		}
	}
}

func (v *vm) handleIO(ctx *exitContext) (stop bool, err error) {
	info := ctx.ioInfo()
	port := ctx.ioPort()
	size := (info >> 1) & 7
	if info&0x10 != 0 {
		b := ctx.bytes()
		v.dump()
		return false, fmt.Errorf("string I/O on port %#x is not implemented (rip %#x, bytes % x)",
			port, ctx.rip(), b[52:52+int(b[48])])
	}
	v.ioPorts[port]++
	rax := ctx.ioRax()
	if info&1 != 0 { // OUT
		stop = v.portOut(port, uint32(rax))
	} else { // IN
		val := uint64(v.portIn(port))
		switch size {
		case 1:
			rax = rax&^0xff | val&0xff
		case 2:
			rax = rax&^0xffff | val&0xffff
		default:
			rax = val & 0xffffffff
		}
	}
	err = v.part.setRegs(vp0, []uint32{regRip, regRax}, []regValue{reg64(ctx.rip() + ctx.instrLen()), reg64(rax)})
	return stop, err
}

func (v *vm) portIn(port uint16) uint32 {
	switch {
	case port >= 0x3f8 && port <= 0x3ff:
		return uint32(v.uart.read(port - 0x3f8))
	case port == 0x71:
		return uint32(v.rtcRead())
	}
	return 0xffffffff // nothing there: PIC, PIT, PCI and i8042 all read as absent
}

// rtcRead answers CMOS clock reads. Linux 6.6 reads the RTC even when the
// FADT says there is none, and spends over a second retrying if it is absent.
func (v *vm) rtcRead() byte {
	bcd := func(n int) byte { return byte(n/10<<4 | n%10) }
	now := time.Now().UTC()
	switch v.rtcIndex {
	case 0x00:
		return bcd(now.Second())
	case 0x02:
		return bcd(now.Minute())
	case 0x04:
		return bcd(now.Hour())
	case 0x06:
		return bcd(int(now.Weekday()) + 1)
	case 0x07:
		return bcd(now.Day())
	case 0x08:
		return bcd(int(now.Month()))
	case 0x09:
		return bcd(now.Year() % 100)
	case 0x32:
		return bcd(now.Year() / 100)
	case 0x0a:
		return 0x26 // no update in progress
	case 0x0b:
		return 0x02 // 24-hour, BCD
	case 0x0d:
		return 0x80 // battery good
	}
	return 0
}

func (v *vm) portOut(port uint16, val uint32) (stop bool) {
	switch {
	case port >= 0x3f8 && port <= 0x3ff:
		v.uart.write(port-0x3f8, byte(val))
	case port == 0x70:
		v.rtcIndex = byte(val) & 0x7f
	case port == 0x64 && byte(val) == 0xfe:
		// ACPI reset register (see writeACPI) and the i8042 reset pulse.
		v.stopReason = "guest requested reset"
		return true
	}
	return false
}

func (v *vm) dump() {
	names := []uint32{regRip, regRsp, regRflags, regCr0, regCr2, regCr3, regCr4, regEfer}
	values := make([]regValue, len(names))
	if err := v.part.getRegs(vp0, names, values); err != nil {
		logf("register dump failed: %v", err)
		return
	}
	logf("rip=%#x rsp=%#x rflags=%#x cr0=%#x cr2=%#x cr3=%#x cr4=%#x efer=%#x",
		values[0][0], values[1][0], values[2][0], values[3][0], values[4][0], values[5][0], values[6][0], values[7][0])
}

func (v *vm) report() {
	names := map[uint32]string{exitIoPort: "io", exitHalt: "halt", exitCanceled: "canceled",
		exitMemoryAccess: "mmio", exitUnrecoverableException: "triple-fault"}
	var parts []string
	for reason, n := range v.exits {
		name := names[reason]
		if name == "" {
			name = fmt.Sprintf("%#x", reason)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	sort.Strings(parts)
	logf("exits: %s", strings.Join(parts, " "))

	type portCount struct {
		port uint16
		n    int
	}
	var ports []portCount
	for p, n := range v.ioPorts {
		ports = append(ports, portCount{p, n})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].n > ports[j].n })
	parts = parts[:0]
	for i, p := range ports {
		if i == 8 {
			break
		}
		parts = append(parts, fmt.Sprintf("%#x=%d", p.port, p.n))
	}
	logf("busiest ports: %s", strings.Join(parts, " "))
	logf("console: %d lines, %d bytes", v.uart.lines, v.uart.bytes)
	logf("mmio accesses: %d, interrupts delivered through the I/O APIC: %d", v.mmioCount, v.ioapic.delivered)
	for i, d := range v.disks {
		logf("disk %d: %d requests, %d MiB read, %d MiB written", i, d.requests, d.bytesRead>>20, d.bytesWritten>>20)
	}
}

// uart is an output-only 16550A at COM1. It raises a transmit-ready interrupt
// so the guest's tty driver works, but it never delivers input.
type uart struct {
	ier, lcr, mcr, scr, dll, dlm, fcr byte
	iir                               byte   // pending interrupt identification; 0x01 means none
	raise                             func() // signals an edge on the UART's interrupt line
	out                               *bufio.Writer
	line                              []byte
	lines, bytes                      int
	onLine                            func(string)
}

func (u *uart) dlab() bool { return u.lcr&0x80 != 0 }

func (u *uart) read(reg uint16) byte {
	switch reg {
	case 0:
		if u.dlab() {
			return u.dll
		}
		return 0
	case 1:
		if u.dlab() {
			return u.dlm
		}
		return u.ier
	case 2: // IIR: reading acknowledges the pending cause; report FIFOs when enabled
		iir := u.iir
		u.iir = 0x01
		if u.fcr&1 != 0 {
			iir |= 0xc0
		}
		return iir
	case 3:
		return u.lcr
	case 4:
		return u.mcr
	case 5: // LSR: transmitter empty, no data ready
		return 0x60
	case 6: // MSR, with loopback wiring for the driver's self-test
		if u.mcr&0x10 != 0 {
			return (u.mcr&0x0c)<<4 | (u.mcr&0x01)<<5 | (u.mcr&0x02)<<3
		}
		return 0xb0
	default:
		return u.scr
	}
}

func (u *uart) write(reg uint16, val byte) {
	switch reg {
	case 0:
		if u.dlab() {
			u.dll = val
			return
		}
		u.emit(val)
		if u.ier&0x02 != 0 { // the byte is sent at once, so the transmitter is empty again
			u.iir = 0x02
			u.raise()
		}
	case 1:
		if u.dlab() {
			u.dlm = val
			return
		}
		old := u.ier
		u.ier = val & 0x0f
		if old&0x02 == 0 && u.ier&0x02 != 0 {
			u.iir = 0x02
			u.raise()
		}
	case 2:
		u.fcr = val
	case 3:
		u.lcr = val
	case 4:
		u.mcr = val
	case 7:
		u.scr = val
	}
}

func (u *uart) emit(b byte) {
	u.bytes++
	u.out.WriteByte(b)
	if b != '\n' {
		u.line = append(u.line, b)
		return
	}
	u.lines++
	u.out.Flush()
	if u.onLine != nil {
		u.onLine(string(u.line))
	}
	u.line = u.line[:0]
}
