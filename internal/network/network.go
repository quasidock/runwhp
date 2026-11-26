// Package network keeps runwhp's user-defined networks in the store, and
// what the runwhps running containers on them need to find each other.
//
// Layout, under the store's root:
//
//	networks\lock                         locked while a network is made or
//	                                      removed, or addresses handed out
//	networks\names\<name>                 the ID of the network called name
//	networks\<id>\network.json            the network: Network
//	networks\<id>\endpoints\<cid>.json    a container booted on it: Endpoint
//
// A network has a subnet of its own, chosen as it is made from the pools
// Docker's are, past those the host or other networks use, and its gateway
// is the subnet's first address. An endpoint is a container's lease on an
// address, made as its VM boots and removed as it stops, as Docker hands
// out addresses as containers start. The runwhp running the container holds
// the endpoint's file open, sharing it for reading but not deleting, for as
// long as the VM runs: whether anyone can open it for deleting says whether
// it is live, as a container's lock file says whether it runs, and an
// endpoint whose runwhp died is reclaimed by the next lease.
package network

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/netswitch"
)

// Network is a network, as network.json records it.
type Network struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Created time.Time    `json:"created"`
	Driver  string       `json:"driver"`
	Subnet  netip.Prefix `json:"subnet"`
	Gateway netip.Addr   `json:"gateway"`
	// Labels are network create --label's, as Docker keeps them.
	Labels map[string]string `json:"labels,omitempty"`
}

// The built-in networks, which no store records: the default network every
// container is alone on, and none.
const (
	User = "user"
	None = "none"
)

// Builtin is the built-in network called name, or nil. bridge and default
// are other names for user, as Docker calls its default network.
func Builtin(name string) *Network {
	switch name {
	case User, "bridge", "default":
		return &Network{ID: builtinID(User), Name: User, Driver: "user", Subnet: netstack.Subnet, Gateway: netstack.Gateway}
	case None:
		return &Network{ID: builtinID(None), Name: None, Driver: "null"}
	}
	return nil
}

// IsBuiltin reports whether n is one of the built-in networks.
func (n *Network) IsBuiltin() bool { return n.Driver == "user" || n.Driver == "null" }

func builtinID(name string) string {
	sum := sha256.Sum256([]byte("runwhp-network-" + name))
	return hex.EncodeToString(sum[:])
}

// Short is the ID's first 12 digits.
func (n *Network) Short() string { return n.ID[:12] }

// Addresses are the addresses of a guest at ip on the network, as its
// stack serves them: the gateway answers DNS too.
func (n *Network) Addresses(ip netip.Addr) netstack.Addresses {
	return netstack.Addresses{Subnet: n.Subnet, Gateway: n.Gateway, DNS: n.Gateway, Guest: ip, GatewayMAC: netswitch.MAC(n.Gateway)}
}

// Errors, in Docker's words.
var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrPredefined = errors.New("operation is not permitted on a predefined network")
	ErrActive     = errors.New("has active endpoints")
	ErrInUse      = errors.New("address already in use")
	ErrNoSubnet   = errors.New("no subnet left")
)

// NotFound is the error for a network ref does not name.
func NotFound(ref string) error { return fmt.Errorf("network %s %w", ref, ErrNotFound) }

// predefined names a built-in network can be asked for by, and that no
// network of the user's may have.
var predefined = []string{"bridge", "default", "host", None, User}

// predefinedError is Docker's refusal for name, or nil.
func predefinedError(name string) error {
	for _, p := range predefined {
		if strings.EqualFold(name, p) {
			return fmt.Errorf("operation is not permitted on predefined %s network: %w", strings.ToLower(name), ErrPredefined)
		}
	}
	return nil
}

// pools are the subnets networks are given, in order, as Docker's default
// address pools give them: 172.17.0.0/16 to 172.31.0.0/16, then
// 192.168.0.0/16 in /20s. 172.17.0.0/16 is Docker's own bridge's, so the
// first network gets 172.18.0.0/16, as with Docker.
func pools() []netip.Prefix {
	var ps []netip.Prefix
	for b := 18; b <= 31; b++ {
		ps = append(ps, netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(b), 0, 0}), 16))
	}
	for c := 0; c < 256; c += 16 {
		ps = append(ps, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(c), 0}), 20))
	}
	return ps
}

// pickSubnet is the first pool that overlaps none of taken.
func pickSubnet(taken []netip.Prefix) (netip.Prefix, error) {
next:
	for _, p := range pools() {
		for _, t := range taken {
			if p.Overlaps(t) {
				continue next
			}
		}
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("every subnet runwhp gives networks overlaps another network or one of the host's: %w", ErrNoSubnet)
}

// pickAddress is ip if it is free, or the lowest free address in the
// subnet after the gateway, as Docker hands them out. used are the
// addresses live endpoints have.
func pickAddress(n *Network, ip netip.Addr, used map[netip.Addr]bool) (netip.Addr, error) {
	last := lastAddr(n.Subnet)
	if ip.IsValid() {
		if !n.Subnet.Contains(ip) || ip == n.Subnet.Addr() || ip == last {
			return netip.Addr{}, fmt.Errorf("invalid endpoint settings:\nno configured subnet contains IP address %s", ip)
		}
		if ip == n.Gateway || used[ip] {
			return netip.Addr{}, fmt.Errorf("address %s on network %s: %w", ip, n.Name, ErrInUse)
		}
		return ip, nil
	}
	for a := n.Gateway.Next(); a.IsValid() && a != last && n.Subnet.Contains(a); a = a.Next() {
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no available IPv4 addresses on network %s", n.Name)
}

// lastAddr is the subnet's broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= 1<<(32-p.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Size is how many addresses the subnet has, and InUse how many a network
// with n live endpoints uses, counting its own address, its gateway and
// its broadcast address, as Docker counts them.
func (n *Network) Size() uint64 {
	if !n.Subnet.IsValid() {
		return 0
	}
	return 1 << (32 - n.Subnet.Bits())
}

// SortByName sorts networks by name, as Docker's CLI lists them.
func SortByName(ns []*Network) {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
}

// Endpoint is a container's lease on an address on a network, as its
// endpoint file records it.
type Endpoint struct {
	Network   string     `json:"network"`
	Container string     `json:"container"`
	ID        string     `json:"endpointID"`
	IP        netip.Addr `json:"ip"`
	Hostname  string     `json:"hostname,omitempty"`
	Aliases   []string   `json:"aliases,omitempty"`
	// Pipe is where the runwhp running the container takes links.
	Pipe string `json:"pipe"`
}

// MAC is the endpoint's MAC address.
func (e Endpoint) MAC() [6]byte { return netswitch.MAC(e.IP) }

// Peer is the endpoint as a router sees it.
func (e Endpoint) Peer() netswitch.Peer {
	return netswitch.Peer{Container: e.Container, IP: e.IP, MAC: e.MAC()}
}
