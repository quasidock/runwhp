//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
)

func TestParseMemory(t *testing.T) {
	want := map[string]uint64{
		"512m": 512 << 20, "1g": 1 << 30, "1.5g": 1536 << 20, "300M": 300 << 20,
		"200000k":   196 << 20, // rounded up to 2 MiB
		"134217728": 128 << 20,
	}
	maxMem := vm.MaxMemory()
	if maxMem >= 4<<30 {
		want["4g"] = 4 << 30 // above the device windows
	}
	want[fmt.Sprintf("%dm", maxMem>>20)] = maxMem
	for in, want := range want {
		got, err := parseMemory(in)
		if err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1g", "64m", "512", fmt.Sprintf("%dm", maxMem>>20+2)} {
		if _, err := parseMemory(in); err == nil {
			t.Errorf("parseMemory(%q) accepted", in)
		}
	}
}

func TestParseCPUs(t *testing.T) {
	for _, c := range []struct {
		in         string
		host, want int
	}{
		{"", 2, 2}, {"", 16, 4}, {"", 1, 1}, {"1", 16, 1}, {" 4 ", 16, 4}, {"16", 16, 16}, {"64", 128, 64},
	} {
		got, err := parseCPUs(c.in, c.host)
		if err != nil || got != c.want {
			t.Errorf("parseCPUs(%q, %d) = %d, %v; want %d", c.in, c.host, got, err, c.want)
		}
	}
	for _, c := range []struct {
		in, msg string
		host    int
	}{
		{"0", "between 1 and 16, the logical processors", 16},
		{"-1", "between 1 and 16", 16},
		{"17", "between 1 and 16", 16},
		{"65", "between 1 and 64", 128},
		{"1.5", "whole number", 16},
		{"2.0", "whole number", 16},
		{"abc", "invalid --cpus", 16},
	} {
		if _, err := parseCPUs(c.in, c.host); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("parseCPUs(%q, %d): %v; want an error saying %q", c.in, c.host, err, c.msg)
		}
	}
}

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

func TestParsePublish(t *testing.T) {
	for in, want := range map[string]string{
		"8080:80":             "127.0.0.1:8080->80/tcp",
		"8080:80/tcp":         "127.0.0.1:8080->80/tcp",
		"0.0.0.0:8080:80":     "0.0.0.0:8080->80/tcp",
		"192.168.1.5:443:443": "192.168.1.5:443->443/tcp",
		"[::1]:8080:80":       "[::1]:8080->80/tcp",
		":8080:80":            "127.0.0.1:8080->80/tcp",
	} {
		got, err := parsePublish(in)
		if err != nil || got.String() != want {
			t.Errorf("parsePublish(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"80":              "random host ports",
		"127.0.0.1::80":   "random host ports",
		"53:53/udp":       "only tcp",
		"8000-8010:80":    "ranges",
		"8080:0":          "not a port",
		"8080:70000":      "not a port",
		"localhost:80:80": "not an IP address",
	} {
		if _, err := parsePublish(in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("parsePublish(%q) error = %v, want one mentioning %q", in, err, msg)
		}
	}
}

func TestParseAddHost(t *testing.T) {
	for in, want := range map[string]string{
		"db:192.168.1.20":                   "192.168.1.20\tdb",
		"db=192.168.1.20":                   "192.168.1.20\tdb",
		"v6:::1":                            "::1\tv6",
		"v6:[2001:db8::1]":                  "2001:db8::1\tv6",
		"host.docker.internal:host-gateway": "10.0.2.2\thost.docker.internal",
	} {
		line, gw, err := parseAddHost(in)
		if err != nil || line != want || gw != strings.HasSuffix(in, "host-gateway") {
			t.Errorf("parseAddHost(%q) = %q, %v, %v; want %q", in, line, gw, err, want)
		}
	}
	for _, in := range []string{"nocolon", ":1.2.3.4", "db:not-an-ip", "a b:1.2.3.4"} {
		if _, _, err := parseAddHost(in); err == nil {
			t.Errorf("parseAddHost(%q) accepted", in)
		}
	}
}

func TestParseNetworkFlags(t *testing.T) {
	n, err := parseNetworkFlags("user", []string{"8080:80"}, []string{"h:host-gateway", "x:1.2.3.4"})
	if err != nil || !n.enabled || !n.hostGateway || len(n.publish) != 1 || len(n.hosts) != 2 {
		t.Fatalf("user: %+v, %v", n, err)
	}
	if s := n.spec(); s == nil || s.MAC != "02:00:00:00:00:0f" || s.Address != "10.0.2.15/24" ||
		s.Gateway != "10.0.2.2" || len(s.DNS) != 1 || s.DNS[0] != "10.0.2.3" || s.MTU != 65520 {
		t.Errorf("spec = %+v", s)
	}
	n, err = parseNetworkFlags("none", nil, []string{"x:1.2.3.4"})
	if err != nil || n.enabled || n.spec() != nil || len(n.hosts) != 1 {
		t.Errorf("none: %+v, %v", n, err)
	}
	for _, c := range []struct {
		mode           string
		publish, hosts []string
	}{
		{"host", nil, nil},
		{"bridge", nil, nil},
		{"none", []string{"8080:80"}, nil},
		{"none", nil, []string{"h:host-gateway"}},
		{"user", []string{"bad"}, nil},
	} {
		if _, err := parseNetworkFlags(c.mode, c.publish, c.hosts); err == nil {
			t.Errorf("parseNetworkFlags(%q, %q, %q) accepted", c.mode, c.publish, c.hosts)
		}
	}
}

func TestNetworkFlags(t *testing.T) {
	var o runOptions
	fs := runFlags(&o)
	fs.SetOutput(io.Discard)
	args := []string{"--net", "none", "-p", "1:2", "--publish", "3:4", "--add-host", "a:1.1.1.1", "alpine"}
	if err := fs.Parse(args); err != nil || o.network != "none" || len(o.publish) != 2 || len(o.addHosts) != 1 {
		t.Errorf("parsed %+v, %v", o, err)
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

func TestResolveKernel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("RUNWHP_KERNEL", `C:\env\bzImage`)
	if p, err := resolveKernel(ctx, root, explicitKernel(`C:\flag\bzImage`), image.PullMissing); err != nil || p != `C:\flag\bzImage` {
		t.Errorf("--kernel: %q, %v", p, err)
	}
	if p, err := resolveKernel(ctx, root, explicitKernel(""), image.PullMissing); err != nil || p != `C:\env\bzImage` {
		t.Errorf("RUNWHP_KERNEL: %q, %v", p, err)
	}

	t.Setenv("RUNWHP_KERNEL", "")
	if kernel.Default.Digest == "" {
		t.Skip("no kernel is pinned")
	}
	// A stored kernel is used without any network access, whatever the
	// policy; a missing one is not fetched under --pull never.
	_, err := resolveKernel(ctx, root, explicitKernel(""), image.PullNever)
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
		if p, err := resolveKernel(ctx, root, explicitKernel(""), policy); err != nil || p != stored {
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
