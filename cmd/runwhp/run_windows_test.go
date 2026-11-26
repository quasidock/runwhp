//go:build windows

package main

import (
	"fmt"
	"io"
	"strings"
	"testing"

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
	want[""] = max(maxMem/2&^(2<<20-1), 512<<20)
	for in, want := range want {
		got, err := parseMemory(in)
		if err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "-1g", "64m", "512", fmt.Sprintf("%dm", maxMem>>20+2)} {
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

func TestParseNetworkFlags(t *testing.T) {
	n, err := parseNetworkFlags("user", []string{"8080:80"}, []string{"h:host-gateway", "x:1.2.3.4"})
	if err != nil || !n.enabled || !n.hostGateway || len(n.publish) != 1 || len(n.hosts) != 2 {
		t.Fatalf("user: %+v, %v", n, err)
	}
	n, err = parseNetworkFlags("none", nil, []string{"x:1.2.3.4"})
	if err != nil || n.enabled || len(n.hosts) != 1 {
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
