package netstack

import (
	"bytes"
	"context"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// fakePinger answers as reply says, and records what it was asked.
type fakePinger struct {
	mu    sync.Mutex
	asked []Echo
	reply func(Echo) (EchoReply, error)
}

func (f *fakePinger) ping(_ context.Context, e Echo) (EchoReply, error) {
	f.mu.Lock()
	e.Data = append([]byte(nil), e.Data...)
	f.asked = append(f.asked, e)
	f.mu.Unlock()
	if f.reply == nil {
		return EchoReply{From: e.Dst, TTL: 57}, nil
	}
	return f.reply(e)
}

func (f *fakePinger) calls() []Echo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Echo(nil), f.asked...)
}

// icmpTap collects the ICMP packets the stack writes to the guest.
type icmpTap struct {
	mu  sync.Mutex
	got chan header.IPv4
}

func newICMPTap() *icmpTap { return &icmpTap{got: make(chan header.IPv4, 16)} }

func (c *icmpTap) tap(frame []byte) {
	if len(frame) < header.EthernetMinimumSize || header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		return
	}
	ip := header.IPv4(append([]byte(nil), frame[header.EthernetMinimumSize:]...))
	if !ip.IsValid(len(ip)) || ip.TransportProtocol() != header.ICMPv4ProtocolNumber {
		return
	}
	select {
	case c.got <- ip:
	default:
	}
}

func (c *icmpTap) next(t *testing.T, wait time.Duration) header.IPv4 {
	t.Helper()
	select {
	case ip := <-c.got:
		return ip
	case <-time.After(wait):
		return nil
	}
}

// echoFrame is an echo request from the guest, as its kernel would send it
// to the gateway's MAC address.
func echoFrame(as Addresses, from, to netip.Addr, ttl uint8, ident, seq uint16, data []byte) []byte {
	msg := make([]byte, header.ICMPv4MinimumSize+len(data))
	h := header.ICMPv4(msg)
	h.SetType(header.ICMPv4Echo)
	h.SetIdent(ident)
	h.SetSequence(seq)
	copy(msg[header.ICMPv4MinimumSize:], data)
	h.SetChecksum(header.ICMPv4Checksum(h, 0))

	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+len(msg))
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(GuestMAC[:]), DstAddr: tcpip.LinkAddress(as.GatewayMAC[:]),
		Type: header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + len(msg)), TTL: ttl,
		Flags: header.IPv4FlagDontFragment, ID: 7,
		Protocol: uint8(header.ICMPv4ProtocolNumber), SrcAddr: addr(from), DstAddr: addr(to),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:], msg)
	return frame
}

// checkICMP fails unless ip is a well-formed ICMP packet from src to dst of
// the given type.
func checkICMP(t *testing.T, ip header.IPv4, src, dst netip.Addr, typ header.ICMPv4Type) header.ICMPv4 {
	t.Helper()
	if ip == nil {
		t.Fatalf("no ICMP %d from %s", typ, src)
	}
	if !ip.IsChecksumValid() {
		t.Errorf("IPv4 checksum wrong")
	}
	if got := netip.AddrFrom4(ip.SourceAddress().As4()); got != src {
		t.Errorf("from %s, want %s", got, src)
	}
	if got := netip.AddrFrom4(ip.DestinationAddress().As4()); got != dst {
		t.Errorf("to %s, want %s", got, dst)
	}
	h := header.ICMPv4(ip.Payload())
	if h.Type() != typ {
		t.Fatalf("ICMP type %d, want %d", h.Type(), typ)
	}
	if checksum.Checksum(h, 0) != 0xffff {
		t.Errorf("ICMP checksum wrong")
	}
	return h
}

