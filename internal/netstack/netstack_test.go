package netstack

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// guest is a second gVisor stack standing in for the guest kernel, joined to
// the Stack under test frame by frame, as the virtio-net device joins them.
type guest struct {
	t     *testing.T
	host  *Stack
	stack *stack.Stack
}

func newGuest(t *testing.T, cfg Config) *guest { return newGuestWith(t, cfg, 0, nil) }

// newGuestWith is newGuest with the guest's link offering caps, and tap
// seeing each frame for the guest.
func newGuestWith(t *testing.T, cfg Config, caps stack.LinkEndpointCapabilities, tap func(frame []byte)) *guest {
	t.Helper()
	link := channel.New(512, MTU+header.EthernetMinimumSize, tcpip.LinkAddress(GuestMAC[:]))
	link.LinkEPCapabilities = caps
	gs := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := gs.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	as := cfg.Addresses
	if !as.Guest.IsValid() {
		as = Default
	}
	own := tcpip.AddressWithPrefix{Address: addr(as.Guest), PrefixLen: as.Subnet.Bits()}
	if err := gs.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: own}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	gs.SetRouteTable([]tcpip.Route{
		{Destination: own.Subnet(), NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: addr(as.Gateway), NIC: 1},
	})

	cfg.Output = func(frame []byte) {
		if tap != nil {
			tap(frame)
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		link.InjectInbound(0, pkt)
		pkt.DecRef()
	}
	host, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			var frame []byte
			for _, b := range pkt.AsSlices() {
				frame = append(frame, b...)
			}
			pkt.DecRef()
			host.Inject(frame)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		host.Close()
		gs.Close()
	})
	return &guest{t, host, gs}
}

func full(a netip.Addr, port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: addr(a), Port: port}
}

func (g *guest) dialTCP(to netip.AddrPort, timeout time.Duration) (*gonet.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, g.stack, full(to.Addr(), to.Port()), ipv4.ProtocolNumber)
}

// redirect is a Dial hook that sends every flow to target and records where
// the guest meant it to go.
type redirect struct {
	target string
	mu     sync.Mutex
	asked  []string
}

func (r *redirect) dial(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.asked = append(r.asked, network+" "+address)
	r.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, r.target)
}

func (r *redirect) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// upperServer answers each TCP connection with what it read, upper-cased,
// once the client has closed its side.
func upperServer(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				c.Write(bytes.ToUpper(data))
			}()
		}
	}()
	return ln
}

