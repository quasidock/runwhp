package netswitch

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/virtio"
)

var (
	gwIP   = netip.MustParseAddr("172.18.0.1")
	selfIP = netip.MustParseAddr("172.18.0.2")
	peerIP = netip.MustParseAddr("172.18.0.3")
	peer2  = netip.MustParseAddr("172.18.0.4")
)

// fakeDir is a Directory over a fixed set of peers, counting misses.
type fakeDir struct {
	mu     sync.Mutex
	peers  map[netip.Addr]Peer
	misses int
}

func newFakeDir(ips ...netip.Addr) *fakeDir {
	d := &fakeDir{peers: map[netip.Addr]Peer{}}
	for _, ip := range ips {
		d.peers[ip] = Peer{Container: "c-" + ip.String(), IP: ip, MAC: MAC(ip)}
	}
	return d
}

func (d *fakeDir) Lookup(ip netip.Addr) (Peer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[ip]
	if !ok {
		d.misses++
	}
	return p, ok
}

func (d *fakeDir) Peers() []Peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ps []Peer
	for _, p := range d.peers {
		ps = append(ps, p)
	}
	return ps
}

// sent is one frame a fakeLinks was given.
type sent struct {
	to    Peer
	frame []byte
	off   virtio.Offload
}

type fakeLinks struct {
	mu   sync.Mutex
	sent []sent
}

func (l *fakeLinks) Send(p Peer, frame []byte, off virtio.Offload) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, sent{p, frame, off})
}

func (l *fakeLinks) take() []sent {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sent
	l.sent = nil
	return s
}

// testRouter is a router with fakes all round. Deliver panics while
// FromGuest runs, as a device's Recv must never deliver.
type testRouter struct {
	*Router
	dir       *fakeDir
	links     *fakeLinks
	inGuest   atomic.Bool
	mu        sync.Mutex
	local     [][]byte
	delivered chan []byte
	offs      chan virtio.Offload
}

func newTestRouter(t *testing.T, peers ...netip.Addr) *testRouter {
	tr := &testRouter{dir: newFakeDir(peers...), links: &fakeLinks{}, delivered: make(chan []byte, 16), offs: make(chan virtio.Offload, 16)}
	tr.Router = &Router{
		Self: Peer{Container: "self", IP: selfIP, MAC: MAC(selfIP)}, Gateway: gwIP, GatewayMAC: MAC(gwIP),
		Local: func(f []byte) {
			tr.mu.Lock()
			tr.local = append(tr.local, f)
			tr.mu.Unlock()
		},
		Deliver: func(f []byte, off virtio.Offload) {
			if tr.inGuest.Load() {
				panic("Deliver called from FromGuest")
			}
			tr.delivered <- f
			tr.offs <- off
		},
		Dir: tr.dir, Links: tr.links,
	}
	tr.Start()
	t.Cleanup(tr.Close)
	return tr
}

func (tr *testRouter) fromGuest(f []byte, off virtio.Offload) {
	tr.inGuest.Store(true)
	defer tr.inGuest.Store(false)
	tr.FromGuest(f, off)
}

func (tr *testRouter) takeLocal() [][]byte {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	l := tr.local
	tr.local = nil
	return l
}

func ether(dst, src [6]byte, typ uint16, payload []byte) []byte {
	f := make([]byte, 14, 14+len(payload))
	copy(f[0:], dst[:])
	copy(f[6:], src[:])
	binary.BigEndian.PutUint16(f[12:], typ)
	return append(f, payload...)
}

func arpPacket(op uint16, sha [6]byte, spa netip.Addr, tha [6]byte, tpa netip.Addr) []byte {
	a := make([]byte, arpLen)
	binary.BigEndian.PutUint16(a[0:], 1)
	binary.BigEndian.PutUint16(a[2:], etherIPv4)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:], op)
	copy(a[8:], sha[:])
	s, tp := spa.As4(), tpa.As4()
	copy(a[14:], s[:])
	copy(a[18:], tha[:])
	copy(a[24:], tp[:])
	return a
}

func ipv4Packet(n int) []byte { return make([]byte, n) }

