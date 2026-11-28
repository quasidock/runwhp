package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
)

// errNoServers is a lookup's error when none of the servers asked for
// could be reached from the host.
var errNoServers = errors.New("no DNS server to ask")

// Upstream is a Resolver that asks the DNS servers given, as Docker's
// embedded server asks --dns's servers for names that are not containers',
// rather than the host's resolver: through dial, from the host, the first
// until it fails, then the next, as Go's resolver tries a lookup again.
// Names are asked as given, absolute, so that neither the host's search
// domains nor its own apply; the guest's resolv.conf has the container's.
// The host's hosts file still answers first, as Go's resolver reads it.
func Upstream(servers []netip.AddrPort, dial func(ctx context.Context, network, address string) (net.Conn, error)) Resolver {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var cur atomic.Uint32
	return upstream{&net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if len(servers) == 0 {
				return nil, errNoServers
			}
			i := cur.Load()
			// A server that fails, to dial or to answer in time, gives the
			// next lookup, and this one's next try, to the server after it.
			failed := func() { cur.CompareAndSwap(i, (i+1)%uint32(len(servers))) }
			c, err := dial(ctx, network, servers[i%uint32(len(servers))].String())
			if err != nil {
				failed()
				return nil, err
			}
			uc := upstreamConn{Conn: c, failed: failed}
			// Go's resolver frames DNS for UDP only on a PacketConn.
			if pc, ok := c.(net.PacketConn); ok {
				return &upstreamPacketConn{uc, pc}, nil
			}
			return &uc, nil
		},
	}}
}

// upstreamConn is a connection to an upstream server, which reports a read
// that times out as the server failing.
type upstreamConn struct {
	net.Conn
	failed func()
}

func (c *upstreamConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.failed()
	}
	return n, err
}

// upstreamPacketConn is an upstreamConn over UDP.
type upstreamPacketConn struct {
	upstreamConn
	pc net.PacketConn
}

func (c *upstreamPacketConn) ReadFrom(b []byte) (int, net.Addr, error) { return c.pc.ReadFrom(b) }

func (c *upstreamPacketConn) WriteTo(b []byte, a net.Addr) (int, error) { return c.pc.WriteTo(b, a) }

type upstream struct{ r *net.Resolver }

func absolute(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

func (u upstream) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return u.r.LookupNetIP(ctx, network, absolute(host))
}

func (u upstream) LookupCNAME(ctx context.Context, host string) (string, error) {
	return u.r.LookupCNAME(ctx, absolute(host))
}

func (u upstream) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return u.r.LookupAddr(ctx, addr)
}

func (u upstream) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return u.r.LookupMX(ctx, absolute(name))
}

func (u upstream) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return u.r.LookupTXT(ctx, absolute(name))
}

func (u upstream) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	return u.r.LookupSRV(ctx, service, proto, absolute(name))
}