func TestTCPOutbound(t *testing.T) {
	ln := upperServer(t)
	r := &redirect{target: ln.Addr().String()}
	g := newGuest(t, Config{Dial: r.dial})

	c, err := g.dialTCP(netip.MustParseAddrPort("93.184.216.34:80"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Enough to need many full-size frames and window updates both ways.
	payload := bytes.Repeat([]byte("abcdefgh"), 512<<10)
	go func() {
		c.Write(payload)
		c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.ToUpper(payload)) {
		t.Fatalf("got %d bytes back, want %d upper-cased", len(got), len(payload))
	}
	if calls := r.calls(); len(calls) != 1 || calls[0] != "tcp 93.184.216.34:80" {
		t.Errorf("dialed %q", calls)
	}
}

func TestTCPRefused(t *testing.T) {
	refuse := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	g := newGuest(t, Config{Dial: refuse})
	start := time.Now()
	_, err := g.dialTCP(netip.MustParseAddrPort("192.0.2.1:443"), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial error = %v, want connection refused", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("refusal took %v", time.Since(start))
	}
}

func TestTarget(t *testing.T) {
	off := &Stack{cfg: Config{}}
	on := &Stack{cfg: Config{HostGateway: true}}
	cases := []struct {
		s    *Stack
		dst  string
		want string // "" means refused
	}{
		{off, "93.184.216.34:443", "93.184.216.34:443"},
		{off, "192.168.1.10:22", "192.168.1.10:22"},
		{off, "10.0.2.2:8080", ""},
		{on, "10.0.2.2:8080", "127.0.0.1:8080"},
		{on, "10.0.2.3:80", ""},
		{on, "10.0.2.99:80", ""},
		{on, "127.0.0.1:8080", ""},
		{on, "127.1.2.3:8080", ""},
		{on, "0.0.0.0:80", ""},
		{on, "224.0.0.251:5353", ""},
		{on, "255.255.255.255:67", ""},
		{on, "1.1.1.1:0", ""},
	}
	for _, c := range cases {
		ap := netip.MustParseAddrPort(c.dst)
		got, ok := c.s.target(addr(ap.Addr()), ap.Port())
		if (c.want == "") == ok || (ok && got.String() != c.want) {
			t.Errorf("target(%s, gateway %v) = %v %v, want %q", c.dst, c.s.cfg.HostGateway, got, ok, c.want)
		}
	}
}

func TestHostGateway(t *testing.T) {
	ln := upperServer(t)
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	gw := netip.AddrPortFrom(Gateway, port)

	r := &redirect{target: ln.Addr().String()}
	g := newGuest(t, Config{Dial: r.dial})
	if _, err := g.dialTCP(gw, 5*time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("gateway without HostGateway: %v, want refused", err)
	}
	if _, err := g.dialTCP(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), time.Second); err == nil {
		t.Error("guest reached host loopback directly")
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Errorf("dialed %q", calls)
	}

	g = newGuest(t, Config{HostGateway: true})
	c, err := g.dialTCP(gw, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("via gateway"))
	c.CloseWrite()
	if got, _ := io.ReadAll(c); string(got) != "VIA GATEWAY" {
		t.Errorf("got %q", got)
	}
}

func TestUDP(t *testing.T) {
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			echo.WriteTo(append([]byte("echo "), buf[:n]...), from)
		}
	}()
	r := &redirect{target: echo.LocalAddr().String()}
	g := newGuest(t, Config{Dial: r.dial})

	remote := full(netip.MustParseAddr("198.51.100.4"), 9999)
	c, err := gonet.DialUDP(g.stack, nil, &remote, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := make([]byte, 30000) // more than one Ethernet frame's worth, in one datagram
	rand.Read(big)
	for _, msg := range [][]byte{[]byte("one"), []byte("two"), big} {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64<<10)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf[:n], append([]byte("echo "), msg...)) {
			t.Errorf("got %d bytes back for a %d-byte datagram", n, len(msg))
		}
	}
	if calls := r.calls(); len(calls) != 1 || calls[0] != "udp 198.51.100.4:9999" {
		t.Errorf("dialed %q; want one flow", calls)
	}
}

// fakeResolver knows one name.
type fakeResolver struct{}

var errNotFound = &net.DNSError{Err: "no such host", IsNotFound: true}

func (fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if host == "app.example.test" && network == "ip4" {
		return []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("192.0.2.8")}, nil
	}
	if host == "broken.example.test" {
		return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	}
	return nil, errNotFound
}

func (fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if host == "www.example.test" {
		return "app.example.test.", nil
	}
	return "", errNotFound
}

func (fakeResolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	if a == "192.0.2.7" {
		return []string{"app.example.test."}, nil
	}
	return nil, errNotFound
}

func (fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Host: "mail.example.test.", Pref: 10}}, nil
}

func (fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if name == "big.example.test" {
		return []string{strings.Repeat("x", 600)}, nil
	}
	return []string{"v=spf1 -all"}, nil
}

func (fakeResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", []*net.SRV{{Target: "sip.example.test.", Port: 5060, Priority: 1, Weight: 2}}, nil
}

func query(t *testing.T, name string, typ dnsmessage.Type, edns bool) []byte {
	t.Helper()
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x1234, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}
	if edns {
		var opt dnsmessage.ResourceHeader
		opt.SetEDNS0(4096, dnsmessage.RCodeSuccess, false)
		m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
	}
	out, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parseReply(t *testing.T, b []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatalf("unpacking reply: %v", err)
	}
	if m.ID != 0x1234 || !m.Response || !m.RecursionAvailable {
		t.Errorf("reply header = %+v", m.Header)
	}
	return m
}

