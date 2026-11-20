package virtio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
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
	// Out receives what the guest writes to the port, in order, from a
	// goroutine of the port's own, so a slow Out holds up this port and
	// nothing else. The guest's write() can return before Out has the
	// bytes; Drain waits until it has them all. nil discards them.
	Out io.Writer
	// In, when set, is read once the guest opens the port, and what it
	// yields is sent to the guest. When In ends, the host side of the port
	// closes, which the guest reads as end of file after the last data.
	In io.Reader
}

// Console is a multiport virtio console. Port ids start at 1: like QEMU, it
// leaves port 0 unused, since drivers treat it as the legacy console port.
type Console struct {
	Ports []Port
	// Logf, when set, reports protocol problems.
	Logf func(format string, a ...any)

	// BytesWritten counts bytes from the guest, BytesRead bytes to it.
	BytesWritten, BytesRead atomic.Uint64

	once     sync.Once
	ports    []portState // ports[id-1]
	stop     chan struct{}
	stopOnce sync.Once
	inflight sync.WaitGroup // transmit chains not yet returned to the driver

	// Guarded by the transport's lock.
	gen     int // bumped by Reset; completions of older chains are dropped
	pending [][]byte
}

type portState struct {
	open atomic.Bool // the guest has the port open

	// Guarded by the transport's lock.
	ready   bool // PORT_READY answered
	pumping bool
	tx      chan txChain // nil until the port first transmits

	// posted gets a token when the guest posts receive buffers or opens the
	// port, for a pump waiting to deliver.
	posted chan struct{}
}

