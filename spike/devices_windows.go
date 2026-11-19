//go:build windows

package main

// Memory-mapped devices: instruction emulation glue, an I/O APIC for interrupt
// routing, and a virtio-mmio block device. Everything here runs on the vCPU
// thread, so no locking is needed.

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	ioapicBase = 0xfec00000
	virtioBase = 0xd0000000 // one 4 KiB window per virtio device
	uartIRQ    = 4
	virtioIRQ0 = 5 // first virtio device; later devices follow
)

// ---- MMIO instruction emulation (WinHvEmulation.dll) ----

// The emulator decodes the faulting instruction and calls back for register
// state, address translation and the device access itself. Go callbacks carry
// no context, so the single VM of this process is reached through emuVM.
var (
	emuVM        *vm
	emuCallbacks struct {
		size, reserved                              uint32
		ioPort, memory, getRegs, setRegs, translate uintptr
	}
)

type emulator struct{ h uintptr }

func newEmulator(v *vm) (*emulator, error) {
	emuVM = v
	emuCallbacks.size = uint32(unsafe.Sizeof(emuCallbacks))
	emuCallbacks.ioPort = syscall.NewCallback(func(ctx, info uintptr) uintptr { return 0x80004001 }) // E_NOTIMPL
	emuCallbacks.memory = syscall.NewCallback(emuMemory)
	emuCallbacks.getRegs = syscall.NewCallback(func(ctx, names, count, values uintptr) uintptr {
		r, _, _ := procGetVPRegisters.Call(emuVM.part.h, vp0, names, count, values)
		return r
	})
	emuCallbacks.setRegs = syscall.NewCallback(func(ctx, names, count, values uintptr) uintptr {
		r, _, _ := procSetVPRegisters.Call(emuVM.part.h, vp0, names, count, values)
		return r
	})
	emuCallbacks.translate = syscall.NewCallback(func(ctx, gva, flags, resultCode, gpa uintptr) uintptr {
		var result [2]uint32 // WHV_TRANSLATE_GVA_RESULT {ResultCode, Reserved}
		r, _, _ := procTranslateGva.Call(emuVM.part.h, vp0, gva, flags, uintptr(unsafe.Pointer(&result)), gpa)
		*(*uint32)(ptr(resultCode)) = result[0]
		return r
	})
	e := &emulator{}
	r, _, _ := procEmuCreate.Call(uintptr(unsafe.Pointer(&emuCallbacks)), uintptr(unsafe.Pointer(&e.h)))
	return e, check("WHvEmulatorCreateEmulator", r)
}

// emuMemory is WHV_EMULATOR_MEMORY_CALLBACK.
func emuMemory(ctx, info uintptr) uintptr {
	access := (*struct {
		gpa       uint64
		direction uint8 // 0 = read, 1 = write
		size      uint8
		data      [8]uint8
	})(ptr(info))
	size := int(access.size)
	if access.direction == 0 {
		val := emuVM.mmioRead(access.gpa, size)
		for i := 0; i < size; i++ {
			access.data[i] = byte(val >> (8 * i))
		}
		return 0
	}
	var val uint64
	for i := 0; i < size; i++ {
		val |= uint64(access.data[i]) << (8 * i)
	}
	emuVM.mmioWrite(access.gpa, size, val)
	return 0
}

func (e *emulator) mmio(ctx *exitContext) error {
	b := ctx.bytes()
	var status uint32
	r, _, _ := procEmuTryMmio.Call(e.h, 0,
		uintptr(unsafe.Pointer(&b[8])), uintptr(unsafe.Pointer(&b[48])), uintptr(unsafe.Pointer(&status)))
	if err := check("WHvEmulatorTryMmioEmulation", r); err != nil {
		return err
	}
	if status&1 == 0 {
		return fmt.Errorf("MMIO emulation failed at GPA %#x, rip %#x (status %#x)", ctx.memGpa(), ctx.rip(), status)
	}
	return nil
}

func (v *vm) mmioRead(gpa uint64, size int) uint64 {
	v.mmioCount++
	if gpa >= ioapicBase && gpa < ioapicBase+0x1000 {
		return uint64(v.ioapic.read(gpa - ioapicBase))
	}
	if d := v.virtioAt(gpa); d != nil {
		return d.read(gpa-d.base, size)
	}
	v.unknownMMIO(gpa)
	return ^uint64(0)
}

func (v *vm) mmioWrite(gpa uint64, size int, val uint64) {
	v.mmioCount++
	if gpa >= ioapicBase && gpa < ioapicBase+0x1000 {
		v.ioapic.write(gpa-ioapicBase, uint32(val))
		return
	}
	if d := v.virtioAt(gpa); d != nil {
		d.write(gpa-d.base, size, val)
		return
	}
	v.unknownMMIO(gpa)
}

func (v *vm) virtioAt(gpa uint64) *virtioBlk {
	if gpa < virtioBase {
		return nil
	}
	if i := (gpa - virtioBase) >> 12; i < uint64(len(v.disks)) {
		return v.disks[i]
	}
	return nil
}

