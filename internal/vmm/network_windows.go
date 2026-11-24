//go:build windows

package vmm

import (
	"net"
	"net/netip"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
)

// GuestNetwork is the guest's side of the network, or nil without one.
func GuestNetwork(enabled bool) *guestspec.Network {
	if !enabled {
		return nil
	}
	return &guestspec.Network{
		MAC:     net.HardwareAddr(netstack.GuestMAC[:]).String(),
		Address: netip.PrefixFrom(netstack.Guest, netstack.Subnet.Bits()).String(),
		Gateway: netstack.Gateway.String(),
		DNS:     []string{netstack.DNS.String()},
		MTU:     netstack.MTU,
	}
}
