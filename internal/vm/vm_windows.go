//go:build windows

// Package vm runs one Linux guest on the Windows Hypervisor Platform: one or
// more vCPUs, guest RAM from address 0 up to the device windows and on from
// 4 GiB, a serial port for the kernel console, an emulated I/O APIC, and
// virtio-mmio devices.
//
// Each vCPU runs on an OS thread of its own and serves the register accesses
// it makes on that thread. The boot processor starts the others itself,
// through the hypervisor's local APIC emulation, which also carries their
// interprocessor interrupts. Devices may also work from goroutines of their
// own, through virtio.Transport.Do, and raise interrupts from there:
// WHvRequestInterrupt wakes a vCPU even while the hypervisor holds it halted,
// which it does without exiting to us when the guest idles.
package vm

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/whp"
)

const (
	virtioBase = LowMemoryEnd // one window per virtio device
	virtioIRQ0 = 5

	// MaxDevices is how many virtio devices fit on the I/O APIC.
	MaxDevices = IOAPICPins - virtioIRQ0
	// MaxCPUs is the most vCPUs a guest can have: runwhp's kernel is built
	// for at most 64.
	MaxCPUs = 64
)

// MaxMemory is the most guest RAM runwhp gives a guest: this machine's
// physical memory, in whole 2 MiB pages.
func MaxMemory() uint64 {
	phys, err := whp.PhysicalMemory()
	if err != nil {
		return LowMemoryEnd
	}
	return phys &^ (2<<20 - 1)
}

// Config describes a guest.
type Config struct {
	Kernel  *boot.Kernel
	Initrd  []byte
	Cmdline string
	Memory  uint64 // guest RAM in bytes
	// CPUs is the number of vCPUs, at most MaxCPUs. Zero means one.
	CPUs int

	// Devices are attached in order, each with its own MMIO window and
	// interrupt line, and declared to the guest in that order.
	Devices []virtio.Device

	// ConsoleLine receives the kernel console (COM1) a line at a time.
	ConsoleLine func(line string)
	// Logf receives VMM diagnostics. nil discards them.
	Logf func(format string, a ...any)
}

// drainer is implemented by devices that work outside the vCPU threads.
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
	cfg    Config
	part   *whp.Partition
	mem    *guestMemory
	ioapic *ioapic
	virtio []*virtio.Transport
	cpus   []*vcpu
	stop   *stopper

	// portMu serializes port I/O from all vCPUs: the serial port, the CMOS
	// clock's index register and the reset register.
	portMu   sync.Mutex
	uart     *uart
	rtcIndex byte
}

// vcpu is one virtual processor. Its counters belong to the thread that runs
// it until its loop has returned.
type vcpu struct {
	m     *machine
	index uint32
	emu   *whp.Emulator

	exits        map[uint32]int
	ioPorts      map[uint16]int
	mmioCount    int
	unknownPages map[uint64]bool
}

// on names the vCPU in messages when there is more than one.
func (v *vcpu) on() string {
	if len(v.m.cpus) == 1 {
		return ""
	}
	return fmt.Sprintf(" on CPU %d", v.index)
}

func (m *machine) logf(format string, a ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, a...)
	}
}

