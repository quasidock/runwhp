//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/network"
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
		"80":                  "127.0.0.1:0->80/tcp",
		"80/tcp":              "127.0.0.1:0->80/tcp",
		"127.0.0.1::80":       "127.0.0.1:0->80/tcp",
		"0.0.0.0::80":         "0.0.0.0:0->80/tcp",
		"0:80":                "127.0.0.1:0->80/tcp",
		"[::1]::80":           "[::1]:0->80/tcp",
		"53:53/udp":           "127.0.0.1:53->53/udp",
		"53/udp":              "127.0.0.1:0->53/udp",
		"0.0.0.0:5353:53/UDP": "0.0.0.0:5353->53/udp",
	} {
		got, err := parsePublish(in)
		if err != nil || got.String() != want {
			t.Errorf("parsePublish(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"9:9/sctp":        "sctp ports cannot be published",
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

func TestExposeAndPublishAll(t *testing.T) {
	for in, want := range map[string][]string{
		"80":        {"80/tcp"},
		"53/udp":    {"53/udp"},
		"8000-8002": {"8000/tcp", "8001/tcp", "8002/tcp"},
	} {
		got, err := parseExpose(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseExpose(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "80/xyz", "90-80", "80-x"} {
		if _, err := parseExpose(in); err == nil {
			t.Errorf("parseExpose(%q) was taken", in)
		}
	}

	pub := func(s string) netstack.Port {
		p, err := parsePublish(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	image := map[string]struct{}{"6379/tcp": {}, "53/udp": {}, "9000": {}, "132/sctp": {}}
	published := []netstack.Port{pub("8080:80"), pub("5000:5000/udp")}
	exposed := exposedPorts(image, []string{"443/tcp", "6379/tcp", "53/tcp"}, published)
	if want := []string{"53/tcp", "53/udp", "80/tcp", "132/sctp", "443/tcp", "5000/udp", "6379/tcp", "9000/tcp"}; !reflect.DeepEqual(exposed, want) {
		t.Errorf("exposed %q, want %q", exposed, want)
	}
	more, skipped := publishAll(exposed, published)
	var got []string
	for _, p := range more {
		got = append(got, p.String())
	}
	if want := []string{"127.0.0.1:0->53/tcp", "127.0.0.1:0->53/udp", "127.0.0.1:0->443/tcp", "127.0.0.1:0->6379/tcp", "127.0.0.1:0->9000/tcp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-P published %q, want %q", got, want)
	}
	if !reflect.DeepEqual(skipped, []string{"132/sctp"}) {
		t.Errorf("-P skipped %q", skipped)
	}

	// The published ports while a container runs are those bound; before
	// and after, those asked for.
	cfg := &container.Config{Publish: []netstack.Port{pub("80")}}
	bound := []netstack.Port{pub("49152:80")}
	if got := container.Published(cfg, container.State{Status: container.Running, Ports: bound}); !reflect.DeepEqual(got, bound) {
		t.Errorf("running: %v", got)
	}
	if got := container.Published(cfg, container.State{Status: container.Stopped, Ports: bound}); !reflect.DeepEqual(got, cfg.Publish) {
		t.Errorf("stopped: %v", got)
	}
}

func TestParseNetworkFlags(t *testing.T) {
	root := t.TempDir()
	st := network.Open(root)
	st.HostPrefixes = func() []netip.Prefix { return nil }
	front, err := st.Create("front")
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Create("back")
	if err != nil {
		t.Fatal(err)
	}

	n, err := parseNetworkFlags(root, networkFlags{publish: []string{"8080:80"}, addHosts: []string{"h:host-gateway", "x:1.2.3.4"}})
	if err != nil || !n.enabled || n.mode != "user" || n.networks != nil || !n.hostGateway || len(n.publish) != 1 || len(n.hosts) != 2 {
		t.Fatalf("default: %+v, %v", n, err)
	}
	for _, m := range []string{"bridge", "default", "user"} {
		if n, err := parseNetworkFlags(root, networkFlags{modes: []string{m}}); err != nil || !n.enabled || n.mode != "user" {
			t.Errorf("%s: %+v, %v", m, n, err)
		}
	}
	n, err = parseNetworkFlags(root, networkFlags{modes: []string{"none"}, addHosts: []string{"x:1.2.3.4"}})
	if err != nil || n.enabled || n.mode != "none" || len(n.hosts) != 1 {
		t.Errorf("none: %+v, %v", n, err)
	}

	// Named networks: sorted, the mode the first given, --network-alias
	// and --ip on the first given, the advanced form on others.
	n, err = parseNetworkFlags(root, networkFlags{modes: []string{"front", "name=back,alias=db,alias=pg,ip=172.19.0.9"}, aliases: []string{"web"}, ip: "172.18.0.7"})
	want := []container.Attachment{
		{Network: back.ID, Name: "back", Aliases: []string{"db", "pg"}, IPv4: "172.19.0.9"},
		{Network: front.ID, Name: "front", Aliases: []string{"web"}, IPv4: "172.18.0.7"},
	}
	if err != nil || !n.enabled || n.mode != "front" || !reflect.DeepEqual(n.networks, want) {
		t.Errorf("named: %+v, %v", n, err)
	}
	if n, err := parseNetworkFlags(root, networkFlags{modes: []string{front.ID[:6]}}); err != nil || n.networks[0].Name != "front" {
		t.Errorf("by ID prefix: %+v, %v", n, err)
	}

	for _, c := range []struct {
		f    networkFlags
		want string
	}{
		{networkFlags{modes: []string{"host"}}, "network host is not supported"},
		{networkFlags{modes: []string{"container:x"}}, "not supported"},
		{networkFlags{modes: []string{"nosuch"}}, "network nosuch not found"},
		{networkFlags{modes: []string{"front", "bridge"}}, "conflicting options: cannot attach both user-defined and non-user-defined network-modes"},
		{networkFlags{modes: []string{"none", "front"}}, "conflicting options"},
		{networkFlags{modes: []string{"front", "front"}}, "more than once"},
		{networkFlags{aliases: []string{"a"}}, "network-scoped aliases are only supported for user-defined networks"},
		{networkFlags{ip: "10.0.2.9"}, "user-specified IP address is supported on user-defined networks only"},
		{networkFlags{modes: []string{"front"}, ip: "172.19.0.2"}, "invalid config for network front: invalid endpoint settings:\nno configured subnet contains IP address 172.19.0.2"},
		{networkFlags{modes: []string{"front"}, ip: "fe80::1"}, "invalid IPv4 address"},
		{networkFlags{modes: []string{"name=front,mac-address=02:00:00:00:00:01"}}, "mac-address is not supported"},
		{networkFlags{modes: []string{"name=front,alias=a"}, aliases: []string{"b"}}, "conflicting options"},
		{networkFlags{modes: []string{"none"}, publish: []string{"8080:80"}}, "--network none"},
		{networkFlags{modes: []string{"none"}, addHosts: []string{"h:host-gateway"}}, "--network none"},
		{networkFlags{publish: []string{"bad"}}, "-p bad"},
	} {
		if _, err := parseNetworkFlags(root, c.f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.f, err, c.want)
		}
	}
}

func TestNetworkFlags(t *testing.T) {
	var o runOptions
	fs := runFlags(&o)
	fs.SetOutput(io.Discard)
	args := []string{"--net", "none", "-p", "1:2", "--publish", "3:4", "--add-host", "a:1.1.1.1", "alpine"}
	if err := fs.Parse(args); err != nil || !reflect.DeepEqual([]string(o.networks), []string{"none"}) || len(o.publish) != 2 || len(o.addHosts) != 1 {
		t.Errorf("parsed %+v, %v", o, err)
	}
	o = runOptions{}
	fs = runFlags(&o)
	args = []string{"--network", "a", "--net", "b", "--network-alias", "x", "--network-alias", "y", "--ip", "172.18.0.2", "alpine"}
	if err := fs.Parse(args); err != nil || len(o.networks) != 2 || len(o.aliases) != 2 || o.ip != "172.18.0.2" {
		t.Errorf("parsed %+v, %v", o, err)
	}
}

func TestParseRunOptions(t *testing.T) {
	parse := func(args ...string) (runSpec, error) {
		var o runOptions
		fs := runFlags(&o)
		fs.SetOutput(io.Discard)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		set := false
		fs.Visit(func(f *flag.Flag) { set = set || f.Name == "entrypoint" })
		return parseRunOptions(&o, set, fs.Args())
	}
	s, err := parse("--entrypoint", "/bin/sh", "-e", "A=1", "-w", "/w", "--rm", "-p", "8080:80", "--network-alias", "x", "--network", "n", "alpine", "-c", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if s.image != "alpine" || !reflect.DeepEqual(s.command, []string{"-c", "echo hi"}) || !s.entrypointSet ||
		!reflect.DeepEqual(s.entrypoint, []string{"/bin/sh"}) || !s.rm || s.workdir != "/w" || !reflect.DeepEqual(s.env, []string{"A=1"}) {
		t.Errorf("parsed %+v", s)
	}
	if len(s.network.attach) != 1 || s.network.attach[0].name != "n" || !reflect.DeepEqual(s.network.attach[0].aliases, []string{"x"}) ||
		len(s.network.publish) != 1 || s.network.publish[0].Guest != 80 {
		t.Errorf("network %+v", s.network)
	}
	if s, err := parse("--entrypoint", "", "alpine"); err != nil || !s.entrypointSet || s.entrypoint != nil {
		t.Errorf("empty entrypoint: %+v, %v", s, err)
	}
	if s, err := parse("alpine"); err != nil || s.entrypointSet || s.memory == 0 || s.cpus == 0 || s.storageSize == 0 || len(s.network.attach) != 0 {
		t.Errorf("defaults: %+v, %v", s, err)
	}
	if s, err := parse("--layer", "a.ext4", "sh", "-c", "x"); err != nil || s.image != "" || !reflect.DeepEqual(s.command, []string{"sh", "-c", "x"}) {
		t.Errorf("layers: %+v, %v", s, err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "no image given"},
		{[]string{"--rm", "--restart", "always", "alpine"}, "both --restart and --rm"},
		{[]string{"-m", "1x", "alpine"}, "invalid memory size"},
		{[]string{"--expose", "80/xyz", "alpine"}, "invalid protocol"},
		{[]string{"--expose", "90-80", "alpine"}, "invalid range"},
	} {
		_, err := parse(c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want an error saying %q", c.args, err, c.want)
		}
	}
}
