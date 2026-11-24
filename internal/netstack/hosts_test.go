package netstack

import (
	"strings"
	"testing"
)

func TestHostsLine(t *testing.T) {
	for in, want := range map[string]string{
		"db:192.168.1.20":                   "192.168.1.20\tdb",
		"db=192.168.1.20":                   "192.168.1.20\tdb",
		"v6:::1":                            "::1\tv6",
		"v6:[2001:db8::1]":                  "2001:db8::1\tv6",
		"host.docker.internal:host-gateway": "10.0.2.2\thost.docker.internal",
	} {
		line, gw, err := HostsLine(in)
		if err != nil || line != want || gw != strings.HasSuffix(in, "host-gateway") {
			t.Errorf("HostsLine(%q) = %q, %v, %v; want %q", in, line, gw, err, want)
		}
	}
	for _, in := range []string{"nocolon", ":1.2.3.4", "db:not-an-ip", "a b:1.2.3.4"} {
		if _, _, err := HostsLine(in); err == nil {
			t.Errorf("HostsLine(%q) accepted", in)
		}
	}
}
