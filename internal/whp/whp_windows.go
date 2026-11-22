//go:build windows

// Package whp binds the parts of the Windows Hypervisor Platform
// (WinHvPlatform.dll) and its instruction emulator (WinHvEmulation.dll) that
// runwhp uses. Layouts and constants follow WinHvPlatformDefs.h and
// WinHvEmulation.h from Windows SDK 10.0.26100.
package whp

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"
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
	procUnmapGpaRange        = whpDLL.NewProc("WHvUnmapGpaRange")
	procAdviseGpaRange       = whpDLL.NewProc("WHvAdviseGpaRange")
	procGetVPCounters        = whpDLL.NewProc("WHvGetVirtualProcessorCounters")
	procCreateVP             = whpDLL.NewProc("WHvCreateVirtualProcessor")
	procDeleteVP             = whpDLL.NewProc("WHvDeleteVirtualProcessor")
	procRunVP                = whpDLL.NewProc("WHvRunVirtualProcessor")
	procCancelRunVP          = whpDLL.NewProc("WHvCancelRunVirtualProcessor")
	procGetVPRegisters       = whpDLL.NewProc("WHvGetVirtualProcessorRegisters")
	procSetVPRegisters       = whpDLL.NewProc("WHvSetVirtualProcessorRegisters")
	procRequestInterrupt     = whpDLL.NewProc("WHvRequestInterrupt")
	procTranslateGva         = whpDLL.NewProc("WHvTranslateGva")

	// x/sys/windows has none of GlobalMemoryStatusEx,
	// K32GetProcessMemoryInfo and DiscardVirtualMemory.
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc         = kernel32.NewProc("VirtualAlloc")
	procVirtualFree          = kernel32.NewProc("VirtualFree")
	procDiscardVirtualMemory = kernel32.NewProc("DiscardVirtualMemory")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

// WHV_CAPABILITY_CODE
const (
	CapHypervisorPresent      = 0x0000
	CapProcessorClockFreq     = 0x1004
	CapInterruptClockFreq     = 0x1005
	CapSyntheticFeaturesBanks = 0x1008
)

// WHV_PARTITION_PROPERTY_CODE
const (
	PropLocalApicEmulationMode = 0x1005
	PropSyntheticFeaturesBanks = 0x100C
	PropCpuidResultList2       = 0x100D
	PropUnimplementedMsrAction = 0x1010
	PropProcessorCount         = 0x1fff
)

// CpuidResult2 is WHV_X64_CPUID_RESULT2: where a bit of Mask is set, the
// guest reads that bit of Output from CPUID Function (and subleaf Index, with
// CpuidSubleafSpecific; on VpIndex only, with CpuidVpSpecific).
type CpuidResult2 struct {
	Function, Index, VpIndex, Flags uint32
	Output, Mask                    [4]uint32 // eax, ebx, ecx, edx
}

const (
	CpuidSubleafSpecific = 1
	CpuidVpSpecific      = 2
)

// SetCpuidResults overrides CPUID results. An entry for one VP is refused,
// with HV_STATUS_INVALID_VP_INDEX from Setup, until that VP exists, so call
// it once the VPs are created.
func (p *Partition) SetCpuidResults(r []CpuidResult2) error {
	if len(r) == 0 {
		return nil
	}
	return p.SetProperty(PropCpuidResultList2, unsafe.Pointer(&r[0]), uint32(len(r))*uint32(unsafe.Sizeof(r[0])))
}

const (
	ApicModeXApic          = 1
	MsrIgnoreWriteReadZero = 1

	MapRead    = 1
	MapWrite   = 2
	MapExecute = 4
)

// WHV_SYNTHETIC_PROCESSOR_FEATURES bits (bank 0).
const (
	SynHypervisorPresent = 1 << 0
	SynHv1               = 1 << 1
	SynVpRunTime         = 1 << 2
	SynPartitionRefCount = 1 << 3
	SynIntrCtrlRegs      = 1 << 6
	SynHypercallRegs     = 1 << 7
	SynVpIndex           = 1 << 8
	SynPartitionRefTsc   = 1 << 9
	SynFrequencyRegs     = 1 << 11
)

// WHV_RUN_VP_EXIT_REASON
const (
	ExitNone                   = 0x0000
	ExitMemoryAccess           = 0x0001
	ExitIoPort                 = 0x0002
	ExitUnrecoverableException = 0x0004
	ExitInvalidVpRegister      = 0x0005
	ExitUnsupportedFeature     = 0x0006
	ExitHalt                   = 0x0008
	ExitApicEoi                = 0x0009
	ExitCanceled               = 0x2001
)

