// Package netswitch joins guests on a user-defined network without a switch
// of their own: each guest's VMM routes its guest's Ethernet frames for the
// others straight to their VMMs, and answers its guest's ARP requests for
// them. Frames for anything else, the gateway, its DNS server and the
// outside, go to the guest's own user-mode stack, as on runwhp's default
// network.
//
// A Router sits on one guest's link to one network. FromGuest takes the
// guest's frames on the vCPU thread, so it never blocks, never touches the
// disk, and never hands a frame back to the guest itself: it only looks
// peers up in a Directory and hands frames to Links, which carry them on
// goroutines of their own. Each frame for a peer is classified by its
// Ethernet header:
//
//   - ARP for the gateway, and frames for the gateway's MAC, go to the local
//     stack; ARP requests for a live peer are answered from a goroutine, with
//     the peer's MAC, which is derived from its address; other ARP is
//     dropped, and a peer that is not known yet is looked for.
//   - Broadcast and IPv4 multicast frames go to every live peer, as a
//     bridge floods them, and not to the local stack, which takes none.
//   - Frames for a peer's MAC go to that peer, if it is live, and only if
//     the guest sent them from its own MAC.
//   - Anything else is dropped.
//
// Frames from peers reach the guest only from the peer's own MAC, and only
// for the guest's MAC, broadcast or multicast; a partial checksum a peer
// left goes on to the guest as it is, or is finished for one that cannot
// take it, by the device.
package netswitch

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/quasidock/runwhp/internal/virtio"
)

// Peer is a guest on a network: its container, address and MAC.
type Peer struct {
	Container string
	IP        netip.Addr
	MAC       [6]byte
}

// MAC is the MAC address of a guest at ip on a user-defined network:
// 02:42 and the address, as Docker's were. Every address has one, and the
// router reads a frame's destination address back from it.
func MAC(ip netip.Addr) [6]byte {
	a := ip.As4()
	return [6]byte{0x02, 0x42, a[0], a[1], a[2], a[3]}
}

// MACString formats a MAC address as Docker does.
func MACString(m [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])
}

// ipOfMAC is the address MAC derived m from, if it was.
func ipOfMAC(m []byte) (netip.Addr, bool) {
	if m[0] != 0x02 || m[1] != 0x42 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{m[2], m[3], m[4], m[5]}), true
}

// Directory knows the live guests on a network.
type Directory interface {
	// Lookup finds the live guest at ip other than this one. It must not
	// block: when it does not know ip, it may look for it in the
	// background, and say no meanwhile.
	Lookup(ip netip.Addr) (Peer, bool)
	// Peers are every live guest other than this one; it must not block.
	Peers() []Peer
}

// Links carry frames to peers.
type Links interface {
	// Send queues frame for p, taking ownership of it, which may be shared
	// with other peers' sends: none may change it. It must not block: a
	// frame that cannot be queued is dropped, as a switch drops one.
	Send(p Peer, frame []byte, off virtio.Offload)
}

// Router routes one guest's frames on one network. Set its fields, then
// Start it; Close stops it.
type Router struct {
	// Self is this guest.
	Self Peer
	// Gateway is the network's gateway, which the local stack answers at,
	// with GatewayMAC.
	Gateway    netip.Addr
	GatewayMAC [6]byte
	// Local hands a frame to the local stack; it must not block.
	Local func(frame []byte)
	// Deliver hands a frame to the guest; it must not block.
	Deliver func(frame []byte, off virtio.Offload)
	Dir     Directory
	Links   Links

	arp       chan arpRequest
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	// Counters, for Stats.
	toLocal, toPeers, flooded, fromPeers, arpAnswered, dropped, refused atomic.Uint64
}

type arpRequest struct {
	peer Peer
	sha  [6]byte
	spa  [4]byte
}

const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806
	arpLen    = 28
)

var broadcast = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// Start starts the goroutine that answers the guest's ARP requests.
func (r *Router) Start() {
	r.arp = make(chan arpRequest, 64)
	r.done = make(chan struct{})
	r.wg.Add(1)
	go r.answerARP()
}

// Close stops the router. Frames that arrive after it are dropped.
func (r *Router) Close() {
	r.closeOnce.Do(func() {
		close(r.done)
		r.wg.Wait()
	})
}

// Stats summarises what the router did.
func (r *Router) Stats() string {
	return fmt.Sprintf("network %s: %d frames to the gateway, %d to peers, %d flooded, %d from peers, %d ARP answers, %d dropped, %d refused from peers",
		r.Self.IP, r.toLocal.Load(), r.toPeers.Load(), r.flooded.Load(), r.fromPeers.Load(), r.arpAnswered.Load(), r.dropped.Load(), r.refused.Load())
}