func TestAnswerDNS(t *testing.T) {
	ctx := context.Background()
	ask := func(name string, typ dnsmessage.Type, edns, udp bool) dnsmessage.Message {
		return parseReply(t, answerDNS(ctx, fakeResolver{}, query(t, name, typ, edns), udp))
	}

	m := ask("app.example.test.", dnsmessage.TypeA, false, true)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 2 ||
		m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 0, 2, 7} || m.Answers[0].Header.TTL != dnsTTL {
		t.Errorf("A: %v %+v", m.RCode, m.Answers)
	}
	if m := ask("app.example.test.", dnsmessage.TypeAAAA, false, true); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("AAAA: %v %+v", m.RCode, m.Answers)
	}
	if m := ask("nope.example.test.", dnsmessage.TypeA, false, true); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("missing name: %v", m.RCode)
	}
	if m := ask("broken.example.test.", dnsmessage.TypeA, false, true); m.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("failing lookup: %v", m.RCode)
	}
	if m := ask("app.example.test.", dnsmessage.TypeHINFO, false, true); m.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("HINFO: %v", m.RCode)
	}
	if m := ask("www.example.test.", dnsmessage.TypeCNAME, false, true); len(m.Answers) != 1 ||
		m.Answers[0].Body.(*dnsmessage.CNAMEResource).CNAME.String() != "app.example.test." {
		t.Errorf("CNAME: %+v", m.Answers)
	}
	if m := ask("7.2.0.192.in-addr.arpa.", dnsmessage.TypePTR, false, true); len(m.Answers) != 1 ||
		m.Answers[0].Body.(*dnsmessage.PTRResource).PTR.String() != "app.example.test." {
		t.Errorf("PTR: %+v", m.Answers)
	}
	if m := ask("example.test.", dnsmessage.TypeMX, false, true); len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.MXResource).Pref != 10 {
		t.Errorf("MX: %+v", m.Answers)
	}
	if m := ask("_sip._udp.example.test.", dnsmessage.TypeSRV, false, true); len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.SRVResource).Port != 5060 {
		t.Errorf("SRV: %+v", m.Answers)
	}

	// A 600-byte TXT record does not fit in 512 bytes: over UDP without
	// EDNS the reply is truncated, with EDNS or over TCP it is whole.
	if m := ask("big.example.test.", dnsmessage.TypeTXT, false, true); !m.Truncated || len(m.Answers) != 0 {
		t.Errorf("big TXT over UDP: truncated %v, %d answers", m.Truncated, len(m.Answers))
	}
	m = ask("big.example.test.", dnsmessage.TypeTXT, true, true)
	if m.Truncated || len(m.Answers) != 1 || strings.Join(m.Answers[0].Body.(*dnsmessage.TXTResource).TXT, "") != strings.Repeat("x", 600) {
		t.Errorf("big TXT with EDNS: truncated %v, %d answers", m.Truncated, len(m.Answers))
	}
	if len(m.Additionals) != 1 || m.Additionals[0].Header.Type != dnsmessage.TypeOPT {
		t.Errorf("EDNS reply has no OPT record: %+v", m.Additionals)
	}
	if m := ask("big.example.test.", dnsmessage.TypeTXT, false, false); m.Truncated || len(m.Answers) != 1 {
		t.Errorf("big TXT over TCP: truncated %v, %d answers", m.Truncated, len(m.Answers))
	}

	if answerDNS(ctx, fakeResolver{}, []byte{1, 2, 3}, true) != nil {
		t.Error("answered garbage")
	}
}

