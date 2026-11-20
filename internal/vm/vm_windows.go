//go:build windows

// Package vm runs one Linux guest on the Windows Hypervisor Platform: one
// vCPU, guest RAM at address 0, a serial port for the kernel console, an
// emulated I/O APIC, and virtio-mmio devices.
//
// Register accesses are served on the vCPU thread. Devices may also work from
// goroutines of their own, through virtio.Transport.Do, and raise interrupts
// from there: WHvRequestInterrupt wakes the vCPU even while the hypervisor
// holds it halted, which it does without exiting to us when the guest idles.
package vm

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/whp"
)

const (
	vp0 = 0

	virtioBase = 0xd0000000 // one window per virtio device
	virtioIRQ0 = 5

	// MaxMemory keeps guest RAM below the virtio windows.
	MaxMemory = virtioBase
	// MaxDevices is how many virtio devices fit on the I/O APIC.
	MaxDevices = IOAPICPins - virtioIRQ0
)

// Config describes a guest.
type Config struct {
	Kernel  *boot.Kernel
	Initrd  []byte
	Cmdline string
	Memory  uint64 // guest RAM in bytes

	// Devices are attached in order, each with its own MMIO window and
	// interrupt line, and declared to the guest in that order.
	Devices []virtio.Device

	// ConsoleLine receives the kernel console (COM1) a line at a time.
	ConsoleLine func(line string)
	// Logf receives VMM diagnostics. nil discards them.
	Logf func(format string, a ...any)
}

// drainer is implemented by devices that work outside the vCPU thread.
// Drain is called once the guest has stopped. It waits until the device has
// finished with guest memory, or until ctx is done, and reports whether it
// finished.
type drainer interface {
	Drain(ctx context.Context) bool
}

// Result says how the guest stopped.
type Result struct {
	// Reset is true when the guest asked to be reset, which is how a
	// runwhp guest shuts down.
	Reset  bool
	Reason string
	// Stats is a human-readable summary of exits and device activity.
	Stats []string
}

type machine struct {
	cfg      Config
	part     *whp.Partition
	mem      []byte
	emu      *whp.Emulator
	ioapic   *ioapic
	uart     *uart
	rtcIndex byte
	virtio   []*virtio.Transport

	exits        map[uint32]int
	ioPorts      map[uint16]int
	mmioCount    int
	unknownPages map[uint64]bool
	result       Result
}

func (m *machine) logf(format string, a ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, a...)
	}
}

