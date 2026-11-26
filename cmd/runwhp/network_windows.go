//go:build windows

package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/network"
)

// networkOptions is what the network flags ask for.
type networkOptions struct {
	enabled     bool
	mode        string                 // as Config.NetworkMode keeps it
	networks    []container.Attachment // user-defined ones, by name
	publish     []netstack.Port
	hosts       []string // /etc/hosts lines
	hostGateway bool
}

// networkFlags are run's network flags: --network, repeatable, which takes
// a network's name or Docker's NAME=VALUE,... form, and --network-alias
// and --ip, which go with the first network named, as Docker's do.
type networkFlags struct {
	modes, aliases, publish, addHosts []string
	ip                                string
}

// networkRequest is what a container asks of the network, before the
// networks it names are looked up: run's network flags parsed, or a compose
// service's networks and ports.
type networkRequest struct {
	// attach are the networks by name, the first being the one --network
	// named first; empty means the default network.
	attach      []networkWant
	publish     []netstack.Port
	hosts       []string // /etc/hosts lines
	hostGateway bool     // a hosts line names host-gateway
}

// networkWant is one network a container joins, with its aliases and
// address there.
type networkWant struct {
	name, ip string
	aliases  []string
}

// parseNetworkFlags checks the network flags together, resolving the
// networks they name in the store at root.
func parseNetworkFlags(root string, f networkFlags) (networkOptions, error) {
	r, err := parseNetworkRequest(f)
	if err != nil {
		return networkOptions{}, err
	}
	return planNetwork(root, r)
}

// parseNetworkRequest reads the network flags, naming no network that
// must exist yet.
func parseNetworkRequest(f networkFlags) (networkRequest, error) {
	var r networkRequest
	for i, m := range f.modes {
		w := networkWant{name: m}
		if strings.Contains(m, "=") {
			w.name = ""
			for _, kv := range strings.Split(m, ",") {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "name":
					w.name = v
				case "alias":
					w.aliases = append(w.aliases, v)
				case "ip":
					w.ip = v
				case "mac-address", "driver-opt", "link-local-ip", "gw-priority", "ip6":
					return r, fmt.Errorf("--network %s: %s is not supported", m, k)
				default:
					return r, fmt.Errorf("--network %s: unknown option %q", m, k)
				}
			}
			if w.name == "" {
				return r, fmt.Errorf("--network %s: no name given", m)
			}
		}
		if i == 0 {
			if len(f.aliases) > 0 && len(w.aliases) > 0 || f.ip != "" && w.ip != "" {
				return r, errors.New("conflicting options: cannot specify both --network-alias or --ip and per-network ones")
			}
			w.aliases = append(w.aliases, f.aliases...)
			if w.ip == "" {
				w.ip = f.ip
			}
		}
		r.attach = append(r.attach, w)
	}
	if len(f.modes) == 0 && (len(f.aliases) > 0 || f.ip != "") {
		// They go with the default network, which refuses them below.
		r.attach = []networkWant{{name: network.User, aliases: f.aliases, ip: f.ip}}
	}
	for _, s := range f.publish {
		p, err := parsePublish(s)
		if err != nil {
			return r, err
		}
		r.publish = append(r.publish, p)
	}
	for _, s := range f.addHosts {
		line, gateway, err := netstack.HostsLine(s)
		if err != nil {
			return r, err
		}
		r.hosts = append(r.hosts, line)
		r.hostGateway = r.hostGateway || gateway
	}
	return r, nil
}

// planNetwork checks what a container asks of the network together,
// resolving the networks it names in the store at root.
func planNetwork(root string, r networkRequest) (networkOptions, error) {
	n := networkOptions{publish: r.publish, hosts: r.hosts, hostGateway: r.hostGateway}
	ws := r.attach
	if len(ws) == 0 {
		ws = []networkWant{{name: network.User}}
	}
	store := network.Open(root)
	seen := map[string]bool{}
	for _, w := range ws {
		switch {
		case w.name == "host":
			return n, errors.New("network host is not supported: every container has a VM, and a network, of its own; publish ports with -p, and reach the host with --add-host NAME:host-gateway")
		case strings.HasPrefix(w.name, "container:"):
			return n, fmt.Errorf("network %s is not supported: containers' VMs cannot share a network stack", w.name)
		}
		nw, err := store.Resolve(w.name)
		if err != nil {
			return n, err
		}
		if nw.IsBuiltin() {
			switch {
			case len(ws) > 1:
				return n, errors.New("conflicting options: cannot attach both user-defined and non-user-defined network-modes")
			case len(w.aliases) > 0:
				return n, errors.New("network-scoped aliases are only supported for user-defined networks")
			case w.ip != "":
				return n, fmt.Errorf("invalid config for network %s: invalid endpoint settings:\nuser-specified IP address is supported on user-defined networks only", w.name)
			}
			n.mode, n.enabled = nw.Name, nw.Name == network.User
			break
		}
		if seen[nw.ID] {
			return n, fmt.Errorf("network %s is given more than once", nw.Name)
		}
		seen[nw.ID] = true
		a := container.Attachment{Network: nw.ID, Name: nw.Name, Aliases: w.aliases}
		if w.ip != "" {
			ip, err := netip.ParseAddr(w.ip)
			if err != nil || !ip.Is4() {
				return n, fmt.Errorf("invalid IPv4 address: %s", w.ip)
			}
			if !nw.Subnet.Contains(ip) {
				return n, fmt.Errorf("invalid config for network %s: invalid endpoint settings:\nno configured subnet contains IP address %s", nw.Name, ip)
			}
			a.IPv4 = ip.String()
		}
		if n.mode == "" {
			n.mode = nw.Name
		}
		n.networks = append(n.networks, a)
		n.enabled = true
	}
	container.SortNetworks(n.networks)
	if !n.enabled && (len(n.publish) > 0 || n.hostGateway) {
		return n, fmt.Errorf("--network none cannot be combined with -p or --add-host NAME:host-gateway")
	}
	return n, nil
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
