//go:build windows

package main

// Minimal bindings for the Windows Hypervisor Platform (WinHvPlatform.dll).
// Layouts and constants follow WinHvPlatformDefs.h from Windows SDK 10.0.26100.

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	whpDLL = syscall.NewLazyDLL("WinHvPlatform.dll")

	procGetCapability        = whpDLL.NewProc("WHvGetCapability")
	procCreatePartition      = whpDLL.NewProc("WHvCreatePartition")
	procSetupPartition       = whpDLL.NewProc("WHvSetupPartition")
	procDeletePartition      = whpDLL.NewProc("WHvDeletePartition")
	procSetPartitionProperty = whpDLL.NewProc("WHvSetPartitionProperty")
	procMapGpaRange          = whpDLL.NewProc("WHvMapGpaRange")
	procCreateVP             = whpDLL.NewProc("WHvCreateVirtualProcessor")
	procDeleteVP             = whpDLL.NewProc("WHvDeleteVirtualProcessor")
	procRunVP                = whpDLL.NewProc("WHvRunVirtualProcessor")
	procCancelRunVP          = whpDLL.NewProc("WHvCancelRunVirtualProcessor")
	procGetVPRegisters       = whpDLL.NewProc("WHvGetVirtualProcessorRegisters")
	procSetVPRegisters       = whpDLL.NewProc("WHvSetVirtualProcessorRegisters")
	procRequestInterrupt     = whpDLL.NewProc("WHvRequestInterrupt")
	procTranslateGva         = whpDLL.NewProc("WHvTranslateGva")

	emuDLL         = syscall.NewLazyDLL("WinHvEmulation.dll")
	procEmuCreate  = emuDLL.NewProc("WHvEmulatorCreateEmulator")
	procEmuTryMmio = emuDLL.NewProc("WHvEmulatorTryMmioEmulation")

	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc = kernel32.NewProc("VirtualAlloc")
)

// WHV_CAPABILITY_CODE
const (
	capHypervisorPresent      = 0x0000
	capProcessorClockFreq     = 0x1004
	capInterruptClockFreq     = 0x1005
	capSyntheticFeaturesBanks = 0x1008
)

// WHV_PARTITION_PROPERTY_CODE
const (
	propLocalApicEmulationMode = 0x1005
	propSyntheticFeaturesBanks = 0x100C
	propUnimplementedMsrAction = 0x1010
	propProcessorCount         = 0x1fff
)

const (
	apicModeXApic            = 1
	msrIgnoreWriteReadZero   = 1
	mapRead, mapWrite, mapEx = 1, 2, 4
)

// WHV_SYNTHETIC_PROCESSOR_FEATURES bits (bank 0).
const (
	synHypervisorPresent = 1 << 0
	synHv1               = 1 << 1
	synVpRunTime         = 1 << 2
	synPartitionRefCount = 1 << 3
	synIntrCtrlRegs      = 1 << 6
	synHypercallRegs     = 1 << 7
	synVpIndex           = 1 << 8
	synPartitionRefTsc   = 1 << 9
	synFrequencyRegs     = 1 << 11
)

// WHV_RUN_VP_EXIT_REASON
const (
	exitNone                   = 0x0000
	exitMemoryAccess           = 0x0001
	exitIoPort                 = 0x0002
	exitUnrecoverableException = 0x0004
	exitInvalidVpRegister      = 0x0005
	exitUnsupportedFeature     = 0x0006
	exitHalt                   = 0x0008
	exitApicEoi                = 0x0009
	exitCanceled               = 0x2001
)

// WHV_REGISTER_NAME
const (
	regRax    = 0x00
	regRsp    = 0x04
	regRsi    = 0x06
	regRip    = 0x10
	regRflags = 0x11
	regEs     = 0x12
	regCs     = 0x13
	regSs     = 0x14
	regDs     = 0x15
	regFs     = 0x16
	regGs     = 0x17
	regGdtr   = 0x1B
	regCr0    = 0x1C
	regCr2    = 0x1D
	regCr3    = 0x1E
	regCr4    = 0x1F
	regEfer   = 0x2001
)

// regValue is WHV_REGISTER_VALUE (a 16-byte union).
type regValue [2]uint64

func reg64(v uint64) regValue { return regValue{v, 0} }

// segment encodes WHV_X64_SEGMENT_REGISTER {Base, Limit, Selector, Attributes}.
func segment(base uint64, limit uint32, selector, attributes uint16) regValue {
	return regValue{base, uint64(limit) | uint64(selector)<<32 | uint64(attributes)<<48}
}

// table encodes WHV_X64_TABLE_REGISTER {Pad[3], Limit, Base}.
func table(base uint64, limit uint16) regValue {
	return regValue{uint64(limit) << 48, base}
}

// exitContext is WHV_RUN_VP_EXIT_CONTEXT (224 bytes on amd64).
type exitContext [28]uint64

func (c *exitContext) bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(c)), int(unsafe.Sizeof(*c)))
}
func (c *exitContext) reason() uint32   { return binary.LittleEndian.Uint32(c.bytes()[0:]) }
func (c *exitContext) instrLen() uint64 { return uint64(c.bytes()[10] & 0x0f) }
func (c *exitContext) rip() uint64      { return c[4] }
func (c *exitContext) rflags() uint64   { return c[5] }

