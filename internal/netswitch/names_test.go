package netswitch

import (
	"context"
	"net/netip"
	"testing"
)

// The first of a guest's networks that knows a name answers; addresses and
// names nobody knows go to the host's resolver, as do AAAA lookups.
func TestNames(t *testing.T) {
	first := names{"x": netip.MustParseAddr("172.18.0.2")}
	second := names{"x": netip.MustParseAddr("172.19.0.2"), "y": netip.MustParseAddr("172.19.0.3")}
	n := &Names{Networks: []NameDirectory{first, second}, Next: failResolver{}}
	ctx := context.Background()
	for name, want := range map[string]string{"x": "172.18.0.2", "X.": "172.18.0.2", "y": "172.19.0.3"} {
		got, err := n.LookupNetIP(ctx, "ip4", name)
		if err != nil || len(got) != 1 || got[0].String() != want {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if _, err := n.LookupNetIP(ctx, "ip4", "z"); err == nil {
		t.Error("z resolved")
	}
	if _, err := n.LookupNetIP(ctx, "ip6", "x"); err == nil {
		t.Error("x resolved for ip6")
	}
	if got, err := n.LookupAddr(ctx, "172.19.0.3"); err != nil || len(got) != 1 || got[0] != "y.net." {
		t.Errorf("PTR: %v %v", got, err)
	}
	if _, err := n.LookupAddr(ctx, "1.2.3.4"); err == nil {
		t.Error("1.2.3.4 named")
	}
}
