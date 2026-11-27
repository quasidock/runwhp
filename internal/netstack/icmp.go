package netstack

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Echo is an ICMP echo request the guest sent beyond the stack, to be sent
// again from the host.
type Echo struct {
	Dst     netip.Addr
	TTL     uint8
	DF      bool // don't fragment
	Data    []byte
	Timeout time.Duration
}

// EchoReply is what came back for an Echo: an echo reply from Dst, or, with
// TTLExpired, word from the router at From that the request's TTL ran out.
type EchoReply struct {
	From       netip.Addr
	TTL        uint8
	TTLExpired bool
}

// ErrNoReply is a Pinger's error for a request nothing answered in time.
var ErrNoReply = errors.New("no reply")

// Pinger sends an echo request from the host and waits for what answers it.
type Pinger func(ctx context.Context, e Echo) (EchoReply, error)

const (
	pingsInFlight = 64
	pingTimeout   = 4 * time.Second
	maxPingData   = 65500
)

// forwardICMP takes the guest's ICMP packets that the stack itself would not
// answer: echo requests for addresses beyond the link. Each is sent again
// from the host, and its reply, or the time-exceeded a router sends, is
// written to the guest from where it came. It runs on the stack's inbound
// path, so the ping waits on a goroutine of its own. Returning false leaves
// the packet to the stack, which answers echoes to its own addresses, the
// gateway's and DNS's; returning true for one of those would silence them.
func (s *Stack) forwardICMP(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	h := header.ICMPv4(pkt.TransportHeader().Slice())
	if len(h) < header.ICMPv4MinimumSize || h.Type() != header.ICMPv4Echo {
		return false
	}
	if s.ownAddress(id.LocalAddress) {
		return false
	}
	dst, ok := s.targetAddr(id.LocalAddress)
	if !ok {
		s.refused.Add(1)
		return true
	}
	ping := s.cfg.Ping
	if ping == nil {
		return true
	}
	select {
	case s.pings <- struct{}{}:
	default:
		s.pingsDropped.Add(1)
		return true
	}

	// The packet is the stack's again once this returns: copy what the
	// reply needs.
	ip := header.IPv4(pkt.NetworkHeader().Slice())
	msg := stack.PayloadSince(pkt.TransportHeader())
	req := append([]byte(nil), msg.AsSlice()...)
	msg.Release()
	if len(req)-header.ICMPv4MinimumSize > maxPingData {
		<-s.pings
		return true
	}
	// A time-exceeded message quotes the datagram's header and its first 8
	// bytes.
	quoted := append(append([]byte(nil), ip[:ip.HeaderLength()]...), req[:header.ICMPv4MinimumSize]...)
	ttl, df := ip.TTL(), ip.Flags()&header.IPv4FlagDontFragment != 0
	guest, asked := id.RemoteAddress, id.LocalAddress
	guestMAC := header.Ethernet(pkt.LinkHeader().Slice()).SourceAddress()
	s.pingsSent.Add(1)

	go func() {
		defer func() { <-s.pings }()
		// The stack is the guest's first hop: a TTL that would not survive
		// it ends here, as at a router.
		if ttl <= 1 {
			s.writeICMP(guestMAC, addr(s.cfg.Addresses.Gateway), guest, 64, timeExceeded(quoted))
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, pingTimeout)
		defer cancel()
		r, err := ping(ctx, Echo{Dst: dst, TTL: ttl - 1, DF: df, Data: req[header.ICMPv4MinimumSize:], Timeout: pingTimeout})
		if err != nil {
			if !errors.Is(err, ErrNoReply) && s.ctx.Err() == nil {
				s.cfg.Logf("ping %s: %v", dst, err)
			}
			return
		}
		if r.TTLExpired {
			s.writeICMP(guestMAC, addr(r.From), guest, hopTTL(r.TTL), timeExceeded(quoted))
			return
		}
		reply := req
		header.ICMPv4(reply).SetType(header.ICMPv4EchoReply)
		header.ICMPv4(reply).SetCode(0)
		// The reply comes from the address the guest asked, which is not
		// dst when host-gateway mapped the gateway to the host's loopback.
		s.writeICMP(guestMAC, asked, guest, hopTTL(r.TTL), reply)
	}()
	return true
}

// timeExceeded is an ICMP time-exceeded message, TTL exceeded in transit,
// quoting the datagram whose TTL ran out.
func timeExceeded(quoted []byte) []byte {
	m := make([]byte, header.ICMPv4MinimumSize+len(quoted))
	header.ICMPv4(m).SetType(header.ICMPv4TimeExceeded)
	header.ICMPv4(m).SetCode(header.ICMPv4TTLExceeded)
	copy(m[header.ICMPv4MinimumSize:], quoted)
	return m
}

// writeICMP writes an ICMP message to the guest from src, which need not be
// one of the stack's own addresses.
func (s *Stack) writeICMP(guestMAC tcpip.LinkAddress, src, dst tcpip.Address, ttl uint8, msg []byte) {
	h := header.ICMPv4(msg)
	h.SetChecksum(0)
	h.SetChecksum(header.ICMPv4Checksum(h, 0))
	pkt := make([]byte, header.IPv4MinimumSize+len(msg))
	ip := header.IPv4(pkt)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(pkt)),
		TTL:         ttl,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(pkt[header.IPv4MinimumSize:], msg)

	// Close takes the lock before it closes the stack, so no write lands
	// on a stack that is closing.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if err := s.stack.WritePacketToRemote(nicID, guestMAC, ipv4.ProtocolNumber, buffer.MakeWithData(pkt)); err != nil {
		s.cfg.Logf("icmp to the guest: %s", err)
	}
}

// ownAddress reports whether a is one of the stack's own addresses on the
// link, which the stack answers itself.
func (s *Stack) ownAddress(a tcpip.Address) bool {
	return a == addr(s.cfg.Addresses.Gateway) || a == addr(s.cfg.Addresses.DNS)
}

// hopTTL is the TTL a reply that reached the host with ttl has once the
// stack, a hop on its way to the guest, has passed it on.
func hopTTL(ttl uint8) uint8 {
	if ttl <= 1 {
		return 64 // the pinger did not say
	}
	return ttl - 1
}