// I/O port access context (union starts at byte 48).
func (c *exitContext) ioInfo() uint32 { return binary.LittleEndian.Uint32(c.bytes()[68:]) }
func (c *exitContext) ioPort() uint16 { return binary.LittleEndian.Uint16(c.bytes()[72:]) }
func (c *exitContext) ioRax() uint64  { return c[10] }

// Memory access context.
func (c *exitContext) memInfo() uint32 { return binary.LittleEndian.Uint32(c.bytes()[68:]) }
func (c *exitContext) memGpa() uint64  { return c[9] }

func check(name string, r uintptr) error {
	if hr := uint32(r); hr&0x80000000 != 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", name, hr)
	}
	return nil
}

type partition struct{ h uintptr }

func capability(code uint32, buf unsafe.Pointer, size uint32) error {
	var written uint32
	r, _, _ := procGetCapability.Call(uintptr(code), uintptr(buf), uintptr(size), uintptr(unsafe.Pointer(&written)))
	return check("WHvGetCapability", r)
}

func createPartition() (*partition, error) {
	var h uintptr
	r, _, _ := procCreatePartition.Call(uintptr(unsafe.Pointer(&h)))
	if err := check("WHvCreatePartition", r); err != nil {
		return nil, err
	}
	return &partition{h}, nil
}

func (p *partition) setProperty(code uint32, buf unsafe.Pointer, size uint32) error {
	r, _, _ := procSetPartitionProperty.Call(p.h, uintptr(code), uintptr(buf), uintptr(size))
	return check(fmt.Sprintf("WHvSetPartitionProperty(0x%x)", code), r)
}

func (p *partition) setup() error {
	r, _, _ := procSetupPartition.Call(p.h)
	return check("WHvSetupPartition", r)
}

func (p *partition) close() { procDeletePartition.Call(p.h) }

func (p *partition) mapGpa(host uintptr, gpa, size uint64, flags uint32) error {
	r, _, _ := procMapGpaRange.Call(p.h, host, uintptr(gpa), uintptr(size), uintptr(flags))
	return check("WHvMapGpaRange", r)
}

func (p *partition) createVP(index uint32) error {
	r, _, _ := procCreateVP.Call(p.h, uintptr(index), 0)
	return check("WHvCreateVirtualProcessor", r)
}

func (p *partition) deleteVP(index uint32) { procDeleteVP.Call(p.h, uintptr(index)) }

func (p *partition) run(index uint32, ctx *exitContext) error {
	r, _, _ := procRunVP.Call(p.h, uintptr(index), uintptr(unsafe.Pointer(ctx)), unsafe.Sizeof(*ctx))
	return check("WHvRunVirtualProcessor", r)
}

func (p *partition) cancel(index uint32) { procCancelRunVP.Call(p.h, uintptr(index), 0) }

func (p *partition) setRegs(index uint32, names []uint32, values []regValue) error {
	r, _, _ := procSetVPRegisters.Call(p.h, uintptr(index),
		uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&values[0])))
	return check("WHvSetVirtualProcessorRegisters", r)
}

func (p *partition) getRegs(index uint32, names []uint32, values []regValue) error {
	r, _, _ := procGetVPRegisters.Call(p.h, uintptr(index),
		uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&values[0])))
	return check("WHvGetVirtualProcessorRegisters", r)
}

// allocGuestMemory commits zeroed, page-aligned memory to back guest RAM.
func allocGuestMemory(size uint64) (uintptr, []byte, error) {
	const memCommit, memReserve, pageReadWrite = 0x1000, 0x2000, 0x04
	addr, _, err := procVirtualAlloc.Call(0, uintptr(size), memCommit|memReserve, pageReadWrite)
	if addr == 0 {
		return 0, nil, fmt.Errorf("VirtualAlloc(%d MiB): %w", size>>20, err)
	}
	// Convert through a pointer-to-pointer so the uintptr never looks like
	// arithmetic on a Go pointer; the memory is outside the Go heap.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return addr, unsafe.Slice((*byte)(ptr), int(size)), nil
}

// requestInterrupt asks the hypervisor's local APIC emulation to deliver an
// edge-triggered interrupt. It is safe to call from any thread.
func (p *partition) requestInterrupt(deliveryMode, destMode, destination, vector uint32) error {
	// WHV_INTERRUPT_CONTROL {Type:8, DestinationMode:4, TriggerMode:4, ...; Destination; Vector}
	ctl := struct {
		flags       uint64
		destination uint32
		vector      uint32
	}{uint64(deliveryMode) | uint64(destMode)<<8, destination, vector}
	r, _, _ := procRequestInterrupt.Call(p.h, uintptr(unsafe.Pointer(&ctl)), unsafe.Sizeof(ctl))
	return check("WHvRequestInterrupt", r)
}

// ptr converts an address handed to us by Windows into a pointer without
// tripping vet's unsafe.Pointer arithmetic check.
func ptr(addr uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&addr)) }
