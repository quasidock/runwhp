//go:build windows

package vmm

import (
	"net"
	"net/netip"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/virtio"
)

// guestNetwork is a guest's network: its network devices, each with the
// stack at the far end of its link, and what the guest is told of them.
type guestNetwork struct {
	// nics are the devices, in the order they are given to the guest.
	nics []*guestNIC
	// specs describe them to init; resolv and hostnameAddrs are its
	// /etc/resolv.conf and the addresses /etc/hosts gives the hostname.
	specs         []guestspec.Network
	resolv        *guestspec.Resolv
	hostnameAddrs []string
}

// guestNIC is one network device and its stack.
type guestNIC struct {
	dev   *virtio.Net
	stack *netstack.Stack
}

// openNetwork sets up the network cfg asks for. With devices false, as
// for a guest serving a stopped container's files, it makes no devices,
// and tells init only what the container's /etc files say. The caller
// closes it once the guest has stopped.
func openNetwork(cfg *container.Config, devices bool, logf func(string, ...any)) (*guestNetwork, error) {
	n := &guestNetwork{}
	if !cfg.Network {
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

// devices are the network devices, for the VM.
func (n *guestNetwork) devices() []virtio.Device {
	var ds []virtio.Device
	for _, nic := range n.nics {
		ds = append(ds, nic.dev)
	}
	return ds
}

// close stops the stacks, logging what they did.
func (n *guestNetwork) close(logf func(string, ...any)) {
	for _, nic := range n.nics {
		logf("%s", nic.stack.Stats())
		nic.stack.Close()
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