func TestDNSOverTheLink(t *testing.T) {
	g := newGuest(t, Config{Resolver: fakeResolver{}})

	server := full(DNS, 53)
	uc, err := gonet.DialUDP(g.stack, nil, &server, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := uc.Write(query(t, "app.example.test.", dnsmessage.TypeA, false)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := uc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if m := parseReply(t, buf[:n]); len(m.Answers) != 2 {
		t.Errorf("over UDP: %+v", m.Answers)
	}

	tc, err := g.dialTCP(netip.AddrPortFrom(DNS, 53), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	for range 2 { // two queries on one connection
		q := query(t, "big.example.test.", dnsmessage.TypeTXT, false)
		tc.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
		var size [2]byte
		if _, err := io.ReadFull(tc, size[:]); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(tc, reply); err != nil {
			t.Fatal(err)
		}
		if m := parseReply(t, reply); m.Truncated || len(m.Answers) != 1 {
			t.Errorf("over TCP: truncated %v, %+v", m.Truncated, m.Answers)
		}
	}

	// Nothing else answers at the DNS address.
	if _, err := g.dialTCP(netip.AddrPortFrom(DNS, 80), 5*time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("DNS address port 80: %v, want refused", err)
	}
}

func TestPublish(t *testing.T) {
	g := newGuest(t, Config{Publish: []Port{{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 8080}}})
	ln, err := gonet.ListenTCP(g.stack, full(Guest, 8080), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		c.Write(bytes.ToUpper(data))
	}()

	hostAddr := g.host.listeners[0].Addr().String()
	c, err := net.DialTimeout("tcp", hostAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("published"))
	c.(*net.TCPConn).CloseWrite()
	if got, _ := io.ReadAll(c); string(got) != "PUBLISHED" {
		t.Errorf("got %q", got)
	}

	// Nothing listens on guest port 9090: the host connection is closed.
	g2 := newGuest(t, Config{Publish: []Port{{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 9090}}})
	c2, err := net.DialTimeout("tcp", g2.host.listeners[0].Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	var ne net.Error
	if n, err := c2.Read(make([]byte, 1)); n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Errorf("read from a port nobody listens on: %d, %v; want it closed", n, err)
	}

	// A host port already in use fails New.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, err = New(Config{Output: func([]byte) {}, Publish: []Port{{Host: netip.MustParseAddrPort(busy.Addr().String()), Guest: 80}}})
	if err == nil || !strings.Contains(err.Error(), "publishing port") {
		t.Errorf("busy port: %v", err)
	}
}

func TestCloseEndsConnections(t *testing.T) {
	ln := upperServer(t)
	r := &redirect{target: ln.Addr().String()}
	g := newGuest(t, Config{Dial: r.dial})
	c, err := g.dialTCP(netip.MustParseAddrPort("203.0.113.9:80"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "the flow to be tracked", func() bool {
		g.host.mu.Lock()
		defer g.host.mu.Unlock()
		return len(g.host.closers) == 2
	})
	done := make(chan struct{})
	go func() {
		g.host.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a live connection")
	}
	if !strings.Contains(g.host.Stats(), "1 tcp") {
		t.Errorf("stats = %s", g.host.Stats())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// tcpChecksums counts the TCP segments in frames for the guest whose
// checksums are right, and those whose checksums are not.
type tcpChecksums struct {
	mu          sync.Mutex
	good, wrong int
}

func (c *tcpChecksums) tap(frame []byte) {
	if len(frame) < header.EthernetMinimumSize || header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		return
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || ip.TransportProtocol() != header.TCPProtocolNumber {
		return
	}
	seg := header.TCP(ip.Payload())
	payload := seg[seg.DataOffset():]
	ok := seg.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), checksum.Checksum(payload, 0), uint16(len(payload)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.good++
	} else {
		c.wrong++
	}
}

// The guest may leave its checksums unfilled, and the stack leaves out
// those of frames for a guest that takes them so.
func TestChecksumOffload(t *testing.T) {
	ln := upperServer(t)
	for _, guestChecksums := range []bool{false, true} {
		var sums tcpChecksums
		r := &redirect{target: ln.Addr().String()}
		// The guest stand-in sends without checksums, as Linux does with
		// VIRTIO_NET_F_CSUM, and, if it takes frames without them, skips
		// checking, as Linux does for frames marked valid.
		caps := stack.CapabilityTXChecksumOffload
		if guestChecksums {
			caps |= stack.CapabilityRXChecksumOffload
		}
		g := newGuestWith(t, Config{Dial: r.dial, GuestChecksums: func() bool { return guestChecksums }}, caps, sums.tap)
		c, err := g.dialTCP(netip.MustParseAddrPort("93.184.216.34:80"), 5*time.Second)
		if err != nil {
			t.Fatalf("guest checksums %v: %v", guestChecksums, err)
		}
		payload := bytes.Repeat([]byte("abcdefgh"), 64<<10)
		go func() {
			c.Write(payload)
			c.CloseWrite()
		}()
		got, err := io.ReadAll(c)
		c.Close()
		if err != nil || !bytes.Equal(got, bytes.ToUpper(payload)) {
			t.Fatalf("guest checksums %v: got %d bytes back, %v", guestChecksums, len(got), err)
		}
		sums.mu.Lock()
		good, wrong := sums.good, sums.wrong
		sums.mu.Unlock()
		switch {
		case !guestChecksums && (wrong != 0 || good == 0):
			t.Errorf("%d segments to the guest with wrong checksums, %d right; want all right", wrong, good)
		case guestChecksums && wrong == 0:
			t.Errorf("all %d segments to a guest that takes them without checksums had them computed", good)
		}
	}
}

// A guest on a network of its own: DNS at the gateway, the host through
// either gateway with HostGateway, its own subnet and the refused ones kept
// from the host, and published ports reaching it at its own address.
func TestOtherAddresses(t *testing.T) {
	as := Addresses{
		Subnet:     netip.MustParsePrefix("172.18.0.0/16"),
		Gateway:    netip.MustParseAddr("172.18.0.1"),
		DNS:        netip.MustParseAddr("172.18.0.1"),
		Guest:      netip.MustParseAddr("172.18.0.5"),
		GatewayMAC: [6]byte{2, 0x42, 172, 18, 0, 1},
	}
	ln := upperServer(t)
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	g := newGuest(t, Config{
		Addresses: as, HostGateway: true, Resolver: fakeResolver{},
		Refuse:  []netip.Prefix{netip.MustParsePrefix("172.19.0.0/16")},
		Publish: []Port{{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 8080}},
	})

	for _, gw := range []netip.Addr{as.Gateway, Gateway} {
		c, err := g.dialTCP(netip.AddrPortFrom(gw, port), 5*time.Second)
		if err != nil {
			t.Fatalf("host through %s: %v", gw, err)
		}
		c.Write([]byte("hi"))
		c.CloseWrite()
		if got, _ := io.ReadAll(c); string(got) != "HI" {
			t.Errorf("through %s: got %q", gw, got)
		}
		c.Close()
	}
	for _, dst := range []string{"172.18.0.9", "172.19.0.3", "10.0.2.15"} {
		if _, err := g.dialTCP(netip.AddrPortFrom(netip.MustParseAddr(dst), port), 5*time.Second); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v, want refused", dst, err)
		}
	}

	server := full(as.DNS, 53)
	uc, err := gonet.DialUDP(g.stack, nil, &server, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(5 * time.Second))
	uc.Write(query(t, "app.example.test.", dnsmessage.TypeA, false))
	buf := make([]byte, 4096)
	n, err := uc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if m := parseReply(t, buf[:n]); len(m.Answers) != 2 {
		t.Errorf("DNS at the gateway: %+v", m.Answers)
	}

	gl, err := gonet.ListenTCP(g.stack, full(as.Guest, 8080), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	go func() {
		if c, err := gl.Accept(); err == nil {
			c.Write([]byte("guest"))
			c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp", g.host.listeners[0].Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if got, _ := io.ReadAll(c); string(got) != "guest" {
		t.Errorf("published port: got %q", got)
	}
}
