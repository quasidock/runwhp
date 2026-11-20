//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/virtio"
)

// isTerminal reports whether f is a Windows console.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// terminalSize returns the size of the console window behind f.
func terminalSize(f *os.File) (rows, cols uint16, ok bool) {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 || h <= 0 || w > 0xffff || h > 0xffff {
		return 0, 0, false
	}
	return uint16(h), uint16(w), true
}

// consoleState is console configuration to put back when runwhp is done.
type consoleState struct {
	in      *term.State
	out     windows.Handle
	outMode uint32
	outSet  bool
}

// setupConsole prepares the console for a Linux program on a pty. Raw input
// makes the keyboard send VT sequences and Ctrl+C a byte; VT processing on
// output interprets the program's escape sequences.
func setupConsole(rawInput, vtOutput bool) (*consoleState, error) {
	s := &consoleState{}
	if rawInput {
		st, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return nil, err
		}
		s.in = st
	}
	if vtOutput {
		h := windows.Handle(os.Stdout.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err == nil {
			// The pty turns \n into \r\n itself.
			vt := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
			if windows.SetConsoleMode(h, vt) == nil {
				s.out, s.outMode, s.outSet = h, mode, true
			}
		}
	}
	return s, nil
}

// restore puts the console back as it was. It is safe to call more than once.
func (s *consoleState) restore() {
	if s == nil {
		return
	}
	if s.in != nil {
		term.Restore(int(os.Stdin.Fd()), s.in)
		s.in = nil
	}
	if s.outSet {
		windows.SetConsoleMode(s.out, s.outMode)
		s.outSet = false
	}
}

// watchSize sends the console's size to the guest whenever it changes, until
// done is closed. Windows reports no resize to a program reading VT input,
// so this polls, as the Docker CLI does.
func watchSize(done <-chan struct{}, rows, cols uint16, q *controlQueue) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			r, c, ok := terminalSize(os.Stdout)
			if ok && (r != rows || c != cols) {
				rows, cols = r, c
				q.send(guestspec.Control{Rows: r, Cols: c})
			}
		}
	}
}

// consoleReader reads keyboard input from a console as UTF-8. os.Stdin also
// reads through ReadConsoleW, but it reports end of file both for Ctrl+Z,
// which on a pty is the key that suspends a job, and for a read that Ctrl+C
// interrupted, which would close the container's input instead of passing
// it the interrupt.
type consoleReader struct {
	h windows.Handle
	// lines is set when the console edits lines itself (no -t). Lines end
	// in \r\n there, which become \n, and Ctrl+Z ends the input, as is the
	// convention on Windows.
	lines bool

	u16     [1024]uint16
	pending []byte
	high    uint16 // a high surrogate waiting for its pair
	cr      bool   // a \r held back in case \n follows in the next read
	eof     bool
}

func (r *consoleReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		var n uint32
		err := windows.ReadConsole(r.h, &r.u16[0], uint32(len(r.u16)), &n, nil)
		if err == windows.ERROR_OPERATION_ABORTED {
			continue // Ctrl+C, which the signal proxy deals with
		}
		if err != nil {
			return 0, err
		}
		r.pending = r.decode(r.u16[:n])
		if r.lines {
			r.pending = r.cook(r.pending)
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// cook applies the line-mode conventions to what one read returned.
func (r *consoleReader) cook(b []byte) []byte {
	if i := bytes.IndexByte(b, 0x1a); i >= 0 {
		b, r.eof = b[:i], true
	}
	if r.cr {
		r.cr = false
		if len(b) == 0 || b[0] != '\n' {
			b = append([]byte{'\r'}, b...)
		}
	}
	if !r.eof && len(b) > 0 && b[len(b)-1] == '\r' {
		b, r.cr = b[:len(b)-1], true
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// decode converts UTF-16 to UTF-8, keeping a surrogate pair split across
// reads together.
func (r *consoleReader) decode(s []uint16) []byte {
	var out []byte
	for _, c := range s {
		switch {
		case c >= 0xd800 && c < 0xdc00:
			if r.high != 0 {
				out = utf8.AppendRune(out, utf8.RuneError)
			}
			r.high = c
		case c >= 0xdc00 && c < 0xe000 && r.high != 0:
			out = utf8.AppendRune(out, utf16.DecodeRune(rune(r.high), rune(c)))
			r.high = 0
		default:
			if r.high != 0 {
				out = utf8.AppendRune(out, utf8.RuneError)
				r.high = 0
			}
			out = utf8.AppendRune(out, rune(c))
		}
	}
	return out
}

// controlQueue is the host's side of the control port: Control messages as
// JSON lines. The console reads it only once init has opened the port, so
// send never blocks; it drops messages beyond a generous backlog.
type controlQueue struct {
	ch  chan []byte
	cur []byte
}

func newControlQueue() *controlQueue { return &controlQueue{ch: make(chan []byte, 64)} }

func (q *controlQueue) Read(p []byte) (int, error) {
	if len(q.cur) == 0 {
		q.cur = <-q.ch
	}
	n := copy(p, q.cur)
	q.cur = q.cur[n:]
	return n, nil
}

func (q *controlQueue) send(m guestspec.Control) bool {
	line, _ := json.Marshal(m)
	select {
	case q.ch <- append(line, '\n'):
		return true
	default:
		return false
	}
}

// signalProxy decides what a signal to runwhp does. Once init listens on the
// control port, the first interrupt (Ctrl+C or Ctrl+Break) goes to the
// container's process group as SIGINT, as a terminal's Ctrl+C would, and a
// second one stops the VM. SIGTERM, which is what Go reports when the console
// closes or Windows logs off or shuts down, goes to the main process, as
// docker stop sends it, and the VM stops 10 s later if it is still running.
// Before init listens, any signal stops the VM at once.
type signalProxy struct {
	stop    context.CancelFunc
	queue   *controlQueue
	console atomic.Pointer[virtio.Console]
	logf    func(format string, a ...any)

	interrupts int
}

func (p *signalProxy) run(sigs <-chan os.Signal) {
	for sig := range sigs {
		p.handle(sig)
	}
}

func (p *signalProxy) handle(sig os.Signal) {
	con := p.console.Load()
	if con == nil || !con.Connected(guestspec.PortControl) {
		p.stop()
		return
	}
	switch sig {
	case os.Interrupt:
		p.interrupts++
		if p.interrupts > 1 {
			p.logf("second interrupt: stopping the VM")
			p.stop()
			return
		}
		p.logf("passing the interrupt on to the container as SIGINT")
		p.queue.send(guestspec.Control{Signal: int(syscall.SIGINT), Group: true})
	default:
		p.logf("passing %v on to the container", sig)
		p.queue.send(guestspec.Control{Signal: int(syscall.SIGTERM)})
		time.AfterFunc(10*time.Second, p.stop)
	}
}