// Run boots the guest and runs it until it resets, halts for good, faults,
// or ctx is canceled. It occupies the calling goroutine's OS thread.
func Run(ctx context.Context, cfg Config) (Result, error) {
	start := time.Now()
	m := &machine{cfg: cfg, exits: map[uint32]int{}, ioPorts: map[uint16]int{}, unknownPages: map[uint64]bool{}}
	since := func() string { return fmt.Sprintf("%4d ms", time.Since(start).Milliseconds()) }

	if cfg.Memory > MaxMemory || cfg.Memory%(2<<20) != 0 {
		return m.result, fmt.Errorf("guest memory must be a multiple of 2 MiB and at most %d MiB", MaxMemory>>20)
	}
	if cfg.Memory < cfg.Kernel.MinMemory()+uint64(len(cfg.Initrd))+(16<<20) {
		return m.result, fmt.Errorf("%d MiB of guest memory is too little for this kernel and initramfs", cfg.Memory>>20)
	}
	if len(cfg.Devices) > MaxDevices {
		return m.result, fmt.Errorf("%d virtio devices requested; at most %d fit", len(cfg.Devices), MaxDevices)
	}
	if !whp.Present() {
		return m.result, fmt.Errorf("the Windows Hypervisor Platform is not available; enable the \"Windows Hypervisor Platform\" feature")
	}
	var tscHz, apicHz uint64
	whp.Capability(whp.CapProcessorClockFreq, unsafe.Pointer(&tscHz), 8)
	whp.Capability(whp.CapInterruptClockFreq, unsafe.Pointer(&apicHz), 8)

	// The vCPU must run on one OS thread for its whole life.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	part, err := whp.CreatePartition()
	if err != nil {
		return m.result, err
	}
	defer part.Close()
	m.part = part
	if err := m.configure(); err != nil {
		return m.result, err
	}

	host, mem, err := whp.AllocMemory(cfg.Memory)
	if err != nil {
		return m.result, err
	}
	freeMemory := true
	defer func() {
		if freeMemory {
			whp.FreeMemory(host)
		}
	}()
	m.mem = mem
	if err := part.MapGpa(host, 0, cfg.Memory, whp.MapRead|whp.MapWrite|whp.MapExecute); err != nil {
		return m.result, err
	}

	m.ioapic = newIOAPIC(part.RequestInterrupt, m.logf)
	m.uart = newUART(func() { m.ioapic.pulse(uartIRQ) }, cfg.ConsoleLine)
	devices := []boot.Device{{Name: "COM1", HID: "PNP0501", IOBase: uartBase, IOLen: 8, GSI: uartIRQ}}
	for i, dev := range cfg.Devices {
		irq := virtioIRQ0 + i
		m.virtio = append(m.virtio, virtio.NewTransport(dev, m, func() { m.ioapic.pulse(irq) }))
		devices = append(devices, boot.Device{Name: fmt.Sprintf("VR%02X", i), HID: "LNRO0005", UID: i,
			MMIOBase: virtioBase + uint32(i)*virtio.WindowSize, MMIOLen: virtio.WindowSize, GSI: irq})
	}

	layout, err := boot.Load(mem, cfg.Kernel, cfg.Initrd, cfg.Cmdline, devices)
	if err != nil {
		return m.result, err
	}
	if err := part.CreateVP(vp0); err != nil {
		return m.result, err
	}
	defer part.DeleteVP(vp0)
	if err := m.setBootRegisters(layout); err != nil {
		return m.result, err
	}
	if m.emu, err = whp.NewEmulator(part, vp0, m.mmio); err != nil {
		return m.result, err
	}
	defer m.emu.Close()
	m.logf("%s  ready: %d MiB RAM, kernel %s, initramfs %d KiB, %d virtio devices, tsc %d MHz, apic %d MHz",
		since(), cfg.Memory>>20, cfg.Kernel.Version, len(cfg.Initrd)>>10, len(cfg.Devices), tscHz/1e6, apicHz/1e6)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			part.Cancel(vp0)
		case <-done:
		}
	}()

	err = m.loop(ctx)
	m.uart.flush()
	m.logf("%s  stopped: %s", since(), m.result.Reason)
	if !m.drain(ctx) {
		// A device is still reading guest memory, most likely blocked
		// writing to the host. The process is about to exit; leaving the
		// memory allocated is safer than freeing it under the device.
		freeMemory = false
		m.logf("devices still busy; guest memory left allocated")
	} else {
		m.logf("%s  devices drained", since())
	}
	m.result.Stats = m.stats()
	return m.result, err
}

// drain waits for devices working outside the vCPU thread to finish, then
// closes every transport so nothing more reaches guest memory. It reports
// whether the devices finished.
func (m *machine) drain(ctx context.Context) bool {
	idle := true
	for _, t := range m.virtio {
		if d, ok := t.Device().(drainer); ok && !d.Drain(ctx) {
			idle = false
		}
	}
	for _, t := range m.virtio {
		t.Close()
	}
	return idle
}

func (m *machine) configure() error {
	p := m.part
	count := uint32(1)
	if err := p.SetProperty(whp.PropProcessorCount, unsafe.Pointer(&count), 4); err != nil {
		return err
	}
	apicMode := uint32(whp.ApicModeXApic)
	if err := p.SetProperty(whp.PropLocalApicEmulationMode, unsafe.Pointer(&apicMode), 4); err != nil {
		return err
	}
	msrAction := uint32(whp.MsrIgnoreWriteReadZero)
	if err := p.SetProperty(whp.PropUnimplementedMsrAction, unsafe.Pointer(&msrAction), 4); err != nil {
		m.logf("unimplemented-MSR action not set: %v", err)
	}
	// Hyper-V enlightenments give the guest its TSC and APIC timer
	// frequencies. Without them Linux cannot calibrate its clock, since
	// there is no PIT, HPET or ACPI timer.
	// WHV_SYNTHETIC_PROCESSOR_FEATURES_BANKS {BanksCount, Reserved, Bank0}
	var supported, wanted [2]uint64
	if err := whp.Capability(whp.CapSyntheticFeaturesBanks, unsafe.Pointer(&supported), 16); err != nil {
		return err
	}
	wanted[0] = 1
	wanted[1] = supported[1] & (whp.SynHypervisorPresent | whp.SynHv1 | whp.SynVpRunTime | whp.SynPartitionRefCount |
		whp.SynIntrCtrlRegs | whp.SynHypercallRegs | whp.SynVpIndex | whp.SynPartitionRefTsc | whp.SynFrequencyRegs)
	if err := p.SetProperty(whp.PropSyntheticFeaturesBanks, unsafe.Pointer(&wanted), 16); err != nil {
		return err
	}
	return p.Setup()
}