// WHV_REGISTER_NAME
const (
	RegRax    = 0x00
	RegRsp    = 0x04
	RegRsi    = 0x06
	RegRip    = 0x10
	RegRflags = 0x11
	RegEs     = 0x12
	RegCs     = 0x13
	RegSs     = 0x14
	RegDs     = 0x15
	RegFs     = 0x16
	RegGs     = 0x17
	RegGdtr   = 0x1B
	RegCr0    = 0x1C
	RegCr2    = 0x1D
	RegCr3    = 0x1E
	RegCr4    = 0x1F
	RegEfer   = 0x2001

	RegApicId = 0x3002

	// RegInternalActivityState is WHV_INTERNAL_ACTIVITY_REGISTER.
	RegInternalActivityState = 0x80000005
)

// ActivityStartupSuspend is the WHV_INTERNAL_ACTIVITY_REGISTER bit that
// keeps a processor from running until its local APIC receives a SIPI.
const ActivityStartupSuspend = 1 << 0

// RegValue is WHV_REGISTER_VALUE (a 16-byte union).
type RegValue [2]uint64

func Reg64(v uint64) RegValue { return RegValue{v, 0} }

// Segment encodes WHV_X64_SEGMENT_REGISTER {Base, Limit, Selector, Attributes}.
func Segment(base uint64, limit uint32, selector, attributes uint16) RegValue {
	return RegValue{base, uint64(limit) | uint64(selector)<<32 | uint64(attributes)<<48}
}

// Table encodes WHV_X64_TABLE_REGISTER {Pad[3], Limit, Base}.
func Table(base uint64, limit uint16) RegValue {
	return RegValue{uint64(limit) << 48, base}
}

// ExitContext is WHV_RUN_VP_EXIT_CONTEXT (224 bytes on amd64).
type ExitContext [28]uint64

func (c *ExitContext) Bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(c)), int(unsafe.Sizeof(*c)))
}
func (c *ExitContext) Reason() uint32   { return binary.LittleEndian.Uint32(c.Bytes()[0:]) }
func (c *ExitContext) InstrLen() uint64 { return uint64(c.Bytes()[10] & 0x0f) }
func (c *ExitContext) Rip() uint64      { return c[4] }
func (c *ExitContext) Rflags() uint64   { return c[5] }

// I/O port access context (the union starts at byte 48).
func (c *ExitContext) IoInfo() uint32 { return binary.LittleEndian.Uint32(c.Bytes()[68:]) }
func (c *ExitContext) IoPort() uint16 { return binary.LittleEndian.Uint16(c.Bytes()[72:]) }
func (c *ExitContext) IoRax() uint64  { return c[10] }

// Memory access context.
func (c *ExitContext) MemGpa() uint64 { return c[9] }

// Memory access kinds, from WHV_MEMORY_ACCESS_TYPE.
const (
	AccessRead    = 0
	AccessWrite   = 1
	AccessExecute = 2
)

// MemAccess decodes WHV_MEMORY_ACCESS_INFO: the kind of access, and whether
// the guest physical address it touched is mapped at all.
func (c *ExitContext) MemAccess() (kind uint32, unmapped bool) {
	info := binary.LittleEndian.Uint32(c.Bytes()[68:])
	return info & 3, info&4 != 0
}

func check(name string, r uintptr) error {
	if hr := uint32(r); hr&0x80000000 != 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", name, hr)
	}
	return nil
}

// Capability reads a WHV_CAPABILITY into buf.
func Capability(code uint32, buf unsafe.Pointer, size uint32) error {
	if err := whpDLL.Load(); err != nil {
		return err
	}
	var written uint32
	r, _, _ := procGetCapability.Call(uintptr(code), uintptr(buf), uintptr(size), uintptr(unsafe.Pointer(&written)))
	return check("WHvGetCapability", r)
}

// Present reports whether the Windows hypervisor is running and WHP is usable.
func Present() bool {
	var present uint32
	return Capability(CapHypervisorPresent, unsafe.Pointer(&present), 4) == nil && present != 0
}

type Partition struct{ h uintptr }

func CreatePartition() (*Partition, error) {
	if err := whpDLL.Load(); err != nil {
		return nil, err
	}
	var h uintptr
	r, _, _ := procCreatePartition.Call(uintptr(unsafe.Pointer(&h)))
	if err := check("WHvCreatePartition", r); err != nil {
		return nil, err
	}
	return &Partition{h}, nil
}

func (p *Partition) SetProperty(code uint32, buf unsafe.Pointer, size uint32) error {
	r, _, _ := procSetPartitionProperty.Call(p.h, uintptr(code), uintptr(buf), uintptr(size))
	return check(fmt.Sprintf("WHvSetPartitionProperty(0x%x)", code), r)
}

