// Package virtio implements virtio 1.x devices over the virtio-mmio transport
// with split virtqueues. Devices reach guest memory through the Memory
// interface and signal the guest through a callback, so nothing here depends
// on the hypervisor and everything can be tested against a byte slice.
//
// A Transport is not safe for concurrent use: the VMM drives all of them from
// the vCPU thread.
package virtio

import "encoding/binary"

var le = binary.LittleEndian

// Memory gives access to guest RAM.
type Memory interface {
	// Slice returns the guest memory backing [addr, addr+n), or false if any
	// of it is not RAM.
	Slice(addr, n uint64) ([]byte, bool)
}

// Device is the device-specific half of a virtio device.
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

// NewTransport attaches dev to an MMIO window. raise signals one edge on the
// device's interrupt line.
func NewTransport(dev Device, mem Memory, raise func()) *Transport {
	return &Transport{dev: dev, mem: mem, raise: raise, queues: make([]Queue, dev.NumQueues())}
}

func (t *Transport) Device() Device { return t.dev }
func (t *Transport) Memory() Memory { return t.mem }

// Queue returns queue i, or nil if the driver has not made it ready.
func (t *Transport) Queue(i int) *Queue {
	if i < 0 || i >= len(t.queues) || !t.queues[i].ready() {
		return nil
	}
	return &t.queues[i]
}

// Interrupt reports used buffers to the driver unless queue q asked for no
// interrupts.
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
	case regConfigGen:
		return 0
	}
	return 0
}

// Write serves a guest write of size bytes at offset off in the window.
func (t *Transport) Write(off uint64, size int, val uint64) {
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
}

func (t *Transport) reset() {
	t.devFeatureSel, t.drvFeatureSel, t.drvFeatures = 0, 0, 0
	t.intStatus, t.queueSel = 0, 0
	clear(t.queues)
	t.dev.Reset()
}
