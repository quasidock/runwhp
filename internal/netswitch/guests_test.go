package netswitch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/virtio"
)

var subnet = netip.MustParsePrefix("172.18.0.0/16")

// testGuest is a gVisor stack standing in for a guest's kernel, on a link
// whose far end is a Router, as a virtio-net device would join them.
type testGuest struct {
	ip     netip.Addr
	stack  *stack.Stack
	link   *channel.Endpoint
	router *Router
	local  *netstack.Stack
	sender *Sender
}

func (g *testGuest) deliver(frame []byte, _ virtio.Offload) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
	g.link.InjectInbound(0, pkt)
	pkt.DecRef()
}

// names is a NameDirectory over a fixed table.
type names map[string]netip.Addr

func (n names) Resolve(name string) []netip.Addr {
	if ip, ok := n[strings.ToLower(name)]; ok {
		return []netip.Addr{ip}
	}
	return nil
}

func (n names) NameOf(ip netip.Addr) (string, bool) {
	for k, v := range n {
		if v == ip && !strings.Contains(k, ".") {
			return k + ".net", true
		}
	}
	return "", false
}

// failResolver answers nothing.
type failResolver struct{}

func (failResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}
func (failResolver) LookupCNAME(context.Context, string) (string, error) { return "", io.EOF }
func (failResolver) LookupAddr(context.Context, string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}
func (failResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return nil, io.EOF }
func (failResolver) LookupTXT(context.Context, string) ([]string, error) { return nil, io.EOF }
func (failResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, io.EOF
}

// twoGuests joins two guests on 172.18.0.0/16, each with its router, its
// local stack and links to the other.
func twoGuests(t *testing.T) (a, b *testGuest) {
	a, b = &testGuest{ip: selfIP}, &testGuest{ip: peerIP}
	byIP := map[netip.Addr]*testGuest{a.ip: a, b.ip: b}
	table := names{"a": a.ip, "b": b.ip, "b.net": b.ip}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, g := range []*testGuest{a, b} {
		other := a
		if g == a {
			other = b
		}
		g.link = channel.New(512, netstack.MTU+header.EthernetMinimumSize, tcpip.LinkAddress(string(func() []byte { m := MAC(g.ip); return m[:] }())))
		g.stack = stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		})
		if err := g.stack.CreateNIC(1, ethernet.New(g.link)); err != nil {
			t.Fatal(err)
		}
		own := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(g.ip.As4()), PrefixLen: subnet.Bits()}
		g.stack.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: own}, stack.AddressProperties{})
		g.stack.SetRouteTable([]tcpip.Route{
			{Destination: own.Subnet(), NIC: 1},
			{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gwIP.As4()), NIC: 1},
		})
		var err error
		g.local, err = netstack.New(netstack.Config{
			Output:    func(f []byte) { g.deliver(f, virtio.Offload{}) },
			Addresses: netstack.Addresses{Subnet: subnet, Gateway: gwIP, DNS: gwIP, Guest: g.ip, GatewayMAC: MAC(gwIP)},
			Resolver:  &Names{Networks: []NameDirectory{table}, Next: failResolver{}},
		})
		if err != nil {
			t.Fatal(err)
		}
		g.sender = &Sender{
			Hello: Hello{Network: "net", From: "c-" + g.ip.String(), FromIP: g.ip.String()},
			Dial: func(ctx context.Context, p Peer) (io.ReadWriteCloser, error) {
				to := byIP[p.IP]
				c1, c2 := net.Pipe()
				go Serve(context.Background(), c2, func(h Hello) (*Router, Peer, error) {
					from, err := PeerFromHello(h)
					return to.router, from, err
				})
				return c1, nil
			},
		}
		g.router = &Router{
			Self: Peer{Container: "c-" + g.ip.String(), IP: g.ip, MAC: MAC(g.ip)}, Gateway: gwIP, GatewayMAC: MAC(gwIP),
			Local: g.local.Inject, Deliver: g.deliver, Dir: newFakeDir(other.ip), Links: g.sender,
		}
		g.router.Start()
	}
	for _, g := range []*testGuest{a, b} {
		go func() {
			for {
				pkt := g.link.ReadContext(ctx)
				if pkt == nil {
					return
				}
				var frame []byte
				for _, s := range pkt.AsSlices() {
					frame = append(frame, s...)
				}
				pkt.DecRef()
				g.router.FromGuest(frame, virtio.Offload{})
			}
		}()
	}
	t.Cleanup(func() {
		for _, g := range []*testGuest{a, b} {
			g.sender.Close()
			g.router.Close()
			g.local.Close()
			g.stack.Close()
		}
	})
	return a, b
}

