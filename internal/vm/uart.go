package vm

// uart is an output-only 16550A at COM1. It raises a transmit-ready interrupt
// so the guest's tty driver works, but it never delivers input. Output is
// reported a line at a time.
type uart struct {
	ier, lcr, mcr, scr, dll, dlm, fcr byte
	iir                               byte   // pending interrupt identification; 0x01 means none
	raise                             func() // signals an edge on the UART's interrupt line
	onLine                            func(string)
	line                              []byte
	lines, bytes                      int
}

const (
	uartBase = 0x3f8
	uartIRQ  = 4
)

func newUART(raise func(), onLine func(string)) *uart {
	return &uart{iir: 0x01, raise: raise, onLine: onLine}
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
	switch b {
	case '\r':
		return
	case '\n':
		u.lines++
		u.report()
	default:
		u.line = append(u.line, b)
	}
}

func (u *uart) report() {
	line := string(u.line)
	u.line = u.line[:0]
	if u.onLine != nil {
		u.onLine(line)
	}
}

// flush reports an unterminated last line, if any.
func (u *uart) flush() {
	if len(u.line) > 0 {
		u.report()
	}
}
