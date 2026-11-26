//go:build windows

package vmm

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/vm"
)

// The default network: one device, at 10.0.2.15 behind 10.0.2.2, with DNS
// at 10.0.2.3; a guest serving files gets the resolv.conf but no device.
func TestOpenDefaultNetwork(t *testing.T) {
	cfg := &container.Config{Network: true}
	n, err := openNetwork(t.TempDir(), cfg, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer n.close(t.Logf)
	want := []guestspec.Network{{MAC: "02:00:00:00:00:0f", Address: "10.0.2.15/24", Gateway: "10.0.2.2", MTU: 65520}}
	if !reflect.DeepEqual(n.specs, want) || len(n.devices()) != 1 {
		t.Errorf("specs %+v, %d devices", n.specs, len(n.devices()))
	}
	if n.resolv == nil || !reflect.DeepEqual(n.resolv.Nameservers, []string{"10.0.2.3"}) || len(n.resolv.Options) != 0 || n.hostnameAddrs != nil {
		t.Errorf("resolv %+v, hostname addresses %q", n.resolv, n.hostnameAddrs)
	}

	files, err := openNetwork(t.TempDir(), cfg, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.specs) != 0 || len(files.devices()) != 0 || files.resolv == nil {
		t.Errorf("without devices: %+v", files)
	}

	none, err := openNetwork(t.TempDir(), &container.Config{}, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.specs) != 0 || len(none.devices()) != 0 || none.resolv != nil {
		t.Errorf("--network none: %+v", none)
	}
	if rx, tx := none.traffic(); rx != 0 || tx != 0 {
		t.Errorf("traffic %d %d", rx, tx)
	}
}

// A container on two user-defined networks: a device each, by name, the
// first with the default route; DNS at its gateway; the hostname at both
// addresses; leases held until close.
func TestOpenUserNetworks(t *testing.T) {
	root := t.TempDir()
	st := network.Open(root)
	st.HostPrefixes = func() []netip.Prefix { return nil }
	a, err := st.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Create("beta")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &container.Config{ID: container.NewID(), Hostname: "h", Network: true, NetworkMode: "beta",
		Networks: []container.Attachment{{Network: a.ID, Name: "alpha", IPv4: "172.18.0.9"}, {Network: b.ID, Name: "beta", Aliases: []string{"db"}}}}
	n, err := openNetwork(root, cfg, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	want := []guestspec.Network{
		{MAC: "02:42:ac:12:00:09", Address: "172.18.0.9/16", Gateway: "172.18.0.1", MTU: 65520},
		{MAC: "02:42:ac:13:00:02", Address: "172.19.0.2/16", MTU: 65520},
	}
	if !reflect.DeepEqual(n.specs, want) || len(n.devices()) != 2 {
		t.Errorf("specs %+v", n.specs)
	}
	if !reflect.DeepEqual(n.resolv, &guestspec.Resolv{Nameservers: []string{"172.18.0.1"}, Options: []string{"ndots:0"}}) ||
		!reflect.DeepEqual(n.hostnameAddrs, []string{"172.18.0.9", "172.19.0.2"}) {
		t.Errorf("resolv %+v, hostname addresses %q", n.resolv, n.hostnameAddrs)
	}
	if eps, _ := st.Endpoints(b.ID); len(eps) != 1 || eps[0].Aliases[0] != "db" || eps[0].Hostname != "h" {
		t.Errorf("endpoints %+v", eps)
	}
	// The same address again is refused while the lease is held.
	other := &container.Config{ID: container.NewID(), Network: true, Networks: []container.Attachment{{Network: a.ID, Name: "alpha", IPv4: "172.18.0.9"}}}
	if _, err := openNetwork(root, other, true, t.Logf); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("a taken address: %v", err)
	}
	files, err := openNetwork(root, cfg, false, t.Logf)
	if err != nil || len(files.devices()) != 0 || files.resolv.Nameservers[0] != "172.18.0.1" || files.hostnameAddrs != nil {
		t.Errorf("without devices: %+v %v", files, err)
	}
	n.close(t.Logf)
	if eps, _ := st.Endpoints(a.ID); len(eps) != 0 {
		t.Errorf("leases kept after close: %+v", eps)
	}
	// A network removed meanwhile fails the boot, as Docker's start does.
	st.Remove("beta")
	if _, err := openNetwork(root, cfg, true, t.Logf); err == nil || err.Error() != "failed to set up container networking: network beta not found" {
		t.Errorf("a removed network: %v", err)
	}
	if eps, _ := st.Endpoints(a.ID); len(eps) != 0 {
		t.Errorf("a failed boot kept a lease: %+v", eps)
	}
}

// Each network takes a device, and an interrupt line.
func TestCheckDevicesNetworks(t *testing.T) {
	nets := make([]container.Attachment, 3)
	cfg := &container.Config{Network: true, Networks: nets, LayerFiles: make([]string, vm.MaxDevices-7)}
	if err := CheckDevices(cfg, 0); err != nil {
		t.Errorf("layers that fit refused: %v", err)
	}
	cfg.LayerFiles = append(cfg.LayerFiles, "one more")
	if err := CheckDevices(cfg, 0); err == nil || !strings.Contains(err.Error(), "3 networks") {
		t.Errorf("one layer too many: %v", err)
	}
	cfg = &container.Config{Network: true, Networks: nets[:1], LayerFiles: make([]string, vm.MaxDevices-5)}
	if err := CheckDevices(cfg, 0); err != nil {
		t.Errorf("one network: %v", err)
	}
}
