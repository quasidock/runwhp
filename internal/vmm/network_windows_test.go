//go:build windows

package vmm

import (
	"reflect"
	"testing"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// The default network: one device, at 10.0.2.15 behind 10.0.2.2, with DNS
// at 10.0.2.3; a guest serving files gets the resolv.conf but no device.
func TestOpenDefaultNetwork(t *testing.T) {
	cfg := &container.Config{Network: true}
	n, err := openNetwork(cfg, true, t.Logf)
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

	files, err := openNetwork(cfg, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.specs) != 0 || len(files.devices()) != 0 || files.resolv == nil {
		t.Errorf("without devices: %+v", files)
	}

	none, err := openNetwork(&container.Config{}, true, t.Logf)
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