// Run boots the guest and runs it until it resets, halts for good, faults,
// or ctx is canceled. The boot processor occupies the calling goroutine's OS
// thread; the others get threads of their own.
func Run(ctx context.Context, cfg Config) (Result, error) {
	start := time.Now()
	var result Result
	since := func() string { return fmt.Sprintf("%4d ms", time.Since(start).Milliseconds()) }

	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	m := &machine{cfg: cfg}
	if cfg.CPUs < 0 || cfg.CPUs > MaxCPUs {
		return result, fmt.Errorf("%d vCPUs requested; between 1 and %d are possible", cfg.CPUs, MaxCPUs)
	}
	if maxMem := MaxMemory(); cfg.Memory > maxMem || cfg.Memory%(2<<20) != 0 {
		return result, fmt.Errorf("guest memory must be a multiple of 2 MiB and at most %d MiB", maxMem>>20)
	}
	if cfg.Memory < cfg.Kernel.MinMemory()+uint64(len(cfg.Initrd))+(16<<20) {
		return result, fmt.Errorf("%d MiB of guest memory is too little for this kernel and initramfs", cfg.Memory>>20)
	}
	if len(cfg.Devices) > MaxDevices {
		return result, fmt.Errorf("%d virtio devices requested; at most %d fit", len(cfg.Devices), MaxDevices)
	}
	if !whp.Present() {
		return result, fmt.Errorf("the Windows Hypervisor Platform is not available; enable the \"Windows Hypervisor Platform\" feature")
	}
	var tscHz, apicHz uint64
	whp.Capability(whp.CapProcessorClockFreq, unsafe.Pointer(&tscHz), 8)
	whp.Capability(whp.CapInterruptClockFreq, unsafe.Pointer(&apicHz), 8)

	// The boot processor must run on one OS thread for its whole life.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	part, err := whp.CreatePartition()
	if err != nil {
		return result, err
	}
	defer part.Close()
	m.part = part
	if err := m.configure(); err != nil {
		return result, err
	}

	low, high := splitMemory(cfg.Memory)
	span := low
	if high > 0 {
		span = boot.HighMemoryBase + high
	}
	host, mem, err := whp.ReserveMemory(span)
	if err != nil {
		return result, err
	}
	freeMemory := true
	defer func() {
		if freeMemory {
			whp.FreeMemory(host)
		}
	}()
	m.mem = &guestMemory{host: mem, low: low, high: high}
	for _, r := range [][2]uint64{{0, low}, {boot.HighMemoryBase, high}} {
		if r[1] == 0 {
			continue
		}
		if err := whp.CommitMemory(host+uintptr(r[0]), r[1]); err != nil {
			return result, err
		}
		if err := part.MapGpa(host+uintptr(r[0]), r[0], r[1], whp.MapRead|whp.MapWrite|whp.MapExecute); err != nil {
			return result, err
		}
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

	for i := range cfg.CPUs {
		if err := part.CreateVP(uint32(i)); err != nil {
			return result, err
		}
		defer part.DeleteVP(uint32(i))
		m.cpus = append(m.cpus, &vcpu{m: m, index: uint32(i),
			exits: map[uint32]int{}, ioPorts: map[uint16]int{}, unknownPages: map[uint64]bool{}})
	}
	apicIDs, err := m.prepareCPUs()
	if err != nil {
		return result, err
	}
	layout, err := boot.Load(m.mem, low, high, cfg.Kernel, cfg.Initrd, cfg.Cmdline, devices, apicIDs)
	if err != nil {
		return result, err
	}
	if err := m.setBootRegisters(layout); err != nil {
		return result, err
	}
	for _, v := range m.cpus {
		if v.emu, err = whp.NewEmulator(part, v.index, v.mmio); err != nil {
			return result, err
		}
		defer v.emu.Close()
	}
	m.logf("%s  ready: %d MiB RAM, %d vCPUs, kernel %s, initramfs %d KiB, %d virtio devices, tsc %d MHz, apic %d MHz",
		since(), cfg.Memory>>20, cfg.CPUs, cfg.Kernel.Version, len(cfg.Initrd)>>10, len(cfg.Devices), tscHz/1e6, apicHz/1e6)

	m.stop = newStopper(cfg.CPUs)
	var loops sync.WaitGroup
	loops.Add(len(m.cpus))
	for _, v := range m.cpus[1:] {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			defer loops.Done()
			v.loop()
		}()
	}
	stopped := make(chan struct{})
	go func() {
		loops.Wait()
		close(stopped)
	}()
	go m.cancelOnStop(ctx, stopped)
	m.cpus[0].loop()
	loops.Done()
	<-stopped

	result.Reason, result.Reset, err = m.stop.outcome()
	m.portMu.Lock()
	m.uart.flush()
	m.portMu.Unlock()
	m.logf("%s  stopped: %s", since(), result.Reason)
	if !m.drain(ctx) {
		// A device is still reading guest memory, most likely blocked
		// writing to the host. The process is about to exit; leaving the
		// memory allocated is safer than freeing it under the device.
		freeMemory = false
		m.logf("devices still busy; guest memory left allocated")
	} else {
		m.logf("%s  devices drained", since())
	}
	result.Stats = m.stats()
	return result, err
}

// cancelOnStop waits until the guest is to stop, because a vCPU stopped it
// or ctx was canceled, and then makes every vCPU's Run return until all
// their loops have, which stopped reports. It keeps canceling, because a
// vCPU between two Run calls may miss a cancel and sleep in the next one.
func (m *machine) cancelOnStop(ctx context.Context, stopped <-chan struct{}) {
	select {
	case <-ctx.Done():
		m.stop.stop("canceled", false, nil)
	case <-m.stop.Done():
	case <-stopped:
		return
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, v := range m.cpus {
			m.part.Cancel(v.index)
		}
		select {
		case <-stopped:
			return
		case <-tick.C:
		}
	}
}

// drain waits for devices working outside the vCPU threads to finish, then
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
	count := uint32(m.cfg.CPUs)
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

// setBootRegisters sets the boot processor's state as the 64-bit boot
// protocol requires.
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
	return m.part.SetRegs(0, names, values)
}

