//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/netswitch"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/vmm"
)

const networkUsage = `Usage:
  runwhp network create [--label K=V]... NAME       make a network
  runwhp network ls [-q] [--no-trunc] [-f FILTER]...
                                                    list networks, or those that match name,
                                                    id, label, driver or type (custom|builtin)
  runwhp network inspect NETWORK...                 describe networks as JSON, with the containers on them
  runwhp network rm NETWORK...                      remove networks no container is running on
  runwhp network prune [-f]                         remove every network no container is running on
  runwhp network connect [--alias NAME]... [--ip ADDRESS] NETWORK CONTAINER
                                                    put a container that is not running on a network
  runwhp network disconnect NETWORK CONTAINER       take a container that is not running off one

Containers on the same network reach each other by name, alias or address:
run them with --network NAME. Each network has a subnet of its own, chosen
as it is made. user, runwhp's default network, keeps every container alone;
none gives a container loopback only. connect and disconnect take effect as
the container next boots, and refuse a running container.
`

// cmdNetwork runs runwhp network. It returns 1 when a command fails and 2
// for a usage error.
func cmdNetwork(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, networkUsage)
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	store := network.Open(root)
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("network "+sub, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), networkUsage) }
	switch sub {
	case "create":
		return networkCreate(store, fs, args, stdout)
	case "ls", "list":
		var quiet, noTrunc bool
		var filterArgs listFlag
		fs.BoolVar(&quiet, "q", false, "")
		fs.BoolVar(&quiet, "quiet", false, "")
		fs.BoolVar(&noTrunc, "no-trunc", false, "")
		fs.Var(&filterArgs, "f", "")
		fs.Var(&filterArgs, "filter", "")
		if fs.Parse(args) != nil || fs.NArg() > 0 {
			return 2
		}
		f, err := parseFilters(filterArgs, "name", "id", "label", "driver", "type")
		if err != nil {
			errorf("%v", err)
			return 2
		}
		all, err := store.List()
		if err != nil {
			errorf("%v", err)
			return 1
		}
		var nets []*network.Network
		for _, n := range all {
			if f.matchNetwork(n) {
				nets = append(nets, n)
			}
		}
		id := func(n *network.Network) string {
			if noTrunc {
				return n.ID
			}
			return n.Short()
		}
		if quiet {
			for _, n := range nets {
				fmt.Fprintln(stdout, id(n))
			}
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "NETWORK ID\tNAME\tDRIVER\tSCOPE")
		for _, n := range nets {
			fmt.Fprintf(tw, "%s\t%s\t%s\tlocal\n", id(n), n.Name, n.Driver)
		}
		tw.Flush()
	case "inspect":
		if fs.Parse(args) != nil || fs.NArg() == 0 {
			fs.Usage()
			return 2
		}
		out := []networkJSON{}
		code := 0
		for _, ref := range fs.Args() {
			n, err := store.Resolve(ref)
			if err != nil {
				errorf("%v", err)
				code = 1
				continue
			}
			out = append(out, inspectNetwork(root, store, n))
		}
		printJSON(stdout, out)
		return code
	case "rm", "remove":
		if fs.Parse(args) != nil || fs.NArg() == 0 {
			fs.Usage()
			return 2
		}
		code := 0
		for _, ref := range fs.Args() {
			if _, err := store.Remove(ref); err != nil {
				errorf("%v", err)
				code = 1
				continue
			}
			fmt.Fprintln(stdout, ref)
		}
		return code
	case "prune":
		var force bool
		fs.BoolVar(&force, "f", false, "")
		fs.BoolVar(&force, "force", false, "")
		if fs.Parse(args) != nil || fs.NArg() > 0 {
			return 2
		}
		if !force && !confirm(stdin, "This will remove all custom networks not used by at least one container.") {
			return 0
		}
		removed, err := pruneNetworks(store)
		printSection(stdout, "Deleted Networks:", removed)
		if err != nil {
			errorf("%v", err)
			return 1
		}
	case "connect":
		var aliases listFlag
		var ip string
		fs.Var(&aliases, "alias", "")
		fs.StringVar(&ip, "ip", "", "")
		if fs.Parse(args) != nil || fs.NArg() != 2 {
			fs.Usage()
			return 2
		}
		if err := networkConnect(root, store, fs.Arg(0), fs.Arg(1), aliases, ip); err != nil {
			errorf("%v", err)
			return 1
		}
	case "disconnect":
		var force bool
		fs.BoolVar(&force, "f", false, "")
		fs.BoolVar(&force, "force", false, "")
		if fs.Parse(args) != nil || fs.NArg() != 2 {
			fs.Usage()
			return 2
		}
		if err := networkDisconnect(root, store, fs.Arg(0), fs.Arg(1)); err != nil {
			errorf("%v", err)
			return 1
		}
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, networkUsage)
	default:
		fmt.Fprintf(os.Stderr, "runwhp: unknown network command %q\n\n%s", sub, networkUsage)
		return 2
	}
	return 0
}

