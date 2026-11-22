//go:build windows

package main

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
)

// networkOptions is what the network flags ask for.
type networkOptions struct {
	enabled     bool
	publish     []netstack.Port
	hosts       []string // /etc/hosts lines
	hostGateway bool
}

// parseNetworkFlags checks --network, -p and --add-host together.
func parseNetworkFlags(mode string, publish, addHosts []string) (networkOptions, error) {
	var n networkOptions
	switch mode {
	case "user":
		n.enabled = true
	case "none":
	default:
		return n, fmt.Errorf("network %q is not supported; use user (the default) or none", mode)
	}
	for _, s := range publish {
		p, err := parsePublish(s)
		if err != nil {
			return n, err
		}
		n.publish = append(n.publish, p)
	}
	for _, s := range addHosts {
		line, gateway, err := parseAddHost(s)
		if err != nil {
			return n, err
		}
		n.hosts = append(n.hosts, line)
		n.hostGateway = n.hostGateway || gateway
	}
	if !n.enabled && (len(n.publish) > 0 || n.hostGateway) {
		return n, fmt.Errorf("--network none cannot be combined with -p or --add-host NAME:host-gateway")
	}
	return n, nil
}

// spec is the guest's side of the network, or nil without one.
func (n networkOptions) spec() *guestspec.Network { return guestNetwork(n.enabled) }

// guestNetwork is the guest's side of the network, or nil without one.
func guestNetwork(enabled bool) *guestspec.Network {
	if !enabled {
		return nil
	}
	return &guestspec.Network{
		MAC:     net.HardwareAddr(netstack.GuestMAC[:]).String(),
		Address: netip.PrefixFrom(netstack.Guest, netstack.Subnet.Bits()).String(),
		Gateway: netstack.Gateway.String(),
		DNS:     []string{netstack.DNS.String()},
		MTU:     netstack.MTU,
	}
}

// parsePublish reads -p [IP:]HOSTPORT:CONTAINERPORT[/tcp]. Without an IP
// the port listens on 127.0.0.1 only.
func parsePublish(s string) (netstack.Port, error) {
	bad := func(format string, a ...any) (netstack.Port, error) {
		return netstack.Port{}, fmt.Errorf("-p %s: "+format, append([]any{s}, a...)...)
	}
	spec, proto, _ := strings.Cut(s, "/")
	if proto != "" && proto != "tcp" {
		return bad("only tcp ports can be published so far")
	}
	i := strings.LastIndex(spec, ":")
	if i < 0 {
		return bad("give a host port, as in -p 8080:%s; random host ports are not supported yet", spec)
	}
	rest, guestPort := spec[:i], spec[i+1:]
	hostIP, hostPort := "127.0.0.1", rest
	if j := strings.LastIndex(rest, ":"); j >= 0 {
		hostIP, hostPort = rest[:j], rest[j+1:]
		hostIP = strings.TrimSuffix(strings.TrimPrefix(hostIP, "["), "]")
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
	}
	ip, err := netip.ParseAddr(hostIP)
	if err != nil {
		return bad("%q is not an IP address", hostIP)
	}
	if hostPort == "" {
		return bad("give a host port; random host ports are not supported yet")
	}
	hp, err := parsePort(hostPort)
	if err != nil {
		return bad("host port: %v", err)
	}
	gp, err := parsePort(guestPort)
	if err != nil {
		return bad("container port: %v", err)
	}
	return netstack.Port{Host: netip.AddrPortFrom(ip, hp), Guest: gp}, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q is not a port number (ranges are not supported)", s)
	}
	return uint16(n), nil
}

// parseAddHost reads --add-host NAME:ADDRESS or NAME=ADDRESS, where ADDRESS
// may be host-gateway: the address at which the guest reaches the host's
// loopback. It returns the /etc/hosts line.
func parseAddHost(s string) (line string, gateway bool, err error) {
	i := strings.IndexAny(s, ":=")
	if i <= 0 {
		return "", false, fmt.Errorf("--add-host %s: want NAME:ADDRESS", s)
	}
	name, value := s[:i], strings.TrimSuffix(strings.TrimPrefix(s[i+1:], "["), "]")
	if strings.ContainsAny(name, " \t") {
		return "", false, fmt.Errorf("--add-host %s: bad host name %q", s, name)
	}
	if value == "host-gateway" {
		return netstack.Gateway.String() + "\t" + name, true, nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "", false, fmt.Errorf("--add-host %s: %q is not an IP address or host-gateway", s, value)
	}
	return ip.String() + "\t" + name, false, nil
}
