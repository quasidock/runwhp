package netstack

import (
	"fmt"
	"net/netip"
	"strings"
)

// HostsLine reads an --add-host value, NAME:ADDRESS or NAME=ADDRESS, into a
// line of /etc/hosts. An ADDRESS of host-gateway is the gateway's, and
// gateway says connections to it are to reach the host.
func HostsLine(s string) (line string, gateway bool, err error) {
	i := strings.IndexAny(s, ":=")
	if i <= 0 {
		return "", false, fmt.Errorf("--add-host %s: want NAME:ADDRESS", s)
	}
	name, value := s[:i], strings.TrimSuffix(strings.TrimPrefix(s[i+1:], "["), "]")
	if strings.ContainsAny(name, " \t") {
		return "", false, fmt.Errorf("--add-host %s: bad host name %q", s, name)
	}
	if value == "host-gateway" {
		return Gateway.String() + "\t" + name, true, nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "", false, fmt.Errorf("--add-host %s: %q is not an IP address or host-gateway", s, value)
	}
	return ip.String() + "\t" + name, false, nil
}
