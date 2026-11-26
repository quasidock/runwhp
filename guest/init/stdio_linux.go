//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// stdio connects the container process to the console ports, through pipes
// or, with a terminal, through a pseudo-terminal. The process never gets a
// port itself: a port can be open only once at a time, so the process could
// not reopen /dev/stdout or /dev/stdin.
type stdio struct {
	child           []*os.File // the process's fds 0, 1 and 2
	closeAfterStart []*os.File // init's copies of the process's ends
	master          *os.File   // the pty master, with a terminal
	outputs         sync.WaitGroup
}

// newStdio sets up the process's standard streams and starts relaying them.
// uid owns the terminal, if there is one.
func newStdio(spec *guestspec.Spec, ports map[string]*os.File, uid uint32) (*stdio, error) {
	s := &stdio{}
	stdin := ports[guestspec.PortStdin] // nil unless spec.Stdin
	if spec.TTY {
		master, slave, err := openPTY(filepath.Join(root, "dev/pts"))
		if err != nil {
			return nil, fmt.Errorf("creating the terminal: %w", err)
		}
		// Like login does: the user owns the terminal, group tty.
		if err := slave.Chown(int(uid), 5); err != nil {
			debugf("chown of the terminal: %v", err)
		}
		if spec.Rows > 0 && spec.Cols > 0 {
			if err := setWinsize(master, spec.Rows, spec.Cols); err != nil {
				logf("setting the terminal size: %v", err)
			}
		}
		s.master = master
		s.child = []*os.File{slave, slave, slave}
		s.closeAfterStart = []*os.File{slave}
		s.output(ports[guestspec.PortStdout], master)
		if stdin != nil {
			go relay(master, stdin)
		}
		return s, nil
	}

	if stdin != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.child = append(s.child, r)
		s.closeAfterStart = append(s.closeAfterStart, r)
		go func() {
			relay(w, stdin)
			w.Close()
		}()
	} else {
		devnull, err := os.Open("/dev/null")
		if err != nil {
			return nil, err
		}
		s.child = append(s.child, devnull)
		s.closeAfterStart = append(s.closeAfterStart, devnull)
	}
	for _, name := range []string{guestspec.PortStdout, guestspec.PortStderr} {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.child = append(s.child, w)
		s.closeAfterStart = append(s.closeAfterStart, w)
		s.output(ports[name], r)
	}
	return s, nil
}

func (s *stdio) fds() []uintptr {
	fds := make([]uintptr, len(s.child))
	for i, f := range s.child {
		fds[i] = f.Fd()
	}
	return fds
}

func (s *stdio) output(port, src *os.File) {
	s.outputs.Add(1)
	go func() {
		defer s.outputs.Done()
		relay(port, src)
	}()
}

// started closes init's copies of the process's ends, so that the output
// relays reach end of file once every process holding them is gone.
func (s *stdio) started() {
	for _, f := range s.closeAfterStart {
		f.Close()
	}
}

// wait waits up to limit for the output relays to pass on what is left.
func (s *stdio) wait(limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.outputs.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// relay copies src to dst until src ends: end of file for a pipe or a port,
// EIO for a pty master whose last slave is closed. If dst fails, it keeps
// reading and drops the data, so that the writer on the other side of src is
// not left blocked. Reads of up to 64 KiB, a pipe's capacity, turn many
// small writes by the process into few writes to the port.
func relay(dst, src *os.File) {
	buf := make([]byte, 64<<10)
	failed := false
	for {
		n, err := src.Read(buf)
		if n > 0 && !failed {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				debugf("relay to %s: %v", dst.Name(), werr)
				failed = true
			}
		}
		if err != nil {
			return
		}
	}
}

