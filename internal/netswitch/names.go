package netswitch

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"github.com/quasidock/runwhp/internal/netstack"
)

// NameDirectory knows the names of the guests on one network.
type NameDirectory interface {
	// Resolve is the addresses of the guests that name names: the
	// container's name, one of its aliases on the network, the first 12
	// digits of its ID or its hostname, in any case, alone or followed by
	// a dot and the network's name. This guest's own count. It may look
	// again, briefly, for a name it does not know.
	Resolve(name string) []netip.Addr
	// NameOf is the name of the guest at ip, as NAME.NETWORK.
	NameOf(ip netip.Addr) (string, bool)
}

// Names answers DNS for a guest as Docker's embedded server does on
// user-defined networks: names of guests on the guest's networks, the first
// of its networks that knows a name answering, and everything else through
// Next, the host's resolver.
type Names struct {
	Networks []NameDirectory
	Next     netstack.Resolver
}

var _ netstack.Resolver = (*Names)(nil)

func (n *Names) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip6" {
		name := strings.TrimSuffix(host, ".")
		for _, d := range n.Networks {
			if addrs := d.Resolve(name); len(addrs) > 0 {
				return addrs, nil
			}
		}
	}
	return n.Next.LookupNetIP(ctx, network, host)
}

func (n *Names) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	if ip, err := netip.ParseAddr(addr); err == nil {
		for _, d := range n.Networks {
			if name, ok := d.NameOf(ip); ok {
				return []string{name + "."}, nil
			}
		}
	}
	return n.Next.LookupAddr(ctx, addr)
}

func (n *Names) LookupCNAME(ctx context.Context, host string) (string, error) {
	return n.Next.LookupCNAME(ctx, host)
}

func (n *Names) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return n.Next.LookupMX(ctx, name)
}

func (n *Names) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return n.Next.LookupTXT(ctx, name)
}

func (n *Names) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	return n.Next.LookupSRV(ctx, service, proto, name)
}