func (p *Partition) Setup() error {
	r, _, _ := procSetupPartition.Call(p.h)
	return check("WHvSetupPartition", r)
}

func (p *Partition) Close() { procDeletePartition.Call(p.h) }

func (p *Partition) MapGpa(host uintptr, gpa, size uint64, flags uint32) error {
	r, _, _ := procMapGpaRange.Call(p.h, host, uintptr(gpa), uintptr(size), uintptr(flags))
	return check("WHvMapGpaRange", r)
}

// PopulateGpa asks the hypervisor to fill in its page tables for a mapped
// range ahead of the guest's first touches, committing the host's pages
// for it as it goes (WHvAdviseGpaRange with WHvAdviseGpaRangeCodePopulate).
func (p *Partition) PopulateGpa(gpa, size uint64) error {
	r := struct{ gpa, size uint64 }{gpa, size}               // WHV_MEMORY_RANGE_ENTRY
	advice := struct{ flags, access uint32 }{0, AccessWrite} // WHV_ADVISE_GPA_RANGE_POPULATE
	const advicePopulate = 0
	rc, _, _ := procAdviseGpaRange.Call(p.h, uintptr(unsafe.Pointer(&r)), 1, advicePopulate,
		uintptr(unsafe.Pointer(&advice)), unsafe.Sizeof(advice))
	return check("WHvAdviseGpaRange", rc)
}

// UnmapGpa removes a mapping made by MapGpa. The range must be exactly one
// that was mapped.
func (p *Partition) UnmapGpa(gpa, size uint64) error {
	r, _, _ := procUnmapGpaRange.Call(p.h, uintptr(gpa), uintptr(size))
	return check("WHvUnmapGpaRange", r)
}

func (p *Partition) CreateVP(index uint32) error {
	r, _, _ := procCreateVP.Call(p.h, uintptr(index), 0)
	return check("WHvCreateVirtualProcessor", r)
}

func (p *Partition) DeleteVP(index uint32) { procDeleteVP.Call(p.h, uintptr(index)) }

func (p *Partition) Run(index uint32, ctx *ExitContext) error {
	r, _, _ := procRunVP.Call(p.h, uintptr(index), uintptr(unsafe.Pointer(ctx)), unsafe.Sizeof(*ctx))
	return check("WHvRunVirtualProcessor", r)
}

// Cancel makes a running Run call return with ExitCanceled. Safe from any thread.
func (p *Partition) Cancel(index uint32) { procCancelRunVP.Call(p.h, uintptr(index), 0) }

func (p *Partition) SetRegs(index uint32, names []uint32, values []RegValue) error {
	r, _, _ := procSetVPRegisters.Call(p.h, uintptr(index),
		uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&values[0])))
	return check("WHvSetVirtualProcessorRegisters", r)
}

func (p *Partition) GetRegs(index uint32, names []uint32, values []RegValue) error {
	r, _, _ := procGetVPRegisters.Call(p.h, uintptr(index),
		uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&values[0])))
	return check("WHvGetVirtualProcessorRegisters", r)
}

// RequestInterrupt asks the hypervisor's local APIC emulation to deliver an
// edge-triggered interrupt. It is safe to call from any thread.
func (p *Partition) RequestInterrupt(deliveryMode, destMode, destination, vector uint32) error {
	// WHV_INTERRUPT_CONTROL {Type:8, DestinationMode:4, TriggerMode:4, ...; Destination; Vector}
	ctl := struct {
		flags       uint64
		destination uint32
		vector      uint32
	}{uint64(deliveryMode) | uint64(destMode)<<8, destination, vector}
	r, _, _ := procRequestInterrupt.Call(p.h, uintptr(unsafe.Pointer(&ctl)), unsafe.Sizeof(ctl))
	return check("WHvRequestInterrupt", r)
}

// VPRuntime reports how long a virtual processor has run, in total and in
// the hypervisor on its behalf (WHV_PROCESSOR_RUNTIME_COUNTERS).
func (p *Partition) VPRuntime(index uint32) (total, hypervisor time.Duration, err error) {
	var c struct{ total, hypervisor uint64 } // 100 ns units
	var written uint32
	const counterSetRuntime = 0
	r, _, _ := procGetVPCounters.Call(p.h, uintptr(index), counterSetRuntime,
		uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c), uintptr(unsafe.Pointer(&written)))
	if err := check("WHvGetVirtualProcessorCounters", r); err != nil {
		return 0, 0, err
	}
	return time.Duration(c.total) * 100, time.Duration(c.hypervisor) * 100, nil
}

const (
	memCommit     = 0x1000
	memReserve    = 0x2000
	memDecommit   = 0x4000
	memRelease    = 0x8000
	memReset      = 0x80000
	pageReadWrite = 0x04
)