// setBootRegisters sets the vCPU state the 64-bit boot protocol requires.
func (m *machine) setBootRegisters(l boot.Layout) error {
	data := whp.Segment(0, 0xffffffff, boot.SelData, boot.AttrData)
	names := []uint32{whp.RegRip, whp.RegRsi, whp.RegRflags, whp.RegCs, whp.RegDs, whp.RegEs, whp.RegSs, whp.RegFs, whp.RegGs,
		whp.RegGdtr, whp.RegCr3, whp.RegCr4, whp.RegEfer, whp.RegCr0}
	values := []whp.RegValue{
		whp.Reg64(l.Entry), whp.Reg64(boot.ZeroPage), whp.Reg64(2),
		whp.Segment(0, 0xffffffff, boot.SelCode, boot.AttrCode64), data, data, data, data, data,
		whp.Table(boot.GDTAddr, 0x1f),
		whp.Reg64(boot.PML4Addr),
		whp.Reg64(boot.CR4),
		whp.Reg64(boot.EFER),
		whp.Reg64(boot.CR0),
	}
	return m.part.SetRegs(vp0, names, values)
}

func (m *machine) loop(ctx context.Context) error {
	var exit whp.ExitContext
	for {
		if ctx.Err() != nil {
			m.result.Reason = "canceled"
			return nil
		}
		if err := m.part.Run(vp0, &exit); err != nil {
			return err
		}
		reason := exit.Reason()
		m.exits[reason]++
		switch reason {
		case whp.ExitIoPort:
			stop, err := m.handleIO(&exit)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		case whp.ExitHalt:
			if exit.Rflags()&0x200 == 0 {
				m.result.Reason = "guest halted with interrupts disabled"
				return nil
			}
		case whp.ExitCanceled:
			if ctx.Err() != nil {
				m.result.Reason = "canceled"
				return nil
			}
		case whp.ExitUnrecoverableException:
			m.result.Reason = "triple fault"
			m.dump()
			return nil
		case whp.ExitMemoryAccess:
			if err := m.emu.TryMmio(&exit); err != nil {
				m.dump()
				return err
			}
		case whp.ExitApicEoi:
			// Only level-triggered interrupts report EOI; none are used.
		default:
			m.dump()
			return fmt.Errorf("unhandled exit reason %#x at rip %#x", reason, exit.Rip())
		}
	}
}

func (m *machine) handleIO(exit *whp.ExitContext) (stop bool, err error) {
	info := exit.IoInfo()
	port := exit.IoPort()
	size := (info >> 1) & 7
	if info&0x10 != 0 {
		m.dump()
		return false, fmt.Errorf("string I/O on port %#x is not implemented (rip %#x)", port, exit.Rip())
	}
	m.ioPorts[port]++
	rax := exit.IoRax()
	if info&1 != 0 { // OUT
		stop = m.portOut(port, uint32(rax))
	} else { // IN
		val := uint64(m.portIn(port))
		switch size {
		case 1:
			rax = rax&^0xff | val&0xff
		case 2:
			rax = rax&^0xffff | val&0xffff
		default:
			rax = val & 0xffffffff
		}
	}
	err = m.part.SetRegs(vp0, []uint32{whp.RegRip, whp.RegRax}, []whp.RegValue{whp.Reg64(exit.Rip() + exit.InstrLen()), whp.Reg64(rax)})
	return stop, err
}

func (m *machine) portIn(port uint16) uint32 {
	switch {
	case port >= uartBase && port < uartBase+8:
		return uint32(m.uart.read(port - uartBase))
	case port == 0x71:
		return uint32(m.rtcRead())
	}
	return 0xffffffff // nothing there: PIC, PIT, PCI and i8042 all read as absent
}

func (m *machine) portOut(port uint16, val uint32) (stop bool) {
	switch {
	case port >= uartBase && port < uartBase+8:
		m.uart.write(port-uartBase, byte(val))
	case port == 0x70:
		m.rtcIndex = byte(val) & 0x7f
	case port == boot.ResetPort && byte(val) == boot.ResetValue:
		// The FADT reset register, which is also the i8042 reset pulse.
		m.result.Reset = true
		m.result.Reason = "guest requested reset"
		return true
	}
	return false
}

