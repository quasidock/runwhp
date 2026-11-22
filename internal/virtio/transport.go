// Package virtio implements virtio 1.x devices over the virtio-mmio transport
// with split virtqueues. Devices reach guest memory through the Memory
// interface and signal the guest through a callback, so nothing here depends
// on the hypervisor and everything can be tested against a byte slice.
//
// Each Transport has a lock. The VMM's register accesses take it, and so does
// Do, through which a device works on its queues from other goroutines while
// the guest runs: the console, for instance, writes output and delivers input
// without holding up the vCPU.
package virtio

import (
	"encoding/binary"
	"sync"
)

var le = binary.LittleEndian

// Memory gives access to guest RAM.
type Memory interface {
	// Slice returns the guest memory backing [addr, addr+n), or false if any
	// of it is not RAM.
	Slice(addr, n uint64) ([]byte, bool)
}

// Discarder is implemented by guest memory that can give RAM back to the
// host.
type Discarder interface {
	// Contains reports whether [addr, addr+n) is all RAM, without
	// touching it.
	Contains(addr, n uint64) bool
	// Discard gives back the RAM in [addr, addr+n), which the guest no
	// longer uses, and reports false if it is not all RAM.
	Discard(addr, n uint64) bool
}

// Device is the device-specific half of a virtio device.
//
// Config, Notify and Reset are called with the transport's lock held, so they
// may use t.Queue and t.Interrupt directly but must not call t.Do.
type Device interface {
	DeviceID() uint32
	Features() uint64
	Config() []byte
	NumQueues() int
	// Notify is called when the driver kicks queue q.
	Notify(t *Transport, q int)
	// Reset is called when the driver resets the device.
	Reset()
}

// Feature bits common to all devices.
const FeatVersion1 = 1 << 32

// WindowSize is the size of one device's MMIO register window.
const WindowSize = 0x1000

// Transport is one virtio-mmio register window.
type Transport struct {
	mu     sync.Mutex
	closed bool

	dev   Device
	mem   Memory
	raise func()

	status        uint32
	devFeatureSel uint32
	drvFeatureSel uint32
	drvFeatures   uint64
	intStatus     uint32
	queueSel      uint32
	queues        []Queue
}

// Deferrer is implemented by devices that do some of a notification's work
// on the vCPU that sent it, but without the transport's lock, so other
// vCPUs can use the device meanwhile. NotifyDeferred is called instead of
// Notify, with the lock held, and the work it returns, if any, runs once
// the lock is released. That work uses t.Do like any other goroutine.
type Deferrer interface {
	NotifyDeferred(t *Transport, q int) func()
}

// Attacher is implemented by devices that work on their queues from outside
// Notify, at the host's initiative, and so need their transport before the
// driver first kicks them.
type Attacher interface {
	Attach(t *Transport)
}

// NewTransport attaches dev to an MMIO window. raise signals one edge on the
// device's interrupt line.
func NewTransport(dev Device, mem Memory, raise func()) *Transport {
	t := &Transport{dev: dev, mem: mem, raise: raise, queues: make([]Queue, dev.NumQueues())}
	if a, ok := dev.(Attacher); ok {
		a.Attach(t)
	}
	return t
}

// DriverFeatures returns the feature bits the driver accepted. The lock must
// be held.
func (t *Transport) DriverFeatures() uint64 { return t.drvFeatures }

func (t *Transport) Device() Device { return t.dev }
func (t *Transport) Memory() Memory { return t.mem }

// Do runs f with the transport locked, so f may use Queue, Memory and
// Interrupt. It does nothing once the transport is closed, and reports whether
// f ran.
func (t *Transport) Do(f func()) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	f()
	return true
}

// Close stops all further work through Do. It waits for work in progress, so
// once it returns nothing reaches guest memory through this transport.
func (t *Transport) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

// Queue returns queue i, or nil if the driver has not made it ready. The
// lock must be held.
func (t *Transport) Queue(i int) *Queue {
	if i < 0 || i >= len(t.queues) || !t.queues[i].ready() {
		return nil
	}
	return &t.queues[i]
}

// Interrupt reports used buffers to the driver unless queue q asked for no
// interrupts. The lock must be held.
func (t *Transport) Interrupt(q int) {
	if qu := t.Queue(q); qu != nil && qu.interruptsSuppressed(t.mem) {
		return
	}
	t.intStatus |= 1
	t.raise()
}