// An echo request for an address beyond the link goes to the host's pinger
// with one less TTL, and its reply comes back from that address with the
// request's ident, sequence and data.
func TestPing(t *testing.T) {
	f := &fakePinger{}
	tap := newICMPTap()
	g := newGuestWith(t, Config{Ping: f.ping}, 0, tap.tap)
	dst := netip.MustParseAddr("192.0.2.1")
	data := []byte("abcdefghijklmnopqrstuvwxyz012345")
	g.host.Inject(echoFrame(Default, Guest, dst, 64, 0x1234, 3, data))

	ip := tap.next(t, 5*time.Second)
	h := checkICMP(t, ip, dst, Guest, header.ICMPv4EchoReply)
	if ttl := ip.TTL(); ttl != 56 {
		t.Errorf("reply TTL %d, want 56: the 57 it reached the host with, less the stack's hop", ttl)
	}
	if h.Ident() != 0x1234 || h.Sequence() != 3 || !bytes.Equal(h.Payload(), data) {
		t.Errorf("reply ident %#x seq %d data %q", h.Ident(), h.Sequence(), h.Payload())
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Dst != dst || calls[0].TTL != 63 || !calls[0].DF || !bytes.Equal(calls[0].Data, data) {
		t.Errorf("pinger asked %+v", calls)
	}
}

// A TTL that runs out on the way, or at the stack, comes back as time
// exceeded, from the router that said so or from the gateway, quoting the
// request.
func TestPingTTLExpired(t *testing.T) {
	router := netip.MustParseAddr("198.51.100.1")
	f := &fakePinger{reply: func(Echo) (EchoReply, error) { return EchoReply{From: router, TTLExpired: true}, nil }}
	tap := newICMPTap()
	g := newGuestWith(t, Config{Ping: f.ping}, 0, tap.tap)
	dst := netip.MustParseAddr("192.0.2.1")

	g.host.Inject(echoFrame(Default, Guest, dst, 5, 9, 1, []byte("x")))
	h := checkICMP(t, tap.next(t, 5*time.Second), router, Guest, header.ICMPv4TimeExceeded)
	quoted := header.IPv4(h[header.ICMPv4MinimumSize:])
	if netip.AddrFrom4(quoted.DestinationAddress().As4()) != dst || len(quoted) != header.IPv4MinimumSize+header.ICMPv4MinimumSize {
		t.Errorf("quoted %x", []byte(quoted))
	}

	g.host.Inject(echoFrame(Default, Guest, dst, 1, 9, 2, []byte("x")))
	checkICMP(t, tap.next(t, 5*time.Second), Gateway, Guest, header.ICMPv4TimeExceeded)
	if n := len(f.calls()); n != 1 {
		t.Errorf("pinger asked %d times, want once: TTL 1 ends at the stack", n)
	}
}

// What the guest may not reach gets no reply, and the pinger is not asked;
// an echo the pinger finds unanswered gets none either.
func TestPingRefused(t *testing.T) {
	f := &fakePinger{reply: func(Echo) (EchoReply, error) { return EchoReply{}, ErrNoReply }}
	tap := newICMPTap()
	g := newGuestWith(t, Config{Ping: f.ping}, 0, tap.tap)
	for _, dst := range []string{"10.0.2.99", "127.0.0.1", "224.0.0.1"} {
		g.host.Inject(echoFrame(Default, Guest, netip.MustParseAddr(dst), 64, 1, 1, nil))
	}
	g.host.Inject(echoFrame(Default, Guest, netip.MustParseAddr("192.0.2.9"), 64, 1, 2, nil))
	if ip := tap.next(t, 300*time.Millisecond); ip != nil {
		t.Errorf("got a reply from %s", ip.SourceAddress())
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Dst != netip.MustParseAddr("192.0.2.9") {
		t.Errorf("pinger asked %+v", calls)
	}
}

// The stack's own addresses are answered by the stack, as before, without
// the pinger; the gateway's even without one.
func TestPingGateway(t *testing.T) {
	f := &fakePinger{}
	tap := newICMPTap()
	g := newGuestWith(t, Config{Ping: f.ping}, 0, tap.tap)
	for _, dst := range []netip.Addr{Gateway, DNS} {
		g.host.Inject(echoFrame(Default, Guest, dst, 64, 5, 1, []byte("hi")))
		checkICMP(t, tap.next(t, 5*time.Second), dst, Guest, header.ICMPv4EchoReply)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("pinger asked %+v", calls)
	}
}

// On a network of its own, a ping to the default network's gateway is a
// ping to the host's loopback with HostGateway, answered from the address
// the guest asked.
func TestPingHostGateway(t *testing.T) {
	as := Addresses{
		Subnet:     netip.MustParsePrefix("172.18.0.0/16"),
		Gateway:    netip.MustParseAddr("172.18.0.1"),
		DNS:        netip.MustParseAddr("172.18.0.1"),
		Guest:      netip.MustParseAddr("172.18.0.5"),
		GatewayMAC: [6]byte{2, 0x42, 172, 18, 0, 1},
	}
	f := &fakePinger{}
	tap := newICMPTap()
	g := newGuestWith(t, Config{Addresses: as, HostGateway: true, Ping: f.ping}, 0, tap.tap)
	g.host.Inject(echoFrame(as, as.Guest, Gateway, 64, 5, 1, nil))
	checkICMP(t, tap.next(t, 5*time.Second), Gateway, as.Guest, header.ICMPv4EchoReply)
	if calls := f.calls(); len(calls) != 1 || calls[0].Dst != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("pinger asked %+v", calls)
	}
}

// The system's pinger reaches the host's own loopback unelevated.
func TestSystemPing(t *testing.T) {
	p := systemPinger()
	if p == nil {
		if runtime.GOOS == "windows" {
			t.Fatal("no system pinger on Windows")
		}
		t.Skip("no system pinger here")
	}
	data := []byte("runwhp")
	r, err := p(context.Background(), Echo{Dst: netip.MustParseAddr("127.0.0.1"), TTL: 64, Data: data, Timeout: 2 * time.Second})
	if err != nil || r.From != netip.MustParseAddr("127.0.0.1") || r.TTLExpired {
		t.Fatalf("got %+v, %v", r, err)
	}
}
