//go:build windows

package vmm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/netswitch"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/virtio"
)

// guestNetwork is a guest's network: its network devices, each with the
// stack at the far end of its link and, on a user-defined network, the
// router that joins it to the other guests there, and what the guest is
// told of them.
type guestNetwork struct {
	// nics are the devices, in the order they are given to the guest.
	nics []*guestNIC
	// specs describe them to init; resolv and hostnameAddrs are its
	// /etc/resolv.conf and the addresses /etc/hosts gives the hostname.
	specs         []guestspec.Network
	resolv        *guestspec.Resolv
	hostnameAddrs []string

	// On user-defined networks: the address leases, the listener peers'
	// links arrive at, and its connections' goroutines.
	leases []*network.Lease
	ln     net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// guestNIC is one network device and what serves it.
type guestNIC struct {
	dev    *virtio.Net
	stack  *netstack.Stack
	router *netswitch.Router  // nil on the default network
	sender *netswitch.Sender  // nil on the default network
	dir    *network.Directory // nil on the default network
}

// openNetwork sets up the network cfg asks for. With devices false, as
// for a guest serving a stopped container's files, it makes no devices,
// and tells init only what the container's /etc files say. The caller
// closes it once the guest has stopped.
func openNetwork(root string, cfg *container.Config, devices bool, logf func(string, ...any)) (*guestNetwork, error) {
	n := &guestNetwork{}
	switch {
	case !cfg.Network:
		return n, nil
	case len(cfg.Networks) > 0:
		if !devices {
			n.resolv = userResolv(root, cfg)
			return n, nil
		}
		if err := n.join(root, cfg, logf); err != nil {
			n.close(logf)
			return nil, err
		}
		return n, nil
	}
	n.resolv = &guestspec.Resolv{Nameservers: []string{netstack.DNS.String()}}
	if !devices {
		return n, nil
	}
	dev := &virtio.Net{MAC: netstack.GuestMAC, MTU: netstack.MTU}
	stack, err := netstack.New(netstack.Config{
		Output:         dev.Deliver,
		GuestChecksums: dev.GuestChecksums,
		HostGateway:    cfg.HostGateway,
		Publish:        cfg.Publish,
		Refuse:         network.Open(root).Subnets(),
		Logf:           logf,
	})
	if err != nil {
		return nil, err
	}
	// The stack checks no checksums, so a frame's offload means nothing
	// to it.
	dev.Recv = func(frame []byte, _ virtio.Offload) { stack.Inject(frame) }
	n.nics = append(n.nics, &guestNIC{dev: dev, stack: stack})
	n.specs = append(n.specs, guestspec.Network{
		MAC:     net.HardwareAddr(netstack.GuestMAC[:]).String(),
		Address: netip.PrefixFrom(netstack.Guest, netstack.Subnet.Bits()).String(),
		Gateway: netstack.Gateway.String(),
		MTU:     netstack.MTU,
	})
	return n, nil
}

// userResolv is the resolv.conf of a container on user-defined networks,
// for a guest that has no network devices.
func userResolv(root string, cfg *container.Config) *guestspec.Resolv {
	nw, err := network.Open(root).Get(cfg.Networks[0].Network)
	if err != nil {
		return &guestspec.Resolv{Options: []string{"ndots:0"}}
	}
	return userResolvFor(nw.Gateway)
}

// userResolvFor is the resolv.conf of a container whose primary network's
// gateway is gateway: the gateway answers DNS, for the names of every
// network the container is on, through init's embedded server at
// 127.0.0.11, as Docker's answers there, or asked directly should that
// server not start; and names are tried as given first, as Docker's
// ndots:0 has them.
func userResolvFor(gateway netip.Addr) *guestspec.Resolv {
	return &guestspec.Resolv{
		Nameservers: []string{gateway.String()},
		Embedded:    gateway.String(),
		Options:     []string{"ndots:0"},
	}
}

// join leases the container an address on each of its networks, as its VM
// boots, and makes a device for each, with a stack for its gateway and a
// router for its peers.
func (n *guestNetwork) join(root string, cfg *container.Config, logf func(string, ...any)) error {
	st := network.Open(root)
	ln, pipe, err := network.Listen(root, cfg.ID)
	if err != nil {
		return fmt.Errorf("taking links from other containers: %w", err)
	}
	n.ln = ln
	var reqs []network.Request
	for _, a := range cfg.Networks {
		r := network.Request{Network: a.Network, Name: a.Name, Aliases: a.Aliases}
		if a.IPv4 != "" {
			if r.IP, err = netip.ParseAddr(a.IPv4); err != nil {
				return fmt.Errorf("network %s: bad address %q", a.Name, a.IPv4)
			}
		}
		reqs = append(reqs, r)
	}
	if n.leases, err = st.Join(cfg.ID, cfg.Hostname, pipe, reqs); err != nil {
		return fmt.Errorf("failed to set up container networking: %w", err)
	}

	refuse := st.Subnets()
	var dirs []netswitch.NameDirectory
	for _, l := range n.leases {
		d := st.Directory(l.Net, cfg.ID)
		d.Refresh()
		dirs = append(dirs, d)
		n.nics = append(n.nics, &guestNIC{dir: d})
	}
	names := &netswitch.Names{Networks: dirs, Next: net.DefaultResolver}
	routers := map[string]*guestNIC{}
	for i, l := range n.leases {
		nic := n.nics[i]
		var publish []netstack.Port
		if i == 0 {
			publish = cfg.Publish
		}
		nic.dev = &virtio.Net{MAC: l.MAC(), MTU: netstack.MTU}
		if nic.stack, err = netstack.New(netstack.Config{
			Output:         nic.dev.Deliver,
			GuestChecksums: nic.dev.GuestChecksums,
			Addresses:      l.Net.Addresses(l.IP),
			Refuse:         refuse,
			HostGateway:    cfg.HostGateway,
			Publish:        publish,
			Resolver:       names,
			Logf:           logf,
		}); err != nil {
			return err
		}
		nic.sender = &netswitch.Sender{
			Hello:  netswitch.Hello{Network: l.Net.ID, From: cfg.ID, FromIP: l.IP.String()},
			Dial:   st.Dialer(l.Net.ID),
			Failed: nic.dir.Failed,
			Logf:   logf,
		}
		nic.router = &netswitch.Router{
			Self: l.Peer(), Gateway: l.Net.Gateway, GatewayMAC: netswitch.MAC(l.Net.Gateway),
			Local: nic.stack.Inject, Deliver: nic.dev.DeliverWith, Dir: nic.dir, Links: nic.sender,
		}
		nic.router.Start()
		nic.dev.Recv = nic.router.FromGuest
		routers[l.Net.ID] = nic

		spec := guestspec.Network{
			MAC:     netswitch.MACString(l.MAC()),
			Address: netip.PrefixFrom(l.IP, l.Net.Subnet.Bits()).String(),
			MTU:     netstack.MTU,
		}
		if i == 0 {
			spec.Gateway = l.Net.Gateway.String()
		}
		n.specs = append(n.specs, spec)
		n.hostnameAddrs = append(n.hostnameAddrs, l.IP.String())
		logf("network %s: %s, gateway %s", l.Net.Name, spec.Address, l.Net.Gateway)
	}
	n.resolv = userResolvFor(n.leases[0].Net.Gateway)

	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	accept := func(h netswitch.Hello) (*netswitch.Router, netswitch.Peer, error) {
		nic := routers[h.Network]
		if nic == nil || h.To != cfg.ID {
			return nil, netswitch.Peer{}, errors.New("not on that network")
		}
		from, err := netswitch.PeerFromHello(h)
		if err != nil {
			return nil, netswitch.Peer{}, err
		}
		// A link from a guest the directory does not know yet, as one just
		// booted: look again.
		p, ok := nic.dir.Lookup(from.IP)
		if !ok || p.Container != from.Container {
			nic.dir.Refresh()
			p, ok = nic.dir.Lookup(from.IP)
		}
		if !ok || p.Container != from.Container {
			return nil, netswitch.Peer{}, fmt.Errorf("%s is not on the network at %s", from.Container, from.IP)
		}
		return nic.router, from, nil
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				if err := netswitch.Serve(ctx, c, accept); err != nil {
					logf("link: %v", err)
				}
			}()
		}
	}()
	return nil
}

