//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// configureNetworks gives each interface the spec names, by its MAC
// address, its address and MTU, brings it up, and adds the default route
// through the one with a gateway. The kernel adds the route to each
// interface's own subnet. The container runs in init's network namespace,
// so this is the container's network.
func configureNetworks(ns []guestspec.Network) error {
	macs := make([]string, len(ns))
	for i, n := range ns {
		mac, err := net.ParseMAC(n.MAC)
		if err != nil {
			return fmt.Errorf("network spec: %w", err)
		}
		macs[i] = mac.String()
	}
	// virtio_net is built into the kernel, so the interfaces normally exist
	// before init starts; wait a little in case they do not.
	ifaces := make([]net.Interface, len(ns))
	found := poll(5*time.Second, func() bool {
		all, _ := net.Interfaces()
		clear(ifaces)
		missing := len(ns)
		for i, mac := range macs {
			for _, iface := range all {
				if iface.HardwareAddr.String() == mac {
					ifaces[i] = iface
					missing--
					break
				}
			}
		}
		return missing == 0
	})
	if !found {
		for i, iface := range ifaces {
			if iface.Name == "" {
				return fmt.Errorf("no network interface with address %s appeared; "+
					"if the kernel lacks virtio_net, pass --network none", ns[i].MAC)
			}
		}
	}
	for i, n := range ns {
		if err := configureInterface(ifaces[i], n); err != nil {
			return err
		}
	}
	return nil
}

// configureInterface gives one interface its address, MTU and, if n has a
// gateway, the default route, and brings it up.
func configureInterface(iface net.Interface, n guestspec.Network) error {
	prefix, err := netip.ParsePrefix(n.Address)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("network spec: bad address %q", n.Address)
	}
	var gateway netip.Addr
	if n.Gateway != "" {
		if gateway, err = netip.ParseAddr(n.Gateway); err != nil || !gateway.Is4() {
			return fmt.Errorf("network spec: bad gateway %q", n.Gateway)
		}
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifreq := func(req uint, set func(*unix.Ifreq) error) error {
		ifr, err := unix.NewIfreq(iface.Name)
		if err != nil {
			return err
		}
		if err := set(ifr); err != nil {
			return err
		}
		return unix.IoctlIfreq(fd, req, ifr)
	}
	if n.MTU > 0 {
		if err := ifreq(unix.SIOCSIFMTU, func(ifr *unix.Ifreq) error { ifr.SetUint32(uint32(n.MTU)); return nil }); err != nil {
			return fmt.Errorf("setting the MTU of %s to %d: %w", iface.Name, n.MTU, err)
		}
	}
	ip := prefix.Addr().As4()
	if err := ifreq(unix.SIOCSIFADDR, func(ifr *unix.Ifreq) error { return ifr.SetInet4Addr(ip[:]) }); err != nil {
		return fmt.Errorf("setting the address of %s: %w", iface.Name, err)
	}
	mask := net.CIDRMask(prefix.Bits(), 32)
	if err := ifreq(unix.SIOCSIFNETMASK, func(ifr *unix.Ifreq) error { return ifr.SetInet4Addr(mask) }); err != nil {
		return fmt.Errorf("setting the netmask of %s: %w", iface.Name, err)
	}
	if err := linkUp(fd, iface.Name); err != nil {
		return fmt.Errorf("bringing up %s: %w", iface.Name, err)
	}
	if gateway.IsValid() {
		if err := addDefaultRoute(gateway.As4(), uint32(iface.Index)); err != nil {
			return fmt.Errorf("adding the default route via %s: %w", gateway, err)
		}
	}
	debugf("network: %s is %s, gateway %s, mtu %d", iface.Name, prefix, n.Gateway, n.MTU)
	return nil
}

// linkUp sets IFF_UP on an interface, using an AF_INET datagram socket fd.
func linkUp(fd int, name string) error {
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return linkUp(fd, "lo")
}

// addDefaultRoute adds 0.0.0.0/0 via gateway out of interface ifindex, with
// one rtnetlink request, and waits for the kernel's answer.
func addDefaultRoute(gateway [4]byte, ifindex uint32) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, routeMessage(gateway, ifindex), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Header.Type == unix.NLMSG_ERROR && len(m.Data) >= 4 {
			if errno := int32(binary.NativeEndian.Uint32(m.Data)); errno != 0 {
				return syscall.Errno(-errno)
			}
			return nil
		}
	}
	return fmt.Errorf("no answer from the kernel")
}

// routeMessage encodes RTM_NEWROUTE for a default IPv4 route: a netlink
// header, a struct rtmsg, then RTA_GATEWAY and RTA_OIF attributes.
func routeMessage(gateway [4]byte, ifindex uint32) []byte {
	ne := binary.NativeEndian
	b := make([]byte, 0, 44)
	b = ne.AppendUint32(b, 44) // nlmsg_len
	b = ne.AppendUint16(b, unix.RTM_NEWROUTE)
	b = ne.AppendUint16(b, unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL)
	b = ne.AppendUint32(b, 1) // nlmsg_seq
	b = ne.AppendUint32(b, 0) // nlmsg_pid
	b = append(b,
		unix.AF_INET,           // rtm_family
		0,                      // rtm_dst_len: the default route
		0,                      // rtm_src_len
		0,                      // rtm_tos
		unix.RT_TABLE_MAIN,     // rtm_table
		unix.RTPROT_BOOT,       // rtm_protocol
		unix.RT_SCOPE_UNIVERSE, // rtm_scope
		unix.RTN_UNICAST,       // rtm_type
	)
	b = ne.AppendUint32(b, 0) // rtm_flags
	b = ne.AppendUint16(b, 8)
	b = ne.AppendUint16(b, unix.RTA_GATEWAY)
	b = append(b, gateway[:]...)
	b = ne.AppendUint16(b, 8)
	b = ne.AppendUint16(b, unix.RTA_OIF)
	b = ne.AppendUint32(b, ifindex)
	return b
}