func networkCreate(store *network.Store, fs *flag.FlagSet, args []string, stdout io.Writer) int {
	var driver string
	var attachable, internal, ipv6 bool
	var unsupported, labelArgs listFlag
	fs.StringVar(&driver, "d", "bridge", "")
	fs.StringVar(&driver, "driver", "bridge", "")
	fs.BoolVar(&attachable, "attachable", false, "")
	fs.BoolVar(&internal, "internal", false, "")
	fs.BoolVar(&ipv6, "ipv6", false, "")
	fs.Var(&labelArgs, "label", "")
	for _, f := range []string{"subnet", "gateway", "ip-range", "o", "opt", "ipam-driver", "ipam-opt", "aux-address", "scope", "config-from"} {
		f := f
		fs.Func(f, "", func(string) error { unsupported = append(unsupported, "--"+f); return nil })
	}
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	switch {
	case driver != "bridge":
		errorf("network driver %s is not supported: runwhp's networks are bridges", driver)
		return 1
	case internal || ipv6:
		errorf("--internal and --ipv6 are not supported")
		return 1
	case len(unsupported) > 0:
		errorf("%s not supported: runwhp chooses each network's subnet itself", strings.Join(unsupported, ", "))
		return 1
	}
	labels, err := parseLabels(labelArgs, nil)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	n, err := store.CreateWith(fs.Arg(0), labels)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	fmt.Fprintln(stdout, n.ID)
	return 0
}

// pruneNetworks removes every user-defined network no container is running
// on, and names them.
func pruneNetworks(store *network.Store) ([]string, error) {
	nets, err := store.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, n := range nets {
		if n.IsBuiltin() {
			continue
		}
		ok, err := store.RemoveIfUnused(n)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed = append(removed, n.Name)
		}
	}
	return removed, errors.Join(errs...)
}

// networkJSON is what network inspect prints, in docker network inspect's
// shape.
type networkJSON struct {
	Name       string                     `json:"Name"`
	ID         string                     `json:"Id"`
	Created    time.Time                  `json:"Created"`
	Scope      string                     `json:"Scope"`
	Driver     string                     `json:"Driver"`
	EnableIPv4 bool                       `json:"EnableIPv4"`
	EnableIPv6 bool                       `json:"EnableIPv6"`
	IPAM       ipamJSON                   `json:"IPAM"`
	Internal   bool                       `json:"Internal"`
	Attachable bool                       `json:"Attachable"`
	Ingress    bool                       `json:"Ingress"`
	ConfigFrom struct{ Network string }   `json:"ConfigFrom"`
	ConfigOnly bool                       `json:"ConfigOnly"`
	Containers map[string]networkEndpoint `json:"Containers"`
	Options    map[string]string          `json:"Options"`
	Labels     map[string]string          `json:"Labels"`
	Status     *networkStatus             `json:"Status,omitempty"`
}

type ipamJSON struct {
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
	Config  []ipamConfig      `json:"Config"`
}

type ipamConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

type networkEndpoint struct {
	Name        string `json:"Name"`
	EndpointID  string `json:"EndpointID"`
	MacAddress  string `json:"MacAddress"`
	IPv4Address string `json:"IPv4Address"`
	IPv6Address string `json:"IPv6Address"`
}

type networkStatus struct {
	IPAM struct {
		Subnets map[string]subnetStatus `json:"Subnets"`
	} `json:"IPAM"`
}

type subnetStatus struct {
	IPsInUse            uint64 `json:"IPsInUse"`
	DynamicIPsAvailable uint64 `json:"DynamicIPsAvailable"`
}

