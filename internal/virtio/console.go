package virtio

import (
	"fmt"
	"io"
)

// Multiport console feature bit and control events (virtio spec 5.3.6.2).
const (
	consoleFeatMultiport = 1 << 1

	ctrlDeviceReady = 0
	ctrlPortAdd     = 1
	ctrlPortReady   = 3
	ctrlPortOpen    = 6
	ctrlPortName    = 7
)

// Port is one named channel of a Console. The guest finds it as
// /dev/vportNpM, with Name in /sys/class/virtio-ports/vportNpM/name.
type Port struct {
	Name string
	// Out receives what the guest writes to the port. Writes happen on the
	// vCPU thread, so by the time the guest's write() returns the bytes have
	// been handed to Out. nil discards them.
	Out io.Writer
}

// Console is a multiport virtio console. Port ids start at 1: like QEMU, it
// leaves port 0 unused, since drivers treat it as the legacy console port.
// Data from the host to the guest is not supported yet.
type Console struct {
	Ports []Port
	// Logf, when set, reports protocol problems.
	Logf func(format string, a ...any)

	pending      [][]byte // control messages waiting for a receive buffer
	guestReady   map[uint32]bool
	BytesWritten uint64
}

// Queue layout: port 0 receive/transmit, control receive/transmit, then a
// receive/transmit pair for each further port.
const (
	ctrlRxQueue = 2 // device to driver
	ctrlTxQueue = 3 // driver to device
)

func (c *Console) maxPorts() int    { return len(c.Ports) + 1 }
func (c *Console) DeviceID() uint32 { return 3 }
func (c *Console) NumQueues() int   { return 2 * (c.maxPorts() + 1) }
func (c *Console) Features() uint64 { return FeatVersion1 | consoleFeatMultiport }

func (c *Console) Reset() {
	c.pending = nil
	c.guestReady = nil
}

func (c *Console) Config() []byte {
	cfg := make([]byte, 12) // cols, rows, max_nr_ports, emerg_wr
	le.PutUint32(cfg[4:], uint32(c.maxPorts()))
	return cfg
}

// portForQueue maps a queue index to a port id and direction.
func portForQueue(q int) (id int, transmit bool) {
	if q < 2 {
		return 0, q == 1
	}
	return q/2 - 1, q%2 == 1
}

func (c *Console) logf(format string, a ...any) {
	if c.Logf != nil {
		c.Logf(format, a...)
	}
}

func (c *Console) Notify(t *Transport, qi int) {
	switch qi {
	case ctrlTxQueue:
		c.control(t)
	case ctrlRxQueue:
		c.flush(t)
	default:
		id, transmit := portForQueue(qi)
		if transmit {
			c.transmit(t, qi, id)
		}
		// Receive buffers posted by the guest stay queued: nothing is sent
		// to the guest yet.
	}
}

func (c *Console) transmit(t *Transport, qi, id int) {
	q := t.Queue(qi)
	var out io.Writer
	if id >= 1 && id <= len(c.Ports) {
		out = c.Ports[id-1].Out
	}
	served := false
	for {
		ch, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		if err == nil {
			for _, b := range ch.Buffers {
				if b.Writable {
					continue
				}
				c.BytesWritten += uint64(len(b.Data))
				if out != nil {
					out.Write(b.Data)
				}
			}
		}
		q.Push(t.Memory(), ch.Head, 0)
		served = true
	}
	if served {
		t.Interrupt(qi)
	}
}

// control handles messages from the driver.
func (c *Console) control(t *Transport) {
	q := t.Queue(ctrlTxQueue)
	served := false
	for {
		ch, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		if err == nil {
			for _, b := range ch.Buffers {
				if !b.Writable && len(b.Data) >= 8 {
					c.handle(le.Uint32(b.Data[0:]), le.Uint16(b.Data[4:]), le.Uint16(b.Data[6:]))
				}
			}
		}
		q.Push(t.Memory(), ch.Head, 0)
		served = true
	}
	if served {
		t.Interrupt(ctrlTxQueue)
	}
	c.flush(t)
}

func (c *Console) handle(id uint32, event, value uint16) {
	switch event {
	case ctrlDeviceReady:
		if value != 1 {
			c.logf("virtio-console: driver failed to initialise")
			return
		}
		for i := range c.Ports {
			c.send(uint32(i+1), ctrlPortAdd, 1, "")
		}
	case ctrlPortReady:
		if id < 1 || int(id) > len(c.Ports) {
			return
		}
		if value != 1 {
			c.logf("virtio-console: driver failed to add port %q", c.Ports[id-1].Name)
			return
		}
		if c.guestReady == nil {
			c.guestReady = map[uint32]bool{}
		}
		if c.guestReady[id] {
			return
		}
		c.guestReady[id] = true
		c.send(id, ctrlPortName, 1, c.Ports[id-1].Name)
		// The host side is always connected. Until it says so, writes to
		// the port block in the guest.
		c.send(id, ctrlPortOpen, 1, "")
	case ctrlPortOpen:
		// The guest opened or closed the port; nothing to do.
	default:
		c.logf("virtio-console: ignoring control event %d for port %d", event, id)
	}
}

func (c *Console) send(id uint32, event, value uint16, name string) {
	msg := make([]byte, 8+len(name))
	le.PutUint32(msg[0:], id)
	le.PutUint16(msg[4:], event)
	le.PutUint16(msg[6:], value)
	copy(msg[8:], name)
	c.pending = append(c.pending, msg)
}

// flush delivers pending control messages into receive buffers the driver
// has posted.
func (c *Console) flush(t *Transport) {
	q := t.Queue(ctrlRxQueue)
	if q == nil {
		return
	}
	served := false
	for len(c.pending) > 0 {
		ch, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		var written uint32
		if err == nil && len(ch.Buffers) > 0 && ch.Buffers[0].Writable && len(ch.Buffers[0].Data) >= len(c.pending[0]) {
			written = uint32(copy(ch.Buffers[0].Data, c.pending[0]))
			c.pending = c.pending[1:]
		} else {
			c.logf("virtio-console: control receive buffer unusable (%v)", errOr(err, fmt.Errorf("too small")))
		}
		q.Push(t.Memory(), ch.Head, written)
		served = true
	}
	if served {
		t.Interrupt(ctrlRxQueue)
	}
}

func errOr(err, alt error) error {
	if err != nil {
		return err
	}
	return alt
}
