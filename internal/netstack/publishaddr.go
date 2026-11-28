package netstack

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// PublishAddressEnv names the setting for the address a port published
// without one listens on.
const PublishAddressEnv = "RUNWHP_PUBLISH_ADDRESS"

// DefaultPublishAddress is the address a port published without one
// listens on: RUNWHP_PUBLISH_ADDRESS, or 127.0.0.1. Docker listens on
// every address, which 0.0.0.0 asks for, or :: for IPv6's too; runwhp
// keeps to the host's loopback unless told otherwise, as listening on
// other addresses has Windows Firewall ask whether to allow it.
func DefaultPublishAddress() (netip.Addr, error) {
	v := strings.TrimSpace(os.Getenv(PublishAddressEnv))
	if v == "" {
		return netip.AddrFrom4([4]byte{127, 0, 0, 1}), nil
	}
	a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(v, "["), "]"))
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%s=%q is not an IP address", PublishAddressEnv, v)
	}
	return a.Unmap(), nil
}
