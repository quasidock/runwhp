//go:build windows

package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/virtio"
)

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]uint64{
		"512m": 512 << 20, "1g": 1 << 30, "1.5g": 1536 << 20, "300M": 300 << 20,
		"200000k":   196 << 20, // rounded up to 2 MiB
		"134217728": 128 << 20,
	} {
		got, err := parseMemory(in)
		if err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1g", "64m", "4g", "512"} {
		if _, err := parseMemory(in); err == nil {
			t.Errorf("parseMemory(%q) accepted", in)
		}
	}
}

func TestStatusWriter(t *testing.T) {
	var w statusWriter
	if _, ok := w.get(); ok {
		t.Fatal("status before any write")
	}
	w.Write([]byte(`{"exit":12`))
	if _, ok := w.get(); ok {
		t.Fatal("status from a partial line")
	}
	w.Write([]byte("7,\"error\":\"boom\"}\nnot json\n"))
	st, ok := w.get()
	if !ok || st.Exit != 127 || st.Error != "boom" {
		t.Errorf("status = %+v, %v", st, ok)
	}
}

func TestRing(t *testing.T) {
	r := &ring{max: 3}
	for _, l := range []string{"a", "b", "c", "d"} {
		r.add(l)
	}
	var buf bytes.Buffer
	r.print(&buf)
	if got := buf.String(); !strings.HasSuffix(got, "  b\n  c\n  d\n") || strings.Contains(got, "  a\n") {
		t.Errorf("ring printed %q", got)
	}
}

func TestInteractiveFlags(t *testing.T) {
	for _, args := range [][]string{{"-it"}, {"-ti"}, {"-i", "-t"}, {"--interactive", "--tty"}} {
		var o runOptions
		fs := runFlags(&o)
		fs.SetOutput(io.Discard)
		if err := fs.Parse(append(args, "alpine", "sh")); err != nil || !o.interactive || !o.tty || fs.Arg(0) != "alpine" {
			t.Errorf("%q: err %v, interactive %v, tty %v, args %q", args, err, o.interactive, o.tty, fs.Args())
		}
	}
}

func TestConsoleReaderDecode(t *testing.T) {
	var r consoleReader
	// U+1F600 is the pair D83D DE00; here it is split across two reads.
	got := string(r.decode([]uint16{'a', 0xd83d})) + string(r.decode([]uint16{0xde00, 'b', 0x1a}))
	if got != "a\U0001F600b\x1a" {
		t.Errorf("decoded %q", got)
	}
	if got := string(r.decode([]uint16{0xd800, 'x'})); got != "\uFFFDx" {
		t.Errorf("unpaired surrogate decoded as %q", got)
	}
}

func TestConsoleReaderLines(t *testing.T) {
	r := consoleReader{lines: true}
	var got []byte
	for _, s := range []string{"ab\r\n", "c\r", "\nd\re\r", "f\r\n\x1aignored"} {
		got = append(got, r.cook([]byte(s))...)
	}
	if string(got) != "ab\nc\nd\re\rf\n" || !r.eof {
		t.Errorf("cooked %q, eof %v", got, r.eof)
	}
}

func TestControlQueue(t *testing.T) {
	q := newControlQueue()
	q.send(guestspec.Control{Signal: 2})
	q.send(guestspec.Control{Rows: 40, Cols: 120})
	const want = "{\"signal\":2}\n{\"rows\":40,\"cols\":120}\n"
	var got []byte
	buf := make([]byte, 5)
	for len(got) < len(want) {
		n, _ := q.Read(buf)
		got = append(got, buf[:n]...)
	}
	if string(got) != want {
		t.Errorf("read %q, want %q", got, want)
	}
}

// TestSignalProxyBeforeInit checks that a signal stops the VM at once while
// init cannot take it yet.
func TestSignalProxyBeforeInit(t *testing.T) {
	stopped := 0
	p := &signalProxy{stop: func() { stopped++ }, queue: newControlQueue(), logf: t.Logf}
	p.handle(os.Interrupt)
	p.console.Store(&virtio.Console{Ports: []virtio.Port{{Name: guestspec.PortControl}}})
	p.handle(os.Interrupt)
	if stopped != 2 || len(p.queue.ch) != 0 {
		t.Errorf("stopped %d times, %d messages queued", stopped, len(p.queue.ch))
	}
}