// prepareCPUs describes the vCPUs' topology in CPUID, holds every
// application processor until the boot processor starts it with INIT and
// SIPI, and returns each vCPU's local APIC id for the MADT. WHP takes CPUID
// results for a VP only once it exists, so this runs after CreateVP.
func (m *machine) prepareCPUs() ([]uint32, error) {
	var topology []whp.CpuidResult2
	for _, r := range cpuidTopology(len(m.cpus)) {
		topology = append(topology, whp.CpuidResult2(r))
	}
	if err := m.part.SetCpuidResults(topology); err != nil {
		return nil, fmt.Errorf("describing the processor topology: %w", err)
	}
	var ids []uint32
	for _, v := range m.cpus {
		if v.index != 0 {
			err := m.part.SetRegs(v.index, []uint32{whp.RegInternalActivityState},
				[]whp.RegValue{whp.Reg64(whp.ActivityStartupSuspend)})
			if err != nil {
				return nil, fmt.Errorf("holding CPU %d until the guest starts it: %w", v.index, err)
			}
		}
		values := make([]whp.RegValue, 1)
		if err := m.part.GetRegs(v.index, []uint32{whp.RegApicId}, values); err != nil {
			return nil, fmt.Errorf("reading CPU %d's APIC id: %w", v.index, err)
		}
		if values[0][0] != uint64(v.index) {
			// cpuidTopology numbers the cores by VP index.
			return nil, fmt.Errorf("CPU %d has APIC id %d; runwhp expects APIC ids to follow processor numbers", v.index, values[0][0])
		}
		ids = append(ids, uint32(values[0][0]))
	}
	return ids, nil
}

// loop runs the vCPU until the guest is to stop.
func (v *vcpu) loop() {
	m := v.m
	var exit whp.ExitContext
	for !m.stop.isStopped() {
		if err := m.part.Run(v.index, &exit); err != nil {
			m.stop.stop("", false, fmt.Errorf("%w%s", err, v.on()))
			return
		}
		reason := exit.Reason()
		v.exits[reason]++
		switch reason {
		case whp.ExitIoPort:
			if err := v.handleIO(&exit); err != nil {
				m.stop.stop("", false, err)
			}
		case whp.ExitHalt:
			if exit.Rflags()&0x200 == 0 {
				m.logf("CPU %d halted with interrupts disabled at rip %#x", v.index, exit.Rip())
				m.stop.park(int(v.index))
			}
		case whp.ExitCanceled:
		case whp.ExitUnrecoverableException:
			v.dump()
			m.stop.stop("triple fault"+v.on(), false, nil)
		case whp.ExitMemoryAccess:
			if err := v.emu.TryMmio(&exit); err != nil {
				v.dump()
				m.stop.stop("", false, fmt.Errorf("%w%s", err, v.on()))
			}
		case whp.ExitApicEoi:
			// Only level-triggered interrupts report EOI; none are used.
		default:
			v.dump()
			m.stop.stop("", false, fmt.Errorf("unhandled exit reason %#x at rip %#x%s", reason, exit.Rip(), v.on()))
		}
	}
}