// rtcRead answers CMOS clock reads. Linux 6.6 reads the RTC even when the
// FADT says there is none, and spends over a second retrying if it is absent.
func (m *machine) rtcRead() byte {
	bcd := func(n int) byte { return byte(n/10<<4 | n%10) }
	now := time.Now().UTC()
	switch m.rtcIndex {
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

// Slice implements virtio.Memory over guest RAM.
func (m *machine) Slice(addr, n uint64) ([]byte, bool) {
	if addr+n < addr || addr+n > uint64(len(m.mem)) {
		return nil, false
	}
	return m.mem[addr : addr+n], true
}

// mmio serves one memory-mapped access decoded by the instruction emulator.
func (m *machine) mmio(gpa uint64, data []byte, write bool) {
	m.mmioCount++
	var val uint64
	if write {
		for i, b := range data {
			val |= uint64(b) << (8 * i)
		}
	}
	switch {
	case gpa >= boot.IOAPICBase && gpa < boot.IOAPICBase+0x1000:
		if write {
			m.ioapic.write(gpa-boot.IOAPICBase, uint32(val))
		} else {
			val = uint64(m.ioapic.read(gpa - boot.IOAPICBase))
		}
	case gpa >= virtioBase && (gpa-virtioBase)/virtio.WindowSize < uint64(len(m.virtio)):
		t := m.virtio[(gpa-virtioBase)/virtio.WindowSize]
		off := (gpa - virtioBase) % virtio.WindowSize
		if write {
			t.Write(off, len(data), val)
		} else {
			val = t.Read(off, len(data))
		}
	default:
		if page := gpa &^ 0xfff; !m.unknownPages[page] {
			m.unknownPages[page] = true
			m.logf("access to unbacked MMIO page %#x ignored", page)
		}
		val = ^uint64(0)
	}
	if !write {
		for i := range data {
			data[i] = byte(val >> (8 * i))
		}
	}
}

func (m *machine) dump() {
	names := []uint32{whp.RegRip, whp.RegRsp, whp.RegRflags, whp.RegCr0, whp.RegCr2, whp.RegCr3, whp.RegCr4, whp.RegEfer}
	values := make([]whp.RegValue, len(names))
	if err := m.part.GetRegs(vp0, names, values); err != nil {
		m.logf("register dump failed: %v", err)
		return
	}
	m.logf("rip=%#x rsp=%#x rflags=%#x cr0=%#x cr2=%#x cr3=%#x cr4=%#x efer=%#x",
		values[0][0], values[1][0], values[2][0], values[3][0], values[4][0], values[5][0], values[6][0], values[7][0])
}

func (m *machine) stats() []string {
	names := map[uint32]string{whp.ExitIoPort: "io", whp.ExitHalt: "halt", whp.ExitCanceled: "canceled",
		whp.ExitMemoryAccess: "mmio", whp.ExitUnrecoverableException: "triple-fault", whp.ExitApicEoi: "eoi"}
	var parts []string
	for reason, n := range m.exits {
		name := names[reason]
		if name == "" {
			name = fmt.Sprintf("%#x", reason)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	sort.Strings(parts)
	out := []string{"exits: " + strings.Join(parts, " ")}

	type portCount struct {
		port uint16
		n    int
	}
	var ports []portCount
	for p, n := range m.ioPorts {
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
	out = append(out, "busiest ports: "+strings.Join(parts, " "))
	if m.uart != nil {
		out = append(out, fmt.Sprintf("console: %d lines, %d bytes", m.uart.lines, m.uart.bytes))
	}
	if m.ioapic != nil {
		out = append(out, fmt.Sprintf("mmio accesses: %d, interrupts delivered through the I/O APIC: %d", m.mmioCount, m.ioapic.deliveredCount()))
	}
	for i, t := range m.virtio {
		switch d := t.Device().(type) {
		case *virtio.Blk:
			out = append(out, fmt.Sprintf("virtio %d: blk %q, %d requests, %d MiB read, %d MiB written",
				i, d.Serial, d.Requests, d.BytesRead>>20, d.BytesWritten>>20))
		case *virtio.Console:
			out = append(out, fmt.Sprintf("virtio %d: console, %d bytes from the guest, %d to it", i, d.BytesWritten.Load(), d.BytesRead.Load()))
		case *virtio.Net:
			out = append(out, fmt.Sprintf("virtio %d: net, %d frames (%d KiB) from the guest, %d frames (%d KiB) to it, %d dropped",
				i, d.TxFrames.Load(), d.TxBytes.Load()>>10, d.RxFrames.Load(), d.RxBytes.Load()>>10, d.RxDropped.Load()))
		}
	}
	return out
}
