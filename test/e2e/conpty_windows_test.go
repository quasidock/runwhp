//go:build windows

package e2e

import (
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pty runs runwhp on a Windows pseudo console, the way a terminal would.
type pty struct {
	t      *testing.T
	hpc    windows.Handle
	keys   *os.File
	proc   windows.Handle
	output syncBuffer
	done   chan struct{} // closed when the console's output ends
}

func startPTY(t *testing.T, rows, cols int16, args ...string) *pty {
	t.Helper()
	exe := runwhp(t)
	// A process started in the background often has Ctrl+C turned off, and
	// children inherit that. A terminal's child would have it on.
	syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler").Call(0, 0)
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &pty{t: t, keys: inW, done: make(chan struct{})}
	if err := windows.CreatePseudoConsole(windows.Coord{X: cols, Y: rows}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &p.hpc); err != nil {
		t.Skipf("no pseudo console here: %v", err)
	}
	// The pseudo console has its own copies.
	inR.Close()
	outW.Close()
	go func() {
		io.Copy(&p.output, outR)
		close(p.done)
	}()

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&p.hpc)), unsafe.Sizeof(p.hpc)); err != nil {
		t.Fatal(err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// Null standard handles, so the child uses the pseudo console rather
	// than the pipes go test gave us.
	si.Flags = windows.STARTF_USESTDHANDLES
	cmdline := syscall.EscapeArg(exe) + " run"
	for _, a := range args {
		cmdline += " " + syscall.EscapeArg(a)
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, windows.StringToUTF16Ptr(cmdline), nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, &pi); err != nil {
		windows.ClosePseudoConsole(p.hpc)
		t.Fatal(err)
	}
	windows.CloseHandle(pi.Thread)
	p.proc = pi.Process
	t.Cleanup(p.close)
	return p
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()=>][0-9A-Za-z]?`)

// text is the output so far without escape sequences.
func (p *pty) text() string { return ansi.ReplaceAllString(p.output.String(), "") }

func (p *pty) waitFor(s string) {
	p.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(p.text(), s); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			p.t.Fatalf("no %q in the terminal; it shows %q", s, p.text())
		}
	}
}

func (p *pty) typeKeys(s string) {
	p.t.Helper()
	if _, err := io.WriteString(p.keys, s); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pty) wait() int {
	p.t.Helper()
	if ev, _ := windows.WaitForSingleObject(p.proc, 20000); ev != windows.WAIT_OBJECT_0 {
		p.t.Fatalf("runwhp did not exit; the terminal shows %q", p.text())
	}
	var code uint32
	windows.GetExitCodeProcess(p.proc, &code)
	return int(code)
}

func (p *pty) close() {
	windows.TerminateProcess(p.proc, 1)
	p.keys.Close()
	// ClosePseudoConsole waits until its output has been read.
	windows.ClosePseudoConsole(p.hpc)
	<-p.done
	windows.CloseHandle(p.proc)
}

func TestTTY(t *testing.T) {
	p := startPTY(t, 24, 80, "-it", alpine, "sh")
	p.waitFor("/ #") // the pseudo console leaves out the trailing space
	// After its prompt, busybox's line editor asks where the cursor is. If
	// the answer and the first keys reach it in one read, it discards them
	// together as one unknown escape sequence, so let the answer go first.
	time.Sleep(300 * time.Millisecond)
	p.typeKeys("tty; stty size; echo @@$((6*7))@@\r")
	p.waitFor("@@42@@")
	text := p.text()
	if !strings.Contains(text, "/dev/pts/0") || !strings.Contains(text, "24 80") {
		t.Errorf("tty or size wrong; the terminal shows %q", text)
	}

	if err := windows.ResizePseudoConsole(p.hpc, windows.Coord{X: 120, Y: 40}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // runwhp polls the size every 250 ms
	p.typeKeys("stty size; echo @@$TERM@@\r")
	p.waitFor("@@xterm@@")
	if !strings.Contains(p.text(), "40 120") {
		t.Errorf("size after resize wrong; the terminal shows %q", p.text())
	}

	p.typeKeys("exit 3\r")
	if code := p.wait(); code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
}

// TestTTYInterrupt sends Ctrl+C as a key, which the guest's terminal turns
// into SIGINT for the foreground process group.
func TestTTYInterrupt(t *testing.T) {
	p := startPTY(t, 24, 80, "-it", alpine, "sh", "-c", "echo ready; sleep 30")
	p.waitFor("ready")
	start := time.Now()
	p.typeKeys("\x03")
	if code := p.wait(); code != 130 {
		t.Errorf("exit = %d, want 130", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

// TestTTYNeedsConsole checks the error for -it without a console.
func TestTTYNeedsConsole(t *testing.T) {
	r := runWithInput(t, strings.NewReader(""), "-it", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "not a TTY") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestInteractiveConsoleInterrupt runs -i without -t on a console, where the
// console edits lines itself and Ctrl+C is a signal to runwhp. The signal
// must reach the container rather than end its input.
func TestInteractiveConsoleInterrupt(t *testing.T) {
	p := startPTY(t, 24, 80, "-i", alpine, "sh", "-c", "echo ready; cat; echo cat-ended $?")
	p.waitFor("ready")
	p.typeKeys("abc\r")
	p.waitFor("abc\r\nabc")
	p.typeKeys("\x03")
	if code := p.wait(); code != 130 {
		t.Errorf("exit = %d, want 130", code)
	}
	if strings.Contains(p.text(), "cat-ended") {
		t.Errorf("cat saw end of input; the terminal shows %q", p.text())
	}
}