func full(a netip.Addr, port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(a.As4()), Port: port}
}

// transfer sends size random bytes from one guest to a listener on the
// other and checks what arrives.
func transfer(t *testing.T, from, to *testGuest, size int) {
	t.Helper()
	ln, err := gonet.ListenTCP(to.stack, full(to.ip, 5000), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan [32]byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- [32]byte{}
			return
		}
		defer c.Close()
		h := sha256.New()
		io.Copy(h, c)
		got <- [32]byte(h.Sum(nil))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, from.stack, full(to.ip, 5000), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("%s to %s: %v; %s %s; %s %s", from.ip, to.ip, err, from.router.Stats(), from.sender.Stats(), to.router.Stats(), to.sender.Stats())
	}
	data := make([]byte, size)
	rand.Read(data)
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case sum := <-got:
		if sum != sha256.Sum256(data) {
			t.Errorf("%s to %s: %d bytes arrived different", from.ip, to.ip, size)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s to %s: transfer timed out", from.ip, to.ip)
	}
}

func TestTwoGuestsTCP(t *testing.T) {
	a, b := twoGuests(t)
	start := time.Now()
	transfer(t, a, b, 20<<20)
	transfer(t, b, a, 20<<20)
	t.Logf("40 MB in %v; %s; %s", time.Since(start), a.router.Stats(), a.sender.Stats())
	if a.router.toPeers.Load() == 0 || b.router.fromPeers.Load() == 0 || a.router.arpAnswered.Load() == 0 {
		t.Errorf("frames went another way: %s / %s", a.router.Stats(), b.router.Stats())
	}
}

func TestTwoGuestsUDP(t *testing.T) {
	a, b := twoGuests(t)
	bc, err := gonet.DialUDP(b.stack, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(b.ip.As4()), Port: 6000}, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	to := full(b.ip, 6000)
	ac, err := gonet.DialUDP(a.stack, nil, &to, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	payload := bytes.Repeat([]byte("udp"), 10000) // 30 KB: one datagram
	bc.SetDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, 64<<10)
	for range 50 { // the first may go while ARP is answered
		ac.Write(payload)
		bc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, from, err := bc.ReadFrom(got)
		if err == nil {
			if !bytes.Equal(got[:n], payload) || from.(*net.UDPAddr).IP.String() != a.ip.String() {
				t.Errorf("got %d bytes from %v", n, from)
			}
			return
		}
	}
	t.Fatal("no datagram arrived")
}

// A guest asks the DNS server at its gateway for its peer's name, which
// the network's names answer, and for one it does not know, which the
// host's resolver does not.
func TestTwoGuestsDNS(t *testing.T) {
	a, b := twoGuests(t)
	ask := func(name string, typ dnsmessage.Type) dnsmessage.Message {
		server := full(gwIP, 53)
		c, err := gonet.DialUDP(a.stack, nil, &server, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}}}
		qb, _ := q.Pack()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write(qb)
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(buf[:n]); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, name := range []string{"b.", "B.", "b.net."} {
		m := ask(name, dnsmessage.TypeA)
		if len(m.Answers) != 1 || netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A) != b.ip {
			t.Errorf("%s: %+v", name, m.Answers)
		}
	}
	if m := ask("nosuch.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("nosuch: %v", m.RCode)
	}
	m := ask("3.0.18.172.in-addr.arpa.", dnsmessage.TypePTR)
	if len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.PTRResource).PTR.String() != "b.net." {
		t.Errorf("PTR: %+v", m.Answers)
	}
}