func (v *vcpu) handleIO(exit *whp.ExitContext) error {
	m := v.m
	info := exit.IoInfo()
	port := exit.IoPort()
	size := (info >> 1) & 7
	if info&0x10 != 0 {
		v.dump()
		return fmt.Errorf("string I/O on port %#x is not implemented (rip %#x%s)", port, exit.Rip(), v.on())
	}
	v.ioPorts[port]++
	rax := exit.IoRax()
	m.portMu.Lock()
	if info&1 != 0 { // OUT
		m.portOut(port, uint32(rax))
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
	m.portMu.Unlock()
	return m.part.SetRegs(v.index, []uint32{whp.RegRip, whp.RegRax}, []whp.RegValue{whp.Reg64(exit.Rip() + exit.InstrLen()), whp.Reg64(rax)})
}

// portIn and portOut serve port I/O with portMu held.
func (m *machine) portIn(port uint16) uint32 {
	switch {
	case port >= uartBase && port < uartBase+8:
		return uint32(m.uart.read(port - uartBase))
	case port == 0x71:
		return uint32(m.rtcRead())
	}
	return 0xffffffff // nothing there: PIC, PIT, PCI and i8042 all read as absent
}

func (m *machine) portOut(port uint16, val uint32) {
	switch {
	case port >= uartBase && port < uartBase+8:
		m.uart.write(port-uartBase, byte(val))
	case port == 0x70:
		m.rtcIndex = byte(val) & 0x7f
	case port == boot.ResetPort && byte(val) == boot.ResetValue:
		// The FADT reset register, which is also the i8042 reset pulse.
		m.stop.stop("guest requested reset", true, nil)
	}
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
func (m *machine) Slice(addr, n uint64) ([]byte, bool) { return m.mem.Slice(addr, n) }

// mmio serves one memory-mapped access decoded by the instruction emulator.
// The I/O APIC and each transport take their own locks.
func (v *vcpu) mmio(gpa uint64, data []byte, write bool) {
	m := v.m
	v.mmioCount++
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
	case m.mem.Contains(gpa, uint64(len(data))):
		// RAM the emulator reaches through us, such as the memory operand
		// of an instruction that also touches a device.
		b, _ := m.mem.Slice(gpa, uint64(len(data)))
		if write {
			copy(b, data)
		} else {
			copy(data, b)
		}
		return
	default:
		if page := gpa &^ 0xfff; !v.unknownPages[page] {
			v.unknownPages[page] = true
			m.logf("access to unbacked MMIO page %#x ignored%s", page, v.on())
		}
		val = ^uint64(0)
	}
	if !write {
		for i := range data {
			data[i] = byte(val >> (8 * i))
		}
	}
}

func (v *vcpu) dump() {
	names := []uint32{whp.RegRip, whp.RegRsp, whp.RegRflags, whp.RegCr0, whp.RegCr2, whp.RegCr3, whp.RegCr4, whp.RegEfer}
	values := make([]whp.RegValue, len(names))
	if err := v.m.part.GetRegs(v.index, names, values); err != nil {
		v.m.logf("register dump failed%s: %v", v.on(), err)
		return
	}
	v.m.logf("rip=%#x rsp=%#x rflags=%#x cr0=%#x cr2=%#x cr3=%#x cr4=%#x efer=%#x%s",
		values[0][0], values[1][0], values[2][0], values[3][0], values[4][0], values[5][0], values[6][0], values[7][0], v.on())
}

func (m *machine) stats() []string {
	names := map[uint32]string{whp.ExitIoPort: "io", whp.ExitHalt: "halt", whp.ExitCanceled: "canceled",
		whp.ExitMemoryAccess: "mmio", whp.ExitUnrecoverableException: "triple-fault", whp.ExitApicEoi: "eoi"}
	exits, ioPorts, mmioCount := map[uint32]int{}, map[uint16]int{}, 0
	var perCPU []string
	for _, v := range m.cpus {
		total := 0
		for reason, n := range v.exits {
			exits[reason] += n
			total += n
		}
		for port, n := range v.ioPorts {
			ioPorts[port] += n
		}
		mmioCount += v.mmioCount
		perCPU = append(perCPU, fmt.Sprintf("%d=%d", v.index, total))
	}
	var parts []string
	for reason, n := range exits {
		name := names[reason]
		if name == "" {
			name = fmt.Sprintf("%#x", reason)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	sort.Strings(parts)
	out := []string{"exits: " + strings.Join(parts, " ")}
	if len(m.cpus) > 1 {
		out = append(out, "exits per CPU: "+strings.Join(perCPU, " "))
	}

	type portCount struct {
		port uint16
		n    int
	}
	var ports []portCount
	for p, n := range ioPorts {
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
		out = append(out, fmt.Sprintf("mmio accesses: %d, interrupts delivered through the I/O APIC: %d", mmioCount, m.ioapic.deliveredCount()))
	}
	for i, t := range m.virtio {
		switch d := t.Device().(type) {
		case *virtio.Blk:
			out = append(out, fmt.Sprintf("virtio %d: blk %q, %d requests, %d MiB read, %d MiB written, %d flushes, %d discards, at most %d at once",
				i, d.Serial, d.Requests.Load(), d.BytesRead.Load()>>20, d.BytesWritten.Load()>>20, d.Flushes.Load(), d.Discards.Load(), d.MaxInflight.Load()))
		case *virtio.Console:
			out = append(out, fmt.Sprintf("virtio %d: console, %d bytes from the guest, %d to it", i, d.BytesWritten.Load(), d.BytesRead.Load()))
		case *virtio.Net:
			out = append(out, fmt.Sprintf("virtio %d: net, %d frames (%d KiB) from the guest, %d frames (%d KiB) to it, %d dropped",
				i, d.TxFrames.Load(), d.TxBytes.Load()>>10, d.RxFrames.Load(), d.RxBytes.Load()>>10, d.RxDropped.Load()))
		case *virtio.Fs:
			out = append(out, fmt.Sprintf("virtio %d: fs %q, %d requests, %d KiB from the guest, %d KiB to it",
				i, d.Tag, d.Requests.Load(), d.BytesIn.Load()>>10, d.BytesOut.Load()>>10))
		}
	}
	return out
}