func TestRouterARP(t *testing.T) {
	tr := newTestRouter(t, peerIP)
	self := tr.Self.MAC

	// For a peer: answered by the router, with the peer's MAC, from a
	// goroutine of its own.
	tr.fromGuest(ether(broadcast, self, etherARP, arpPacket(1, self, selfIP, [6]byte{}, peerIP)), virtio.Offload{})
	select {
	case f := <-tr.delivered:
		<-tr.offs
		a := f[14:]
		if [6]byte(f[0:6]) != self || [6]byte(f[6:12]) != MAC(peerIP) || binary.BigEndian.Uint16(a[6:]) != 2 ||
			[6]byte(a[8:14]) != MAC(peerIP) || netip.AddrFrom4([4]byte(a[14:18])) != peerIP ||
			[6]byte(a[18:24]) != self || netip.AddrFrom4([4]byte(a[24:28])) != selfIP {
			t.Errorf("ARP reply % x", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ARP reply")
	}

	// For the gateway, and the guest's answer to the stack: the stack's.
	tr.fromGuest(ether(broadcast, self, etherARP, arpPacket(1, self, selfIP, [6]byte{}, gwIP)), virtio.Offload{})
	tr.fromGuest(ether(MAC(gwIP), self, etherARP, arpPacket(2, self, selfIP, MAC(gwIP), gwIP)), virtio.Offload{})
	if l := tr.takeLocal(); len(l) != 2 {
		t.Errorf("%d ARP frames to the stack, want 2", len(l))
	}

	// For an unknown address, for itself, and one claiming another MAC:
	// dropped; the unknown one looked for.
	tr.fromGuest(ether(broadcast, self, etherARP, arpPacket(1, self, selfIP, [6]byte{}, peer2)), virtio.Offload{})
	tr.fromGuest(ether(broadcast, self, etherARP, arpPacket(1, self, selfIP, [6]byte{}, selfIP)), virtio.Offload{})
	other := MAC(peer2)
	tr.fromGuest(ether(broadcast, other, etherARP, arpPacket(1, other, selfIP, [6]byte{}, peerIP)), virtio.Offload{})
	tr.fromGuest(ether(broadcast, self, etherARP, []byte{1, 2, 3}), virtio.Offload{})
	select {
	case f := <-tr.delivered:
		t.Errorf("answered % x", f)
	case <-time.After(100 * time.Millisecond):
	}
	if tr.dir.misses != 2 || tr.dropped.Load() != 4 || len(tr.takeLocal()) != 0 || len(tr.links.take()) != 0 {
		t.Errorf("misses %d dropped %d", tr.dir.misses, tr.dropped.Load())
	}
}

func TestRouterFromGuest(t *testing.T) {
	tr := newTestRouter(t, peerIP, peer2)
	self := tr.Self.MAC
	off := virtio.Offload{NeedsCsum: true, Start: 34, Offset: 16}

	// To a peer, with its offload.
	tr.fromGuest(ether(MAC(peerIP), self, etherIPv4, ipv4Packet(60)), off)
	if s := tr.links.take(); len(s) != 1 || s[0].to.IP != peerIP || s[0].off != off {
		t.Fatalf("to a peer: %+v", s)
	}
	// To the gateway, and other ethertypes: the stack's.
	tr.fromGuest(ether(MAC(gwIP), self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest(ether(broadcast, self, 0x86dd, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest([]byte{1, 2, 3}, virtio.Offload{})
	if l := tr.takeLocal(); len(l) != 3 {
		t.Errorf("%d frames to the stack, want 3", len(l))
	}
	// Broadcast and IPv4 multicast: to every peer, not the stack.
	tr.fromGuest(ether(broadcast, self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest(ether([6]byte{0x01, 0, 0x5e, 0, 0, 0xfb}, self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	if s := tr.links.take(); len(s) != 4 {
		t.Errorf("flooded to %d, want 2 peers twice", len(s))
	}
	if l := tr.takeLocal(); len(l) != 0 {
		t.Errorf("broadcast reached the stack")
	}
	// Spoofed source MAC, an unknown peer, a MAC no address made, IPv6
	// multicast: dropped.
	tr.fromGuest(ether(MAC(peerIP), MAC(peer2), etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest(ether(MAC(netip.MustParseAddr("172.18.0.9")), self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest(ether([6]byte{0x0e, 1, 2, 3, 4, 5}, self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.fromGuest(ether([6]byte{0x33, 0x33, 0, 0, 0, 1}, self, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	if s := tr.links.take(); len(s) != 0 || tr.dropped.Load() != 4 {
		t.Errorf("sent %d, dropped %d", len(s), tr.dropped.Load())
	}
}

func TestRouterFromPeer(t *testing.T) {
	tr := newTestRouter(t, peerIP)
	from := Peer{Container: "c", IP: peerIP, MAC: MAC(peerIP)}
	off := virtio.Offload{NeedsCsum: true, Start: 34, Offset: 6}

	tr.FromPeer(from, ether(tr.Self.MAC, from.MAC, etherIPv4, ipv4Packet(60)), off)
	tr.FromPeer(from, ether(broadcast, from.MAC, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.FromPeer(from, ether([6]byte{0x01, 0, 0x5e, 1, 2, 3}, from.MAC, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	for i := range 3 {
		select {
		case <-tr.delivered:
			if o := <-tr.offs; i == 0 && o != off {
				t.Errorf("offload %+v", o)
			}
		case <-time.After(time.Second):
			t.Fatalf("frame %d not delivered", i)
		}
	}
	// From another MAC, to another guest, an offload past the frame, a
	// runt: refused.
	tr.FromPeer(from, ether(tr.Self.MAC, MAC(peer2), etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.FromPeer(from, ether(MAC(peer2), from.MAC, etherIPv4, ipv4Packet(60)), virtio.Offload{})
	tr.FromPeer(from, ether(tr.Self.MAC, from.MAC, etherIPv4, ipv4Packet(10)), virtio.Offload{NeedsCsum: true, Start: 20, Offset: 6})
	tr.FromPeer(from, []byte{1}, virtio.Offload{})
	if len(tr.delivered) != 0 || tr.refused.Load() != 4 {
		t.Errorf("delivered %d, refused %d", len(tr.delivered), tr.refused.Load())
	}
}

func TestMAC(t *testing.T) {
	m := MAC(netip.MustParseAddr("172.18.0.10"))
	if MACString(m) != "02:42:ac:12:00:0a" {
		t.Errorf("MAC %s", MACString(m))
	}
	if ip, ok := ipOfMAC(m[:]); !ok || ip.String() != "172.18.0.10" {
		t.Errorf("back to %v %v", ip, ok)
	}
}