// txChain is a transmit buffer handed to a port's writer.
type txChain struct {
	gen  int
	q    int
	head uint16
	bufs []Buffer
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

func (c *Console) init() {
	c.once.Do(func() {
		c.ports = make([]portState, len(c.Ports))
		for i := range c.ports {
			c.ports[i].posted = make(chan struct{}, 1)
		}
		c.stop = make(chan struct{})
	})
}

func (c *Console) Reset() {
	c.init()
	c.pending = nil
	c.gen++
	for i := range c.ports {
		c.ports[i].ready = false
		c.ports[i].open.Store(false)
	}
}

func (c *Console) Config() []byte {
	cfg := make([]byte, 12) // cols, rows, max_nr_ports, emerg_wr
	le.PutUint32(cfg[4:], uint32(c.maxPorts()))
	return cfg
}

// Connected reports whether the guest has the named port open, and so will
// receive what is sent to it.
func (c *Console) Connected(name string) bool {
	c.init()
	select {
	case <-c.stop:
		return false
	default:
	}
	for i, p := range c.Ports {
		if p.Name == name {
			return c.ports[i].open.Load()
		}
	}
	return false
}

// Drain is called once the guest has stopped. It stops input and waits until
// every byte the guest wrote has been handed to its port's Out, or until ctx
// is done. It reports whether the console is idle, that is, no longer reading
// guest memory.
func (c *Console) Drain(ctx context.Context) bool {
	c.init()
	c.stopOnce.Do(func() { close(c.stop) })
	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// portForQueue maps a queue index to a port id and direction.
func portForQueue(q int) (id int, transmit bool) {
	if q < 2 {
		return 0, q == 1
	}
	return q/2 - 1, q%2 == 1
}

// receiveQueue is the queue on which port id gets data from the host.
func receiveQueue(id int) int { return 2 * (id + 1) }

// port returns the state of port id, or nil for port 0 and unknown ids.
func (c *Console) port(id int) *portState {
	if id < 1 || id > len(c.ports) {
		return nil
	}
	return &c.ports[id-1]
}

func (p *portState) signal() {
	select {
	case p.posted <- struct{}{}:
	default:
	}
}

func (c *Console) logf(format string, a ...any) {
	if c.Logf != nil {
		c.Logf(format, a...)
	}
}

func (c *Console) Notify(t *Transport, qi int) {
	c.init()
	switch qi {
	case ctrlTxQueue:
		c.control(t)
	case ctrlRxQueue:
		c.flush(t)
	default:
		id, transmit := portForQueue(qi)
		if transmit {
			c.transmit(t, qi, id)
		} else if p := c.port(id); p != nil {
			p.signal()
		}
	}
}

// transmit hands the chains the guest made available to the port's writer.
// Chains for a port nobody reads are returned at once.
func (c *Console) transmit(t *Transport, qi, id int) {
	q := t.Queue(qi)
	p := c.port(id)
	var out io.Writer
	if p != nil {
		out = c.Ports[id-1].Out
	}
	served := false
	for {
		ch, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		if err == nil && out != nil {
			if p.tx == nil {
				// A queue holds at most MaxQueueSize chains; the spare room
				// covers chains from before a reset still being written.
				p.tx = make(chan txChain, 2*MaxQueueSize)
				go c.write(t, p, out)
			}
			c.inflight.Add(1)
			p.tx <- txChain{c.gen, qi, ch.Head, ch.Buffers}
			continue
		}
		if err == nil {
			for _, b := range ch.Buffers {
				if !b.Writable {
					c.BytesWritten.Add(uint64(len(b.Data)))
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

// write is a port's writer. It returns chains to the driver in batches, with
// one interrupt per batch, as soon as nothing more is waiting to be written.
func (c *Console) write(t *Transport, p *portState, out io.Writer) {
	const maxBatch = 16
	var done []txChain
	for {
		var ch txChain
		if len(done) == 0 {
			select {
			case ch = <-p.tx:
			case <-c.stop:
				if len(p.tx) > 0 {
					continue
				}
				return
			}
		} else {
			select {
			case ch = <-p.tx:
			default:
				c.complete(t, done)
				done = done[:0]
				continue
			}
		}
		for _, b := range ch.bufs {
			if !b.Writable {
				out.Write(b.Data)
				c.BytesWritten.Add(uint64(len(b.Data)))
			}
		}
		done = append(done, ch)
		if len(done) == maxBatch {
			c.complete(t, done)
			done = done[:0]
		}
	}
}

// complete returns written chains, all from one queue, to the driver.
func (c *Console) complete(t *Transport, done []txChain) {
	t.Do(func() {
		pushed := false
		for _, ch := range done {
			if ch.gen != c.gen {
				continue
			}
			if q := t.Queue(ch.q); q != nil {
				q.Push(t.Memory(), ch.head, 0)
				pushed = true
			}
		}
		if pushed {
			t.Interrupt(done[0].q)
		}
	})
	for range done {
		c.inflight.Done()
	}
}

// pump sends what In yields to port id, then closes the host side of the
// port. It runs from the guest's first open of the port until In ends or the
// console stops.
func (c *Console) pump(t *Transport, id int, p *portState, in io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := in.Read(buf)
		for data := buf[:n]; len(data) > 0; {
			var sent int
			if !t.Do(func() { sent = c.deliver(t, id, p, data) }) {
				return
			}
			data = data[sent:]
			if len(data) > 0 {
				select {
				case <-p.posted:
				case <-c.stop:
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				c.logf("virtio-console: reading input for port %q: %v", c.Ports[id-1].Name, err)
			}
			// The data is in the used ring already, so the driver hands it
			// out before it reports end of file.
			t.Do(func() {
				c.send(uint32(id), ctrlPortOpen, 0, "")
				c.flush(t)
			})
			return
		}
	}
}

// deliver copies data into receive buffers the guest has posted on port id
// and returns how much of it fit. Nothing is sent to a port the guest has
// not opened, since the driver throws such data away.
func (c *Console) deliver(t *Transport, id int, p *portState, data []byte) int {
	if !p.open.Load() {
		return 0
	}
	qi := receiveQueue(id)
	q := t.Queue(qi)
	if q == nil {
		return 0
	}
	sent, served := 0, false
	for sent < len(data) {
		ch, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		var written uint32
		if err == nil {
			for _, b := range ch.Buffers {
				if b.Writable && sent < len(data) {
					n := copy(b.Data, data[sent:])
					sent += n
					written += uint32(n)
				}
			}
		}
		q.Push(t.Memory(), ch.Head, written)
		served = true
	}
	if served {
		t.Interrupt(qi)
	}
	c.BytesRead.Add(uint64(sent))
	return sent
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
					c.handle(t, le.Uint32(b.Data[0:]), le.Uint16(b.Data[4:]), le.Uint16(b.Data[6:]))
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

func (c *Console) handle(t *Transport, id uint32, event, value uint16) {
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
		p := c.port(int(id))
		if p == nil {
			return
		}
		if value != 1 {
			c.logf("virtio-console: driver failed to add port %q", c.Ports[id-1].Name)
			return
		}
		if p.ready {
			return
		}
		p.ready = true
		c.send(id, ctrlPortName, 1, c.Ports[id-1].Name)
		// The host side starts out connected. Until it says so, writes to
		// the port block in the guest, and reads return end of file.
		c.send(id, ctrlPortOpen, 1, "")
	case ctrlPortOpen:
		p := c.port(int(id))
		if p == nil {
			return
		}
		p.open.Store(value == 1)
		if in := c.Ports[id-1].In; value == 1 && in != nil && !p.pumping {
			p.pumping = true
			go c.pump(t, int(id), p, in)
		}
		p.signal()
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