// devices are the network devices, for the VM.
func (n *guestNetwork) devices() []virtio.Device {
	var ds []virtio.Device
	for _, nic := range n.nics {
		ds = append(ds, nic.dev)
	}
	return ds
}

// published is what the first device's stack publishes on the host, as
// bound; the others publish nothing.
func (n *guestNetwork) published() []netstack.Port {
	if n == nil || len(n.nics) == 0 || n.nics[0].stack == nil {
		return nil
	}
	return n.nics[0].stack.Published()
}

// close stops taking links, closes the links and the stacks, logging what
// they did, and gives the leases back, once the guest has stopped.
func (n *guestNetwork) close(logf func(string, ...any)) {
	if n.ln != nil {
		n.ln.Close()
	}
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
	for _, nic := range n.nics {
		if nic.sender != nil {
			nic.sender.Close()
			logf("%s", nic.sender.Stats())
		}
		if nic.router != nil {
			nic.router.Close()
			logf("%s", nic.router.Stats())
		}
		if nic.stack != nil {
			logf("%s", nic.stack.Stats())
			nic.stack.Close()
		}
		if nic.dir != nil {
			nic.dir.Close()
		}
	}
	for _, l := range n.leases {
		l.Release()
	}
}

// traffic is the bytes the guest has received and sent on all its devices.
func (n *guestNetwork) traffic() (rx, tx uint64) {
	if n == nil {
		return 0, 0
	}
	for _, nic := range n.nics {
		rx += nic.dev.RxBytes.Load()
		tx += nic.dev.TxBytes.Load()
	}
	return rx, tx
}
