package vm

// IOAPICPins is the number of interrupt lines the emulated I/O APIC has.
// Line 4 is the serial port; virtio devices use lines 5 and up.
const IOAPICPins = 64

// ioapic routes device interrupt lines to local APIC vectors as programmed by
// the guest. Only edge-triggered delivery is implemented.
type ioapic struct {
	// deliver hands one interrupt to the local APIC emulation.
	deliver   func(deliveryMode, destMode, destination, vector uint32) error
	logf      func(format string, a ...any)
	sel       uint32
	id        uint32
	redir     [IOAPICPins]uint64
	pending   [IOAPICPins]bool
	delivered int
}

func newIOAPIC(deliver func(deliveryMode, destMode, destination, vector uint32) error, logf func(string, ...any)) *ioapic {
	a := &ioapic{deliver: deliver, logf: logf}
	for i := range a.redir {
		a.redir[i] = 1 << 16 // masked until the guest programs it
	}
	return a
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
				a.send(int(pin))
			}
		}
	}
}

// pulse signals one edge on an interrupt line.
func (a *ioapic) pulse(pin int) {
	if pin < 0 || pin >= len(a.redir) {
		return
	}
	if a.redir[pin]&(1<<16) != 0 { // masked: remember the edge
		a.pending[pin] = true
		return
	}
	a.send(pin)
}

func (a *ioapic) send(pin int) {
	e := a.redir[pin]
	a.delivered++
	if err := a.deliver(uint32(e>>8&7), uint32(e>>11&1), uint32(e>>56), uint32(e&0xff)); err != nil {
		a.logf("interrupt for pin %d not delivered: %v", pin, err)
	}
}
