package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers A queries for fixture.test. with 192.0.2.7 over UDP, and
// everything else with NXDOMAIN, recording what it was asked.
type fakeDNS struct {
	pc    net.PacketConn
	mu    sync.Mutex
	asked []string
}

func newFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	f := &fakeDNS{pc: pc}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil || len(m.Questions) != 1 {
				continue
			}
			q := m.Questions[0]
			f.mu.Lock()
			f.asked = append(f.asked, q.Name.String())
			f.mu.Unlock()
			m.Header.Response, m.Header.RecursionAvailable = true, true
			if q.Name.String() == "fixture.test." && q.Type == dnsmessage.TypeA {
				m.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 7}},
				}}
			} else if q.Type == dnsmessage.TypeA {
				m.Header.RCode = dnsmessage.RCodeNameError
			}
			if reply, err := m.Pack(); err == nil {
				pc.WriteTo(reply, from)
			}
		}
	}()
	return f
}

func (f *fakeDNS) addr() netip.AddrPort { return netip.MustParseAddrPort(f.pc.LocalAddr().String()) }

func (f *fakeDNS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// Upstream asks the servers given, the next once one fails, and from then
// on that one; and asks names absolute, the host's search domains not
// applying.
func TestUpstream(t *testing.T) {
	f := newFakeDNS(t)
	dead := netip.MustParseAddrPort("192.0.2.99:53")
	var mu sync.Mutex
	var dials []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dials = append(dials, address)
		mu.Unlock()
		if address == dead.String() {
			return nil, errors.New("unreachable")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}

	r := Upstream([]netip.AddrPort{dead, f.addr()}, dial)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addrs, err := r.LookupNetIP(ctx, "ip4", "fixture.test")
	if err != nil || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.0.2.7") {
		t.Fatalf("fixture.test: %v, %v", addrs, err)
	}
	if _, err := r.LookupNetIP(ctx, "ip4", "fixture.test"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(dials) != 3 || dials[0] != dead.String() || dials[1] != f.addr().String() || dials[2] != f.addr().String() {
		t.Errorf("dialled %q: want the dead server once, then the live one", dials)
	}
	mu.Unlock()
	if _, err := r.LookupNetIP(ctx, "ip4", "missing.test"); err == nil {
		t.Error("missing.test was found")
	} else if de, ok := err.(*net.DNSError); !ok || !de.IsNotFound {
		t.Errorf("missing.test: %v, want not found", err)
	}
	for _, n := range f.names() {
		if n != "fixture.test." && n != "missing.test." {
			t.Errorf("asked %q: names are asked as given", n)
		}
	}

	none := Upstream(nil, nil)
	if _, err := none.LookupNetIP(ctx, "ip4", "fixture.test"); err == nil {
		t.Error("no servers, yet an answer")
	}
}