func (v *vm) unknownMMIO(gpa uint64) {
	if page := gpa &^ 0xfff; !v.unknownPages[page] {
		v.unknownPages[page] = true
		logf("access to unbacked MMIO page %#x ignored", page)
	}
}

// guest returns the guest RAM backing [addr, addr+n).
func (v *vm) guest(addr, n uint64) ([]byte, bool) {
	if addr+n < addr || addr+n > uint64(len(v.mem)) {
		return nil, false
	}
	return v.mem[addr : addr+n], true
}

// ---- I/O APIC ----

// ioapic routes device interrupt lines to local APIC vectors as programmed by
// the guest. Only edge-triggered delivery is implemented.
type ioapic struct {
	part      *partition
	sel       uint32
	id        uint32
	redir     [24]uint64
	pending   [24]bool
	delivered int
}

func (a *ioapic) read(off uint64) uint32 {
	switch off {
	case 0x00:
		return a.sel
	case 0x10:
		switch {
		case a.sel == 0, a.sel == 2:
			return a.id << 24
		case a.sel == 1:
			return uint32(len(a.redir)-1)<<16 | 0x11
		case a.sel >= 0x10 && a.sel < 0x10+2*uint32(len(a.redir)):
			entry := a.redir[(a.sel-0x10)/2]
			if a.sel&1 != 0 {
				return uint32(entry >> 32)
			}
			return uint32(entry)
		}
	}
	return 0
}

func (a *ioapic) write(off uint64, val uint32) {
	switch off {
	case 0x00:
		a.sel = val & 0xff
	case 0x10:
		switch {
		case a.sel == 0:
			a.id = val >> 24 & 0x0f
		case a.sel >= 0x10 && a.sel < 0x10+2*uint32(len(a.redir)):
			pin := (a.sel - 0x10) / 2
			if a.sel&1 != 0 {
				a.redir[pin] = a.redir[pin]&0xffffffff | uint64(val)<<32
			} else {
				a.redir[pin] = a.redir[pin]&^0xffffffff | uint64(val&^0x5000) // status bits are read-only
			}
			if a.pending[pin] && a.redir[pin]&(1<<16) == 0 {
				a.pending[pin] = false
				a.deliver(int(pin))
			}
		}
	}
}

// pulse signals one edge on an interrupt line.
func (a *ioapic) pulse(pin int) {
	if a.redir[pin]&(1<<16) != 0 { // masked: remember the edge
		a.pending[pin] = true
		return
	}
	a.deliver(pin)
}

func (a *ioapic) deliver(pin int) {
	e := a.redir[pin]
	a.delivered++
	if err := a.part.requestInterrupt(uint32(e>>8&7), uint32(e>>11&1), uint32(e>>56), uint32(e&0xff)); err != nil {
		logf("interrupt for pin %d not delivered: %v", pin, err)
	}
}

// ---- virtio-mmio block device (virtio 1.x, split virtqueue) ----

const (
	virtioQueueSize = 256

	featBlkSegMax  = 1 << 2
	featBlkRO      = 1 << 5
	featBlkFlush   = 1 << 9
	featVersion1   = 1 << 32
	blkIn, blkOut  = 0, 1
	blkFlush       = 4
	blkGetID       = 8
	blkOK, blkErr  = 0, 1
	blkUnsupported = 2
)

type virtioBlk struct {
	vm       *vm
	base     uint64
	irq      int
	file     *os.File
	readOnly bool
	sectors  uint64

	status                    uint32
	featureSel, drvFeatureSel uint32
	drvFeatures               uint64
	intStatus                 uint32
	queueNum                  uint32
	queueReady                bool
	desc, avail, used         uint64
	lastAvail                 uint16

	requests, bytesRead, bytesWritten uint64
}

func openDisk(v *vm, index int, spec string) (*virtioBlk, error) {
	d := &virtioBlk{vm: v, base: virtioBase + uint64(index)<<12, irq: virtioIRQ0 + index}
	path := spec
	if n := len(spec) - len(":ro"); n > 0 && spec[n:] == ":ro" {
		path, d.readOnly = spec[:n], true
	}
	flags := os.O_RDWR
	if d.readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	d.file, d.sectors = f, uint64(st.Size())/512
	return d, nil
}

func (d *virtioBlk) features() uint64 {
	f := uint64(featVersion1 | featBlkSegMax | featBlkFlush)
	if d.readOnly {
		f |= featBlkRO
	}
	return f
}

func (d *virtioBlk) config() []byte {
	cfg := make([]byte, 64)
	le.PutUint64(cfg[0:], d.sectors)
	le.PutUint32(cfg[12:], virtioQueueSize-2) // seg_max
	return cfg
}

func (d *virtioBlk) read(off uint64, size int) uint64 {
	if off >= 0x100 {
		cfg := d.config()
		var val uint64
		for i := 0; i < size && int(off-0x100)+i < len(cfg); i++ {
			val |= uint64(cfg[int(off-0x100)+i]) << (8 * i)
		}
		return val
	}
	switch off {
	case 0x000:
		return 0x74726976 // "virt"
	case 0x004:
		return 2 // virtio-mmio version
	case 0x008:
		return 2 // block device
	case 0x00c:
		return 0x574e5552 // vendor "runwhp"
	case 0x010:
		return d.features() >> (32 * (d.featureSel & 1)) & 0xffffffff
	case 0x034:
		return virtioQueueSize
	case 0x044:
		if d.queueReady {
			return 1
		}
	case 0x060:
		return uint64(d.intStatus)
	case 0x070:
		return uint64(d.status)
	}
	return 0
}

