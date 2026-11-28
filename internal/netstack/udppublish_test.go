package netstack

import (
	"bytes"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// A TCP port reads and writes as records did before ports had protocols;
// a UDP port says so.
func TestPortJSON(t *testing.T) {
	var old Port
	if err := json.Unmarshal([]byte(`{"Host":"127.0.0.1:80","Guest":80}`), &old); err != nil || old.Protocol() != "tcp" || old.String() != "127.0.0.1:80->80/tcp" {
		t.Errorf("an old record: %+v, %v", old, err)
	}
	if b, _ := json.Marshal(old); string(b) != `{"Host":"127.0.0.1:80","Guest":80}` {
		t.Errorf("tcp: %s", b)
	}
	udp := Port{Host: netip.MustParseAddrPort("127.0.0.1:53"), Guest: 53, Proto: "udp"}
	b, _ := json.Marshal(udp)
	var back Port
	if json.Unmarshal(b, &back); back != udp || udp.String() != "127.0.0.1:53->53/udp" {
		t.Errorf("udp: %s, %+v", b, back)
	}
	for in, want := range map[string]string{"": "", "tcp": "", "TCP": "", "udp": "udp", "UDP": "udp"} {
		if got, err := ParseProto(in); got != want || err != nil {
			t.Errorf("ParseProto(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseProto("sctp"); err == nil || !strings.Contains(err.Error(), "sctp ports cannot be published") {
		t.Errorf("sctp: %v", err)
	}
}

// udpEcho answers each datagram to the guest's port with it upper-cased,
// as a guest's UDP server would, and reports where each came from.
func udpEcho(t *testing.T, g *guest, port uint16) <-chan string {
	t.Helper()
	local := full(Guest, port)
	c, err := gonet.DialUDP(g.stack, &local, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	from := make(chan string, 64)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, a, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case from <- a.(*net.UDPAddr).IP.String():
			default:
			}
			c.WriteTo(bytes.ToUpper(buf[:n]), a)
		}
	}()
	return from
}

// ask sends a datagram to a published UDP port until it is answered.
func ask(t *testing.T, c *net.UDPConn, msg []byte) []byte {
	t.Helper()
	buf := make([]byte, 64<<10)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, err := c.Read(buf); err == nil {
			return buf[:n]
		}
	}
	t.Fatalf("no answer to %d bytes", len(msg))
	return nil
}

// A UDP port published on the host carries each client's datagrams to the
// guest, from the gateway, and the guest's replies back to that client;
// clients each have a flow of their own, a TCP port of the same number is
// another, and a port asked for as 0 is reported as bound. A guest not
// listening yet is answered once it listens.
func TestPublishUDP(t *testing.T) {
	g := newGuest(t, Config{Publish: []Port{
		{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 9999, Proto: "udp"},
		{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 9999},
	}})
	pub := g.host.Published()
	if len(pub) != 2 || pub[0].Protocol() != "udp" || pub[0].Random() || pub[1].Protocol() != "tcp" || pub[1].Random() {
		t.Fatalf("published %v", pub)
	}
	if want := g.host.published[0].pc.LocalAddr().String(); pub[0].Host.String() != want {
		t.Errorf("published %s, bound %s", pub[0].Host, want)
	}

	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(pub[0].Host))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Nothing listens in the guest yet: the datagram is lost, as on a
	// host, and the flow waits for one that is answered.
	c.Write([]byte("early"))
	time.Sleep(50 * time.Millisecond)
	from := udpEcho(t, g, 9999)
	if got := ask(t, c, []byte("hello")); string(got) != "HELLO" {
		t.Errorf("got %q", got)
	}
	if a := <-from; a != Gateway.String() {
		t.Errorf("the guest saw the datagram from %s, want the gateway", a)
	}
	big := bytes.Repeat([]byte("x"), 60000)
	if got := ask(t, c, big); !bytes.Equal(got, bytes.ToUpper(big)) {
		t.Errorf("a 60 KB datagram came back as %d bytes", len(got))
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(pub[0].Host))
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			msg := []byte(strings.Repeat("c", i+1))
			if got := ask(t, c, msg); !bytes.Equal(got, bytes.ToUpper(msg)) {
				t.Errorf("client %d got %q", i, got)
			}
		}(i)
	}
	wg.Wait()
	if n := g.host.udpFlowsIn.Load(); n != 5 {
		t.Errorf("%d flows, want one a client", n)
	}

	// Close ends the flows and frees the port.
	g.host.Close()
	g.host.mu.Lock()
	left := len(g.host.closers)
	g.host.mu.Unlock()
	if left != 0 {
		t.Errorf("%d connections left after Close", left)
	}
	pc, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(pub[0].Host))
	if err != nil {
		t.Errorf("the port after Close: %v", err)
	} else {
		pc.Close()
	}

	// A host UDP port already in use fails New.
	busy, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, err = New(Config{Output: func([]byte) {}, Publish: []Port{{Host: netip.MustParseAddrPort(busy.LocalAddr().String()), Guest: 53, Proto: "udp"}}})
	if err == nil || !strings.Contains(err.Error(), "publishing port") || !strings.Contains(err.Error(), "/udp") {
		t.Errorf("busy port: %v", err)
	}
}

// RUNWHP_PUBLISH_ADDRESS sets the address of ports published without one.
func TestDefaultPublishAddress(t *testing.T) {
	for v, want := range map[string]string{"": "127.0.0.1", "0.0.0.0": "0.0.0.0", "::": "::", "[::]": "::", " 192.168.2.10 ": "192.168.2.10", "::ffff:127.0.0.2": "127.0.0.2"} {
		t.Setenv(PublishAddressEnv, v)
		if got, err := DefaultPublishAddress(); err != nil || got.String() != want {
			t.Errorf("%q: %v, %v; want %s", v, got, err, want)
		}
	}
	for _, v := range []string{"localhost", "fe80::1%eth0", "1.2.3"} {
		t.Setenv(PublishAddressEnv, v)
		if _, err := DefaultPublishAddress(); err == nil || !strings.Contains(err.Error(), "is not an IP address") {
			t.Errorf("%q: %v", v, err)
		}
	}
}
