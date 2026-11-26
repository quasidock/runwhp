package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestPickSubnet(t *testing.T) {
	p := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	for _, c := range []struct {
		taken []netip.Prefix
		want  string
	}{
		{nil, "172.18.0.0/16"},
		{[]netip.Prefix{p("10.0.2.0/24")}, "172.18.0.0/16"},
		{[]netip.Prefix{p("172.18.0.0/16"), p("172.19.0.0/16")}, "172.20.0.0/16"},
		// WSL's vEthernet in a /20 of 172.20: that /16 is skipped.
		{[]netip.Prefix{p("172.18.0.0/16"), p("172.19.0.0/16"), p("172.20.48.0/20")}, "172.21.0.0/16"},
		// A host on 172.16.0.0/12 takes every /16.
		{[]netip.Prefix{p("172.16.0.0/12")}, "192.168.0.0/20"},
		{[]netip.Prefix{p("172.16.0.0/12"), p("192.168.2.0/24")}, "192.168.16.0/20"},
	} {
		got, err := pickSubnet(c.taken)
		if err != nil || got.String() != c.want {
			t.Errorf("taken %v: %v %v, want %s", c.taken, got, err, c.want)
		}
	}
	if _, err := pickSubnet([]netip.Prefix{p("172.16.0.0/12"), p("192.168.0.0/16")}); !errors.Is(err, ErrNoSubnet) {
		t.Errorf("all taken: %v", err)
	}
}

func TestPickAddress(t *testing.T) {
	n := &Network{Name: "n", Subnet: netip.MustParsePrefix("172.18.0.0/16"), Gateway: netip.MustParseAddr("172.18.0.1")}
	a := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	used := map[netip.Addr]bool{a("172.18.0.2"): true, a("172.18.0.4"): true}
	if got, err := pickAddress(n, netip.Addr{}, used); err != nil || got != a("172.18.0.3") {
		t.Errorf("lowest free: %v %v", got, err)
	}
	if got, err := pickAddress(n, a("172.18.9.9"), used); err != nil || got != a("172.18.9.9") {
		t.Errorf("asked for: %v %v", got, err)
	}
	for ip, want := range map[string]string{
		"172.18.0.2":     "already in use",
		"172.18.0.1":     "already in use",
		"172.19.0.5":     "no configured subnet contains IP address 172.19.0.5",
		"172.18.0.0":     "no configured subnet contains",
		"172.18.255.255": "no configured subnet contains",
	} {
		if _, err := pickAddress(n, a(ip), used); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ip, err, want)
		}
	}
	small := &Network{Name: "s", Subnet: netip.MustParsePrefix("192.168.0.0/30"), Gateway: netip.MustParseAddr("192.168.0.1")}
	if got, err := pickAddress(small, netip.Addr{}, nil); err != nil || got != a("192.168.0.2") {
		t.Errorf("/30: %v %v", got, err)
	}
	if _, err := pickAddress(small, netip.Addr{}, map[netip.Addr]bool{a("192.168.0.2"): true}); err == nil {
		t.Error("a full /30 gave an address")
	}
}

func TestBuiltin(t *testing.T) {
	for _, name := range []string{"user", "bridge", "default"} {
		n := Builtin(name)
		if n == nil || n.Name != "user" || n.Driver != "user" || n.Subnet.String() != "10.0.2.0/24" || !n.IsBuiltin() {
			t.Errorf("%s: %+v", name, n)
		}
	}
	if n := Builtin("none"); n == nil || n.Driver != "null" || n.ID == Builtin("user").ID || len(n.ID) != 64 {
		t.Errorf("none: %+v", n)
	}
	if Builtin("host") != nil || Builtin("mine") != nil {
		t.Error("host or mine is built in")
	}
	for _, name := range []string{"bridge", "Host", "none", "DEFAULT", "user"} {
		if err := predefinedError(name); !errors.Is(err, ErrPredefined) || !strings.Contains(err.Error(), "operation is not permitted on predefined "+strings.ToLower(name)+" network") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if predefinedError("mine") != nil {
		t.Error("mine is predefined")
	}
}

func TestLastAddr(t *testing.T) {
	for p, want := range map[string]string{"172.18.0.0/16": "172.18.255.255", "192.168.16.0/20": "192.168.31.255", "10.0.2.0/24": "10.0.2.255"} {
		if got := lastAddr(netip.MustParsePrefix(p)); got.String() != want {
			t.Errorf("%s: %s", p, got)
		}
	}
}
