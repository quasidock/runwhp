//go:build windows

package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/virtio"
)

func TestEventWriter(t *testing.T) {
	var started []int
	w := eventWriter{onStarted: func(pid int) { started = append(started, pid) }}
	if _, ok := w.get(); ok {
		t.Fatal("status before any write")
	}
	w.Write([]byte(`{"started":true,"pid":4`))
	if len(started) != 0 {
		t.Fatal("started from a partial line")
	}
	w.Write([]byte("2}\n{\"exited\":true,\"exit\":12"))
	if _, ok := w.get(); ok || len(started) != 1 || started[0] != 42 {
		t.Fatalf("after the start: started %v", started)
	}
	w.Write([]byte("7,\"error\":\"boom\"}\nnot json\n"))
	st, ok := w.get()
	if !ok || st.Exit != 127 || st.Error != "boom" {
		t.Errorf("status = %+v, %v", st, ok)
	}
	// An exit of 0 has no exit field.
	w = eventWriter{}
	w.Write([]byte("{\"exited\":true}\n"))
	if st, ok := w.get(); !ok || st.Exit != 0 {
		t.Errorf("exit 0: %+v, %v", st, ok)
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
		t.Errorf("Ring printed %q", got)
	}
}

func TestConsoleReaderDecode(t *testing.T) {
	var r ConsoleReader
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
	r := ConsoleReader{lines: true}
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

// TestControlQueueTime checks that the time is taken when the console reads
// the message, not when it was queued.
func TestControlQueueTime(t *testing.T) {
	q := newControlQueue()
	q.sendTime()
	time.Sleep(50 * time.Millisecond)
	before := time.Now()
	buf := make([]byte, 128)
	n, _ := q.Read(buf)
	after := time.Now()
	var m guestspec.Control
	if err := json.Unmarshal(buf[:n], &m); err != nil || buf[n-1] != '\n' {
		t.Fatalf("read %q: %v", buf[:n], err)
	}
	if got := time.Unix(0, m.Time); got.Before(before.Truncate(time.Microsecond)) || got.After(after) {
		t.Errorf("time %v, read between %v and %v", got, before, after)
	}
}

// TestSignalProxyBeforeInit checks that a signal stops the VM at once while
// init cannot take it yet.
func TestSignalProxyBeforeInit(t *testing.T) {
	stopped := 0
	p := &SignalProxy{stop: func() { stopped++ }, queue: newControlQueue(), logf: t.Logf}
	p.handle(os.Interrupt)
	p.console.Store(&virtio.Console{Ports: []virtio.Port{{Name: guestspec.PortControl}}})
	p.handle(os.Interrupt)
	if stopped != 2 || len(p.queue.ch) != 0 {
		t.Errorf("stopped %d times, %d messages queued", stopped, len(p.queue.ch))
	}
}

func TestResolveKernel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("RUNWHP_KERNEL", `C:\env\bzImage`)
	if p, err := ResolveKernel(ctx, root, ExplicitKernel(`C:\flag\bzImage`), image.PullMissing); err != nil || p != `C:\flag\bzImage` {
		t.Errorf("--kernel: %q, %v", p, err)
	}
	if p, err := ResolveKernel(ctx, root, ExplicitKernel(""), image.PullMissing); err != nil || p != `C:\env\bzImage` {
		t.Errorf("RUNWHP_KERNEL: %q, %v", p, err)
	}

	t.Setenv("RUNWHP_KERNEL", "")
	if kernel.Default.Digest == "" {
		t.Skip("no kernel is pinned")
	}
	// A stored kernel is used without any network access, whatever the
	// policy; a missing one is not fetched under --pull never.
	_, err := ResolveKernel(ctx, root, ExplicitKernel(""), image.PullNever)
	if err == nil || !strings.Contains(err.Error(), "runwhp pull") {
		t.Errorf("missing kernel under --pull never: %v", err)
	}
	stored, err := kernel.Open(root).Path(kernel.Default)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("bzImage"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []image.Policy{image.PullNever, image.PullMissing, image.PullAlways} {
		if p, err := ResolveKernel(ctx, root, ExplicitKernel(""), policy); err != nil || p != stored {
			t.Errorf("stored kernel, --pull %s: %q, %v", policy, p, err)
		}
	}
}

func TestReadModules(t *testing.T) {
	if files, err := readModules(nil); err != nil || files != nil {
		t.Errorf("no modules: %v, %v", files, err)
	}
	ko := filepath.Join(t.TempDir(), "virtio_blk.ko")
	os.WriteFile(ko, []byte("module"), 0o644)
	files, err := readModules([]string{ko})
	if err != nil || len(files) != 2 || files[0].Name != "modules" || files[1].Name != "modules/virtio_blk.ko" || string(files[1].Data) != "module" {
		t.Errorf("readModules: %+v, %v", files, err)
	}
	if _, err := readModules([]string{ko + ".missing"}); err == nil {
		t.Error("read a missing module")
	}
}

func TestReclaimPolicy(t *testing.T) {
	logf := func(string, ...any) {}
	for _, c := range []struct {
		env            string
		set            bool
		idle, interval int
	}{
		{set: false, idle: 60, interval: 5},
		{env: "0", set: true, idle: 0, interval: 1},
		{env: "2", set: true, idle: 2, interval: 2},
		{env: "300", set: true, idle: 300, interval: 5},
		{env: "soon", set: true, idle: 60, interval: 5},
	} {
		if c.set {
			t.Setenv("RUNWHP_RECLAIM_IDLE", c.env)
		} else {
			os.Unsetenv("RUNWHP_RECLAIM_IDLE")
		}
		if r := reclaimPolicy(logf); r.IdleSeconds != c.idle || r.IntervalSeconds != c.interval {
			t.Errorf("RUNWHP_RECLAIM_IDLE=%q: %+v", c.env, r)
		}
	}
}

func TestClockPeriod(t *testing.T) {
	logf := func(string, ...any) {}
	for _, c := range []struct {
		env  string
		set  bool
		want time.Duration
	}{
		{set: false, want: time.Minute},
		{env: "0", set: true, want: 0},
		{env: "1", set: true, want: time.Second},
		{env: "-3", set: true, want: time.Minute},
		{env: "soon", set: true, want: time.Minute},
	} {
		if c.set {
			t.Setenv("RUNWHP_CLOCK_SYNC", c.env)
		} else {
			os.Unsetenv("RUNWHP_CLOCK_SYNC")
		}
		if got := clockPeriod(logf); got != c.want {
			t.Errorf("RUNWHP_CLOCK_SYNC=%q: %v, want %v", c.env, got, c.want)
		}
	}
}