// openPTY creates a pseudo-terminal in the devpts instance mounted at pts.
func openPTY(pts string) (master, slave *os.File, err error) {
	master, err = os.OpenFile(filepath.Join(pts, "ptmx"), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	var n uint32
	if err = ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err == nil {
		err = ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&n))
	}
	if err == nil {
		slave, err = os.OpenFile(filepath.Join(pts, strconv.Itoa(int(n))), os.O_RDWR|syscall.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWinsize(f *os.File, rows, cols uint16) error {
	ws := struct{ row, col, xpixel, ypixel uint16 }{rows, cols, 0, 0}
	return ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

// ioctl leaves f in non-blocking mode, which f.Fd() would not.
func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// controller applies the host's Control messages: signals for the container
// process or its process group, held until it has started, and terminal
// sizes. It also sends init's Events to the host.
type controller struct {
	mu         sync.Mutex
	pid        int
	started    bool
	held       []guestspec.Control
	master     *os.File
	rows, cols uint16 // the latest size, for a terminal not made yet

	sendMu sync.Mutex
	port   *os.File // the control port, once open

	// mem is where statistics come from, once init has opened its files.
	mem atomic.Pointer[memFiles]

	// With Spec.WaitStart: whether the host has asked to start the
	// process, the first signal it sent before that, and a wake-up for
	// waitStart.
	waitMode  bool
	startReq  bool
	preSignal int
	wake      chan struct{}

	// clockSet is closed once the clock has been set from the host's time
	// the first time.
	clockSet  chan struct{}
	clockOnce sync.Once

	// shutdown is closed once the host has asked a guest serving files to
	// end.
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

func newController(waitMode bool) *controller {
	return &controller{waitMode: waitMode, wake: make(chan struct{}, 1), clockSet: make(chan struct{}), shutdown: make(chan struct{})}
}

// waitShutdown waits for the host to ask a guest serving files to end, or
// for the control port to close.
func (c *controller) waitShutdown() {
	<-c.shutdown
}

// waitClock waits up to limit for the host's first time to be applied,
// and reports how long it waited and whether it came.
func (c *controller) waitClock(limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	select {
	case <-c.clockSet:
		return 0, true
	default:
	}
	select {
	case <-c.clockSet:
		return time.Since(start), true
	case <-time.After(limit):
		return limit, false
	}
}

// waitStart waits for the host to start the process. It returns 0, or the
// number of a signal the host sent first.
func (c *controller) waitStart() int {
	for {
		c.mu.Lock()
		sig, start := c.preSignal, c.startReq
		c.mu.Unlock()
		if sig != 0 {
			return sig
		}
		if start {
			return 0
		}
		<-c.wake
	}
}

func (c *controller) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// setPort gives send the control port.
func (c *controller) setPort(port *os.File) {
	c.sendMu.Lock()
	c.port = port
	c.sendMu.Unlock()
}

// send writes an Event to the host, if the control port is open.
func (c *controller) send(ev guestspec.Event) {
	line, _ := json.Marshal(ev)
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.port == nil {
		return
	}
	if _, err := c.port.Write(append(line, '\n')); err != nil {
		logf("sending %s: %v", line, err)
	}
}

// listen reads Control messages from the control port until it closes.
func (c *controller) listen(port *os.File) {
	sc := bufio.NewScanner(port)
	for sc.Scan() {
		var m guestspec.Control
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			logf("bad control message %q: %v", sc.Bytes(), err)
			continue
		}
		if m.Signal != 0 {
			c.signal(m)
		}
		if m.Start {
			c.mu.Lock()
			c.startReq = true
			c.mu.Unlock()
			c.notify()
		}
		if m.Rows > 0 && m.Cols > 0 {
			c.resize(m.Rows, m.Cols)
		}
		if m.Time != 0 {
			setClock(m.Time)
			c.clockOnce.Do(func() { close(c.clockSet) })
		}
		if m.Stats != 0 {
			// Walking /proc takes a while; signals behind it should not wait.
			go c.sendStats(m.Stats)
		}
		if m.Top != 0 {
			go c.sendTop(m.Top)
		}
		if m.Shutdown {
			c.shutdownOnce.Do(func() { close(c.shutdown) })
		}
		if f := m.Freeze; f != nil {
			// The kernel takes a while to freeze the processes. Signals sent
			// after a thaw, as kill sends them, wait for nothing either way:
			// a frozen process takes them once it thaws.
			go func() {
				reply := &guestspec.Freeze{Seq: f.Seq, Frozen: f.Frozen}
				if err := cgroup.setFrozen(f.Frozen); err != nil {
					reply.Error = err.Error()
				}
				c.send(guestspec.Event{Freeze: reply})
			}()
		}
	}
	debugf("control port closed: %v", sc.Err())
	// No host is left to ask a guest serving files to end.
	c.shutdownOnce.Do(func() { close(c.shutdown) })
}

// sendStats answers a request for statistics, with as much as init can
// read.
func (c *controller) sendStats(seq uint64) {
	st := &guestspec.GuestStats{Seq: seq}
	if mem := c.mem.Load(); mem != nil {
		if data, err := read(mem.meminfo); err == nil {
			f := parseMeminfo(data)
			st.MemTotal, st.MemAvailable, st.Cached = f["MemTotal"], f["MemAvailable"], f["Cached"]
		}
		st.Tasks = mem.tasks()
	}
	c.send(guestspec.Event{Stats: st})
}

func (c *controller) signal(m guestspec.Control) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.started && c.waitMode:
		if c.preSignal == 0 {
			c.preSignal = m.Signal
		}
		c.notify()
	case !c.started:
		c.held = append(c.held, m)
	case c.pid > 0:
		// The process leads its own session, so its process group has
		// its pid. -1 is every process but init.
		target, sig := c.pid, syscall.Signal(m.Signal)
		switch {
		case m.All:
			target = -1
		case m.Group:
			target = -c.pid
		}
		debugf("sending %v to %d", sig, target)
		if err := syscall.Kill(target, sig); err != nil {
			debugf("signal %v: %v", sig, err)
		}
	}
}

func (c *controller) resize(rows, cols uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows, c.cols = rows, cols
	if c.master != nil {
		if err := setWinsize(c.master, rows, cols); err != nil {
			logf("resizing the terminal: %v", err)
		}
	}
}

// setTerminal gives resizes a terminal to act on, and applies a size that
// arrived while init was still setting up.
func (c *controller) setTerminal(master *os.File) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.master = master
	if master != nil && c.rows > 0 && c.cols > 0 {
		setWinsize(master, c.rows, c.cols)
	}
}

// start records the process and delivers the signals held for it.
func (c *controller) start(pid int) {
	c.mu.Lock()
	c.pid, c.started = pid, true
	held := c.held
	c.held = nil
	c.mu.Unlock()
	for _, m := range held {
		c.signal(m)
	}
}

// exited stops signals from reaching a pid that may be reused.
func (c *controller) exited() {
	c.mu.Lock()
	c.pid = 0
	c.mu.Unlock()
}