func inspectNetwork(root string, store *network.Store, n *network.Network) networkJSON {
	j := networkJSON{
		Name: n.Name, ID: n.ID, Created: n.Created, Scope: "local", Driver: n.Driver, EnableIPv4: n.Driver != "null",
		IPAM:       ipamJSON{Driver: "default", Options: map[string]string{}, Config: []ipamConfig{}},
		Containers: map[string]networkEndpoint{}, Options: map[string]string{}, Labels: withLabels(map[string]string{}, n.Labels),
	}
	if n.Subnet.IsValid() {
		j.IPAM.Config = append(j.IPAM.Config, ipamConfig{Subnet: n.Subnet.String(), Gateway: n.Gateway.String()})
	}
	cs := container.Open(root)
	if n.IsBuiltin() {
		// Containers running on the default network, each alone at the
		// same address, or with none.
		list, _ := cs.List()
		for _, c := range list {
			if c.State.Status != container.Running || c.Config.Mode() != n.Name {
				continue
			}
			e := networkEndpoint{Name: c.Name}
			if n.Name == network.User {
				e.MacAddress = netswitch.MACString(netstack.GuestMAC)
				e.IPv4Address = netip.PrefixFrom(netstack.Guest, netstack.Subnet.Bits()).String()
			}
			j.Containers[c.ID] = e
		}
		return j
	}
	eps, _ := store.Endpoints(n.ID)
	for _, e := range eps {
		j.Containers[e.Container] = networkEndpoint{
			Name: cs.Name(e.Container), EndpointID: e.ID, MacAddress: netswitch.MACString(e.MAC()),
			IPv4Address: netip.PrefixFrom(e.IP, n.Subnet.Bits()).String(),
		}
	}
	inUse := uint64(3 + len(eps)) // the subnet's own address, the gateway, the broadcast address
	j.Status = &networkStatus{}
	j.Status.IPAM.Subnets = map[string]subnetStatus{n.Subnet.String(): {IPsInUse: inUse, DynamicIPsAvailable: n.Size() - inUse}}
	return j
}

// connectTarget resolves the network and container connect and disconnect
// name.
func connectTarget(root string, store *network.Store, netRef, ref string) (*network.Network, *container.Store, *container.Container, error) {
	n, err := store.Resolve(netRef)
	if err != nil {
		return nil, nil, nil, err
	}
	cs := container.Open(root)
	c, err := cs.Resolve(ref)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("No such container: %s", ref)
	}
	return n, cs, c, nil
}

func networkConnect(root string, store *network.Store, netRef, ref string, aliases []string, ip string) error {
	n, cs, c, err := connectTarget(root, store, netRef, ref)
	if err != nil {
		return err
	}
	switch n.Name {
	case network.None:
		return errors.New("container cannot be connected to multiple networks with one of the networks in private (none) mode")
	case network.User:
		return errors.New("runwhp cannot connect a container to its default network, which keeps every container alone")
	}
	a := container.Attachment{Network: n.ID, Name: n.Name, Aliases: aliases}
	if ip != "" {
		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() {
			return fmt.Errorf("invalid IPv4 address: %s", ip)
		}
		if !n.Subnet.Contains(addr) {
			return fmt.Errorf("invalid endpoint settings:\nno configured subnet contains IP address %s", addr)
		}
		a.IPv4 = addr.String()
	}
	cfg, err := c.Config.Connect(a)
	if err != nil {
		return err
	}
	if err := vmm.CheckDevices(cfg, len(cfg.Volumes)); err != nil {
		return err
	}
	return changeNetworks(root, cs, c, container.Request{Op: container.OpNetworks, Connect: &a}, func(cfg *container.Config) (*container.Config, error) {
		return cfg.Connect(a)
	})
}

func networkDisconnect(root string, store *network.Store, netRef, ref string) error {
	n, cs, c, err := connectTarget(root, store, netRef, ref)
	if err != nil {
		return err
	}
	if _, err := c.Config.Disconnect(n.ID, n.Name); err != nil {
		return err
	}
	return changeNetworks(root, cs, c, container.Request{Op: container.OpNetworks, Disconnect: n.ID}, func(cfg *container.Config) (*container.Config, error) {
		return cfg.Disconnect(n.ID, n.Name)
	})
}

// changeNetworks changes a container's networks: its configuration, under
// its lock, if it is not running, or through the runwhp that holds that,
// which refuses a running container and boots a created one again.
func changeNetworks(root string, cs *container.Store, c *container.Container, req container.Request, change func(*container.Config) (*container.Config, error)) error {
	lock, err := cs.Lock(c)
	if errors.Is(err, container.ErrRunning) {
		if c.State.Status == container.Running {
			return container.ErrNetworksRunning
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		rep, err := container.Ask(ctx, root, c.ID, req)
		if err != nil {
			return err
		}
		if !rep.OK {
			return errors.New(rep.Error)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	// Read again under the lock.
	fresh, err := cs.Get(c.ID)
	if err != nil {
		return err
	}
	cfg, err := change(fresh.Config)
	if err != nil {
		return err
	}
	return lock.WriteConfig(cfg)
}