func (d *virtioBlk) write(off uint64, size int, val uint64) {
	v32 := uint32(val)
	setLow := func(p *uint64) { *p = *p&^0xffffffff | uint64(v32) }
	setHigh := func(p *uint64) { *p = *p&0xffffffff | uint64(v32)<<32 }
	switch off {
	case 0x014:
		d.featureSel = v32
	case 0x020:
		if d.drvFeatureSel&1 != 0 {
			setHigh(&d.drvFeatures)
		} else {
			setLow(&d.drvFeatures)
		}
	case 0x024:
		d.drvFeatureSel = v32
	case 0x038:
		d.queueNum = v32
	case 0x044:
		d.queueReady = v32&1 != 0
	case 0x050:
		d.processQueue()
	case 0x064:
		d.intStatus &^= v32
	case 0x070:
		d.status = v32
		if v32 == 0 { // reset
			d.queueReady, d.lastAvail, d.intStatus, d.drvFeatures = false, 0, 0, 0
		}
	case 0x080:
		setLow(&d.desc)
	case 0x084:
		setHigh(&d.desc)
	case 0x090:
		setLow(&d.avail)
	case 0x094:
		setHigh(&d.avail)
	case 0x0a0:
		setLow(&d.used)
	case 0x0a4:
		setHigh(&d.used)
	}
}

// processQueue serves every request the driver has made available, then
// raises the device interrupt once.
func (d *virtioBlk) processQueue() {
	n := uint64(d.queueNum)
	if !d.queueReady || n == 0 || n > virtioQueueSize {
		return
	}
	avail, ok1 := d.vm.guest(d.avail, 4+2*n)
	used, ok2 := d.vm.guest(d.used, 4+8*n)
	if !ok1 || !ok2 {
		return
	}
	served := false
	for d.lastAvail != le.Uint16(avail[2:]) {
		head := le.Uint16(avail[4+2*(uint64(d.lastAvail)%n):])
		d.lastAvail++
		written := d.request(head)
		idx := le.Uint16(used[2:])
		slot := used[4+8*(uint64(idx)%n):]
		le.PutUint32(slot[0:], uint32(head))
		le.PutUint32(slot[4:], written)
		le.PutUint16(used[2:], idx+1)
		served = true
	}
	if served && le.Uint16(avail[0:])&1 == 0 { // driver has not suppressed interrupts
		d.intStatus |= 1
		d.vm.ioapic.pulse(d.irq)
	}
}

// request executes one descriptor chain: header, data buffers, status byte.
// It returns the number of bytes written into guest memory.
func (d *virtioBlk) request(head uint16) uint32 {
	type buffer struct {
		data     []byte
		writable bool
	}
	var chain []buffer
	n := uint64(d.queueNum)
	for i, next := uint64(0), uint64(head); i < n; i++ {
		desc, ok := d.vm.guest(d.desc+16*(next%n), 16)
		if !ok {
			return 0
		}
		flags := le.Uint16(desc[12:])
		data, ok := d.vm.guest(le.Uint64(desc[0:]), uint64(le.Uint32(desc[8:])))
		if !ok {
			return 0
		}
		chain = append(chain, buffer{data, flags&2 != 0})
		if flags&1 == 0 {
			break
		}
		next = uint64(le.Uint16(desc[14:]))
	}
	if len(chain) < 2 || len(chain[0].data) < 16 {
		return 0
	}
	status := chain[len(chain)-1]
	if !status.writable || len(status.data) < 1 {
		return 0
	}
	kind := le.Uint32(chain[0].data[0:])
	offset := int64(le.Uint64(chain[0].data[8:])) * 512
	result, written := byte(blkOK), uint32(0)
	d.requests++
	for _, buf := range chain[1 : len(chain)-1] {
		var err error
		switch {
		case kind == blkIn && buf.writable:
			_, err = d.file.ReadAt(buf.data, offset)
			written += uint32(len(buf.data))
			d.bytesRead += uint64(len(buf.data))
		case kind == blkOut && !buf.writable && !d.readOnly:
			_, err = d.file.WriteAt(buf.data, offset)
			d.bytesWritten += uint64(len(buf.data))
		case kind == blkGetID && buf.writable:
			written += uint32(copy(buf.data, fmt.Sprintf("runwhp-spike-%d", d.irq-virtioIRQ0)))
		default:
			result = blkErr
		}
		if err != nil {
			result = blkErr
		}
		offset += int64(len(buf.data))
	}
	switch kind {
	case blkIn, blkOut, blkGetID:
	case blkFlush:
		if err := d.file.Sync(); err != nil && !d.readOnly {
			result = blkErr
		}
	default:
		result = blkUnsupported
	}
	status.data[0] = result
	return written + 1
}