// ReserveMemory reserves size bytes of address space outside the Go heap to
// back guest RAM, without committing any of it. Commit parts of it with
// CommitMemory, and release all of it with FreeMemory once the partition is
// gone.
func ReserveMemory(size uint64) (uintptr, []byte, error) {
	addr, _, err := procVirtualAlloc.Call(0, uintptr(size), memReserve, pageReadWrite)
	if addr == 0 {
		return 0, nil, fmt.Errorf("reserving %d MiB of address space: %w", size>>20, err)
	}
	return addr, unsafe.Slice((*byte)(ptr(addr)), int(size)), nil
}

// CommitMemory commits zeroed pages of a reservation.
func CommitMemory(addr uintptr, size uint64) error {
	if r, _, err := procVirtualAlloc.Call(addr, uintptr(size), memCommit, pageReadWrite); r == 0 {
		return fmt.Errorf("committing %d MiB: %w", size>>20, err)
	}
	return nil
}

// DecommitMemory gives committed pages back to Windows. They stay
// reserved, and touching them faults until they are committed again.
func DecommitMemory(addr uintptr, size uint64) error {
	if r, _, err := procVirtualFree.Call(addr, uintptr(size), memDecommit); r == 0 {
		return fmt.Errorf("decommitting %d MiB: %w", size>>20, err)
	}
	return nil
}

// DiscardMemory tells Windows the contents of committed pages are no
// longer needed: it may drop them from the working set without writing
// them anywhere, and they read back as anything. They stay committed.
func DiscardMemory(addr uintptr, size uint64) error {
	if r, _, _ := procDiscardVirtualMemory.Call(addr, uintptr(size)); r != 0 {
		return fmt.Errorf("discarding %d KiB: %w", size>>10, syscall.Errno(r))
	}
	return nil
}

// ResetMemory marks committed pages as not worth keeping, as DiscardMemory
// does, through VirtualAlloc's MEM_RESET.
func ResetMemory(addr uintptr, size uint64) error {
	if r, _, err := procVirtualAlloc.Call(addr, uintptr(size), memReset, pageReadWrite); r == 0 {
		return fmt.Errorf("resetting %d KiB: %w", size>>10, err)
	}
	return nil
}

func FreeMemory(addr uintptr) {
	procVirtualFree.Call(addr, 0, memRelease)
}

// PhysicalMemory reports how much RAM Windows has to work with.
func PhysicalMemory() (uint64, error) {
	// MEMORYSTATUSEX
	var st struct {
		length, memoryLoad                    uint32
		totalPhys, availPhys                  uint64
		totalPageFile, availPageFile          uint64
		totalVirtual, availVirtual, availExtd uint64
	}
	st.length = uint32(unsafe.Sizeof(st))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return st.totalPhys, nil
}

// ProcessMemoryInfo is what Windows holds for a process.
type ProcessMemoryInfo struct {
	// Commit is the private memory committed to the process, which is
	// what Task Manager calls its commit size.
	Commit uint64
	// WorkingSet is the memory resident for it, and PrivateWorkingSet the
	// part not shared with other processes, which is what Task Manager
	// shows as its memory. PrivateWorkingSet is 0 before Windows 10 21H2.
	WorkingSet, PrivateWorkingSet, PeakWorkingSet uint64
}

// ProcessMemory reports what Windows holds for the process behind h.
func ProcessMemory(h syscall.Handle) (ProcessMemoryInfo, error) {
	// PROCESS_MEMORY_COUNTERS_EX2, of which _EX is the first 80 bytes.
	var c struct {
		cb, pageFaults                   uint32
		peakWorkingSet, workingSet       uint64
		quota                            [4]uint64
		pagefileUsage, peakPagefileUsage uint64
		privateUsage, privateWorkingSet  uint64
		sharedCommit                     uint64
	}
	for _, size := range []uint32{uint32(unsafe.Sizeof(c)), 80} {
		c.cb = size
		if r, _, err := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(size)); r == 0 {
			if size == 80 {
				return ProcessMemoryInfo{}, fmt.Errorf("GetProcessMemoryInfo: %w", err)
			}
			continue
		}
		break
	}
	return ProcessMemoryInfo{Commit: c.privateUsage, WorkingSet: c.workingSet,
		PrivateWorkingSet: c.privateWorkingSet, PeakWorkingSet: c.peakWorkingSet}, nil
}

// CurrentProcess is the pseudo-handle for this process.
const CurrentProcess = ^syscall.Handle(0)

// ptr converts an address handed to us by Windows into a pointer without
// tripping vet's unsafe.Pointer arithmetic check. The memory is never on the
// Go heap.
func ptr(addr uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&addr)) }