// MMIO register offsets (virtio-mmio version 2).
const (
	regMagic         = 0x000
	regVersion       = 0x004
	regDeviceID      = 0x008
	regVendorID      = 0x00c
	regDevFeatures   = 0x010
	regDevFeatSel    = 0x014
	regDrvFeatures   = 0x020
	regDrvFeatSel    = 0x024
	regQueueSel      = 0x030
	regQueueNumMax   = 0x034
	regQueueNum      = 0x038
	regQueueReady    = 0x044
	regQueueNotify   = 0x050
	regIntStatus     = 0x060
	regIntAck        = 0x064
	regStatus        = 0x070
	regQueueDescLo   = 0x080
	regQueueDescHi   = 0x084
	regQueueDriverLo = 0x090
	regQueueDriverHi = 0x094
	regQueueDeviceLo = 0x0a0
	regQueueDeviceHi = 0x0a4
	regSHMLenLo      = 0x0b0
	regSHMLenHi      = 0x0b4
	regSHMBaseLo     = 0x0b8
	regSHMBaseHi     = 0x0bc
	regConfigGen     = 0x0fc
	regConfig        = 0x100
)

func (t *Transport) selected() *Queue {
	if int(t.queueSel) < len(t.queues) {
		return &t.queues[t.queueSel]
	}
	return nil
}

// Read serves a guest read of size bytes at offset off in the window.
func (t *Transport) Read(off uint64, size int) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if off >= regConfig {
		cfg := t.dev.Config()
		var val uint64
		for i := 0; i < size && int(off-regConfig)+i < len(cfg); i++ {
			val |= uint64(cfg[int(off-regConfig)+i]) << (8 * i)
		}
		return val
	}
	switch off {
	case regMagic:
		return 0x74726976 // "virt"
	case regVersion:
		return 2
	case regDeviceID:
		return uint64(t.dev.DeviceID())
	case regVendorID:
		return 0x57485052 // "RPHW"
	case regDevFeatures:
		if t.devFeatureSel > 1 {
			return 0
		}
		return t.dev.Features() >> (32 * t.devFeatureSel) & 0xffffffff
	case regQueueNumMax:
		if t.selected() != nil {
			return MaxQueueSize
		}
	case regQueueReady:
		if q := t.selected(); q != nil && q.Ready {
			return 1
		}
	case regIntStatus:
		return uint64(t.intStatus)
	case regStatus:
		return uint64(t.status)
	case regSHMLenLo, regSHMLenHi, regSHMBaseLo, regSHMBaseHi:
		// No device has shared memory regions. A missing region reads as
		// length -1; with 0, a kernel built with FUSE_DAX tries to reserve
		// it, fails, and gives up on the virtio-fs device.
		return 0xffffffff
	case regConfigGen:
		return 0
	}
	return 0
}

// Write serves a guest write of size bytes at offset off in the window.
func (t *Transport) Write(off uint64, size int, val uint64) {
	if after := t.write(off, val); after != nil {
		after()
	}
}

// write serves a register write with the lock held, and returns what a
// Deferrer left to do once it is released.
func (t *Transport) write(off uint64, val uint64) (after func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v32 := uint32(val)
	setLow := func(p *uint64) { *p = *p&^0xffffffff | uint64(v32) }
	setHigh := func(p *uint64) { *p = *p&0xffffffff | uint64(v32)<<32 }
	q := t.selected()
	switch off {
	case regDevFeatSel:
		t.devFeatureSel = v32
	case regDrvFeatures:
		switch t.drvFeatureSel {
		case 0:
			setLow(&t.drvFeatures)
		case 1:
			setHigh(&t.drvFeatures)
		}
	case regDrvFeatSel:
		t.drvFeatureSel = v32
	case regQueueSel:
		t.queueSel = v32
	case regQueueNum:
		if q != nil {
			q.Size = v32
		}
	case regQueueReady:
		if q != nil {
			q.Ready = v32&1 != 0
		}
	case regQueueNotify:
		if int(v32) < len(t.queues) && t.queues[v32].ready() {
			if d, ok := t.dev.(Deferrer); ok {
				return d.NotifyDeferred(t, int(v32))
			}
			t.dev.Notify(t, int(v32))
		}
	case regIntAck:
		t.intStatus &^= v32
	case regStatus:
		t.status = v32
		if v32 == 0 {
			t.reset()
		}
	case regQueueDescLo, regQueueDescHi, regQueueDriverLo, regQueueDriverHi, regQueueDeviceLo, regQueueDeviceHi:
		if q == nil {
			return
		}
		p := &q.Desc
		switch off &^ 4 {
		case regQueueDriverLo:
			p = &q.Avail
		case regQueueDeviceLo:
			p = &q.Used
		}
		if off&4 == 0 {
			setLow(p)
		} else {
			setHigh(p)
		}
	}
	return nil
}

func (t *Transport) reset() {
	t.devFeatureSel, t.drvFeatureSel, t.drvFeatures = 0, 0, 0
	t.intStatus, t.queueSel = 0, 0
	clear(t.queues)
	t.dev.Reset()
}