// FromGuest routes a frame the guest sent. It runs on the vCPU thread.
func (r *Router) FromGuest(frame []byte, off virtio.Offload) {
	if len(frame) < 14 {
		r.local(frame)
		return
	}
	dst := [6]byte(frame[0:6])
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case etherARP:
		r.fromGuestARP(frame)
		return
	case etherIPv4:
	default:
		// The local stack takes IPv4 and ARP alone, and drops the rest.
		r.local(frame)
		return
	}
	switch {
	case dst == r.GatewayMAC:
		r.local(frame)
	case [6]byte(frame[6:12]) != r.Self.MAC:
		r.dropped.Add(1)
	case dst == broadcast || dst[0] == 0x01 && dst[1] == 0x00 && dst[2] == 0x5e:
		peers := r.Dir.Peers()
		for _, p := range peers {
			r.Links.Send(p, frame, off)
		}
		r.flooded.Add(1)
	default:
		ip, ok := ipOfMAC(dst[:])
		if !ok {
			r.dropped.Add(1)
			return
		}
		p, ok := r.Dir.Lookup(ip)
		if !ok || p.MAC != dst {
			r.dropped.Add(1)
			return
		}
		r.Links.Send(p, frame, off)
		r.toPeers.Add(1)
	}
}

func (r *Router) local(frame []byte) {
	r.toLocal.Add(1)
	r.Local(frame)
}

// fromGuestARP handles the guest's ARP: for the gateway, the local stack
// answers; for a peer, the router does.
func (r *Router) fromGuestARP(frame []byte) {
	a := frame[14:]
	if len(a) < arpLen || binary.BigEndian.Uint16(a[0:2]) != 1 || binary.BigEndian.Uint16(a[2:4]) != etherIPv4 || a[4] != 6 || a[5] != 4 {
		r.dropped.Add(1)
		return
	}
	op := binary.BigEndian.Uint16(a[6:8])
	tpa := netip.AddrFrom4([4]byte(a[24:28]))
	switch {
	case [6]byte(frame[0:6]) == r.GatewayMAC, tpa == r.Gateway:
		// The gateway's: a request for it, or the guest's answer to the
		// stack's request for the guest.
		r.local(frame)
	case op != 1 || [6]byte(a[8:14]) != r.Self.MAC || [6]byte(frame[6:12]) != r.Self.MAC:
		r.dropped.Add(1)
	default:
		p, ok := r.Dir.Lookup(tpa)
		if !ok {
			// Gratuitous, for itself, or for a guest not known yet: the
			// guest asks again, by when the directory may know it.
			r.dropped.Add(1)
			return
		}
		select {
		case r.arp <- arpRequest{peer: p, sha: r.Self.MAC, spa: [4]byte(a[14:18])}:
		default:
			r.dropped.Add(1)
		}
	}
}

// answerARP answers the guest's ARP requests for peers. Deliver cannot be
// called on the vCPU thread, from FromGuest.
func (r *Router) answerARP() {
	defer r.wg.Done()
	for {
		select {
		case req := <-r.arp:
			r.Deliver(arpReply(req), virtio.Offload{})
			r.arpAnswered.Add(1)
		case <-r.done:
			return
		}
	}
}

// arpReply is the ARP reply req's peer would send.
func arpReply(req arpRequest) []byte {
	f := make([]byte, 14+arpLen)
	copy(f[0:6], req.sha[:])
	copy(f[6:12], req.peer.MAC[:])
	binary.BigEndian.PutUint16(f[12:], etherARP)
	a := f[14:]
	binary.BigEndian.PutUint16(a[0:], 1)
	binary.BigEndian.PutUint16(a[2:], etherIPv4)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:], 2)
	copy(a[8:14], req.peer.MAC[:])
	ip := req.peer.IP.As4()
	copy(a[14:18], ip[:])
	copy(a[18:24], req.sha[:])
	copy(a[24:28], req.spa[:])
	return f
}

// FromPeer delivers a frame from a peer to the guest: from the peer's own
// MAC, to the guest's, broadcast or multicast, with an offload that fits.
func (r *Router) FromPeer(from Peer, frame []byte, off virtio.Offload) {
	if len(frame) < 14 || [6]byte(frame[6:12]) != from.MAC || !off.Fits(len(frame)) {
		r.refused.Add(1)
		return
	}
	dst := [6]byte(frame[0:6])
	if dst != r.Self.MAC && dst != broadcast && !(dst[0] == 0x01 && dst[1] == 0x00 && dst[2] == 0x5e) {
		r.refused.Add(1)
		return
	}
	select {
	case <-r.done:
		return
	default:
	}
	r.fromPeers.Add(1)
	r.Deliver(frame, off)
}
