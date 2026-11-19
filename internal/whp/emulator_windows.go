//go:build windows

package whp

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

var (
	emuDLL         = syscall.NewLazyDLL("WinHvEmulation.dll")
	procEmuCreate  = emuDLL.NewProc("WHvEmulatorCreateEmulator")
	procEmuDestroy = emuDLL.NewProc("WHvEmulatorDestroyEmulator")
	procEmuTryMmio = emuDLL.NewProc("WHvEmulatorTryMmioEmulation")
)

// MemoryHandler serves one memory-mapped access decoded by the emulator. For a
// read it fills data; for a write data holds the value. len(data) is the
// access size in bytes.
type MemoryHandler func(gpa uint64, data []byte, write bool)

// Emulator wraps WinHvEmulation.dll, which decodes the instruction behind a
// memory-access exit and calls back for registers, address translation and
// the device access itself.
type Emulator struct {
	h    uintptr
	id   uintptr
	part *Partition
	vp   uint32
	mem  MemoryHandler
}

// Go callbacks carry no closure, and syscall.NewCallback slots are never
// freed, so one set of callbacks is shared by every emulator. The emulator is
// found through the context value passed to WHvEmulatorTryMmioEmulation.
var (
	emuOnce      sync.Once
	emuCallbacks struct {
		size, reserved                              uint32
		ioPort, memory, getRegs, setRegs, translate uintptr
	}
	emuMu     sync.Mutex
	emuNextID uintptr
	emuByID   = map[uintptr]*Emulator{}
)

func emulatorFor(ctx uintptr) *Emulator {
	emuMu.Lock()
	defer emuMu.Unlock()
	return emuByID[ctx]
}

func initCallbacks() {
	emuCallbacks.size = uint32(unsafe.Sizeof(emuCallbacks))
	emuCallbacks.ioPort = syscall.NewCallback(func(ctx, info uintptr) uintptr { return 0x80004001 }) // E_NOTIMPL
	emuCallbacks.memory = syscall.NewCallback(func(ctx, info uintptr) uintptr {
		e := emulatorFor(ctx)
		if e == nil {
			return 0x80004005 // E_FAIL
		}
		// WHV_EMULATOR_MEMORY_ACCESS_INFO {GpaAddress, Direction, AccessSize, Data[8]}
		access := (*struct {
			gpa       uint64
			direction uint8 // 0 = read, 1 = write
			size      uint8
			data      [8]uint8
		})(ptr(info))
		n := min(int(access.size), len(access.data))
		e.mem(access.gpa, access.data[:n], access.direction != 0)
		return 0
	})
	emuCallbacks.getRegs = syscall.NewCallback(func(ctx, names, count, values uintptr) uintptr {
		e := emulatorFor(ctx)
		if e == nil {
			return 0x80004005
		}
		r, _, _ := procGetVPRegisters.Call(e.part.h, uintptr(e.vp), names, count, values)
		return r
	})
	emuCallbacks.setRegs = syscall.NewCallback(func(ctx, names, count, values uintptr) uintptr {
		e := emulatorFor(ctx)
		if e == nil {
			return 0x80004005
		}
		r, _, _ := procSetVPRegisters.Call(e.part.h, uintptr(e.vp), names, count, values)
		return r
	})
	emuCallbacks.translate = syscall.NewCallback(func(ctx, gva, flags, resultCode, gpa uintptr) uintptr {
		e := emulatorFor(ctx)
		if e == nil {
			return 0x80004005
		}
		var result [2]uint32 // WHV_TRANSLATE_GVA_RESULT {ResultCode, Reserved}
		r, _, _ := procTranslateGva.Call(e.part.h, uintptr(e.vp), gva, flags, uintptr(unsafe.Pointer(&result)), gpa)
		*(*uint32)(ptr(resultCode)) = result[0]
		return r
	})
}

// NewEmulator creates an instruction emulator for one virtual processor.
func NewEmulator(p *Partition, vp uint32, mem MemoryHandler) (*Emulator, error) {
	if err := emuDLL.Load(); err != nil {
		return nil, err
	}
	emuOnce.Do(initCallbacks)
	e := &Emulator{part: p, vp: vp, mem: mem}
	r, _, _ := procEmuCreate.Call(uintptr(unsafe.Pointer(&emuCallbacks)), uintptr(unsafe.Pointer(&e.h)))
	if err := check("WHvEmulatorCreateEmulator", r); err != nil {
		return nil, err
	}
	emuMu.Lock()
	emuNextID++
	e.id = emuNextID
	emuByID[e.id] = e
	emuMu.Unlock()
	return e, nil
}

func (e *Emulator) Close() {
	emuMu.Lock()
	delete(emuByID, e.id)
	emuMu.Unlock()
	procEmuDestroy.Call(e.h)
}

// TryMmio emulates the instruction behind a memory-access exit.
func (e *Emulator) TryMmio(ctx *ExitContext) error {
	b := ctx.Bytes()
	var status uint32
	r, _, _ := procEmuTryMmio.Call(e.h, e.id,
		uintptr(unsafe.Pointer(&b[8])), uintptr(unsafe.Pointer(&b[48])), uintptr(unsafe.Pointer(&status)))
	if err := check("WHvEmulatorTryMmioEmulation", r); err != nil {
		return err
	}
	if status&1 == 0 {
		return fmt.Errorf("MMIO emulation failed at GPA %#x, rip %#x (status %#x)", ctx.MemGpa(), ctx.Rip(), status)
	}
	return nil
}
