// Package netstack gives a guest a network without touching the host's
// network configuration. A user-mode TCP/IP stack, gVisor's, sits at the far
// end of the guest's Ethernet link. It terminates the guest's TCP connections
// and UDP flows and carries them on over ordinary host sockets, answers DNS
// through the host's resolver, and publishes guest ports as host listeners.
//
// Nothing here depends on the hypervisor: frames from the guest come in
// through Inject and frames for it leave through Config.Output, so tests
// connect a second stack in the guest's place.
package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	gvlog "gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The default network's addresses, after QEMU's user-mode networking. The
// guest is the only machine on it; the stack answers at both the gateway
// and the DNS address. Gateway is also where every guest reaches the host,
// whatever network it is on: host-gateway names it.
var (
	Subnet  = netip.MustParsePrefix("10.0.2.0/24")
	Gateway = netip.MustParseAddr("10.0.2.2")
	DNS     = netip.MustParseAddr("10.0.2.3")
	Guest   = netip.MustParseAddr("10.0.2.15")

	GatewayMAC = [6]byte{0x02, 0, 0, 0, 0, 0x02}
	GuestMAC   = [6]byte{0x02, 0, 0, 0, 0, 0x0f}
)

// Addresses are the addresses on a guest's link: its subnet, the guest's
// own address, and the gateway and DNS server, which the stack answers at.
type Addresses struct {
	Subnet              netip.Prefix
	Gateway, DNS, Guest netip.Addr
	GatewayMAC          [6]byte
}

// Default is the default network's addresses.
var Default = Addresses{Subnet: Subnet, Gateway: Gateway, DNS: DNS, Guest: Guest, GatewayMAC: GatewayMAC}

// MTU is the link's MTU. The link never reaches a wire, so it is as large as
// IPv4 allows (less room for headers, as slirp4netns uses): fewer, larger
// frames mean fewer VM exits per byte.
const MTU = 65520

const (
	nicID        = 1
	inboundQueue = 512
	dialTimeout  = 30 * time.Second
	udpIdle      = 60 * time.Second
	tcpInFlight  = 256
)

// Port is a guest port published on the host. A Host port of 0 asks for
// any port the system has free, as Docker's random host ports do.
type Port struct {
	Host  netip.AddrPort
	Guest uint16
	// Proto is "udp" for a UDP port, and empty for TCP, so that records of
	// TCP ports read the same as before there were others.
	Proto string `json:",omitempty"`
}

// Random reports whether the port asks for any free host port.
func (p Port) Random() bool { return p.Host.Port() == 0 }

// Protocol is the port's protocol: "tcp" or "udp".
func (p Port) Protocol() string {
	if p.Proto == "" {
		return "tcp"
	}
	return p.Proto
}

func (p Port) String() string { return fmt.Sprintf("%s->%d/%s", p.Host, p.Guest, p.Protocol()) }

// ParseProto reads a port's protocol as Docker writes it, into Port.Proto:
// "" or "tcp" is TCP, "udp" UDP; nothing else can be published.
func ParseProto(s string) (string, error) {
	switch strings.ToLower(s) {
	case "", "tcp":
		return "", nil
	case "udp":
		return "udp", nil
	}
	return "", fmt.Errorf("%s ports cannot be published: runwhp publishes TCP and UDP ports", s)
}

// Config configures a Stack.
type Config struct {
	// Output receives each Ethernet frame for the guest, which it may keep.
	// It is called from one goroutine at a time and must not block.
	Output func(frame []byte)
	// Addresses are the link's; the zero value means Default.
	Addresses Addresses
	// Refuse lists further subnets the guest may not reach through the
	// stack: other guests' networks, which the stack must not carry to the
	// host.
	Refuse []netip.Prefix
	// HostGateway makes connections to the gateway address, or the default
	// network's, reach the host's 127.0.0.1. Without it the gateway refuses
	// them.
	HostGateway bool
	// Publish lists guest ports to listen for on the host.
	Publish []Port
	// Dial opens host connections; nil means a net.Dialer. Tests replace it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Resolver answers DNS queries; nil means net.DefaultResolver, which on
	// Windows asks the system resolver.
	Resolver Resolver
	// Ping sends the guest's echo requests for addresses beyond the link
	// from the host; nil means the system's, where there is one. Without
	// one, only the stack's own addresses answer pings.
	Ping Pinger
	// GuestChecksums, if set, reports whether the guest takes frames
	// without TCP and UDP checksums, which the stack then leaves out.
	GuestChecksums func() bool
	// Logf receives diagnostics. nil discards them.
	Logf func(format string, a ...any)
}

// Stack is the host end of a guest's network link.
type Stack struct {
	cfg    Config
	stack  *stack.Stack
	link   *channel.Endpoint
	in     chan []byte
	ctx    context.Context
	cancel context.CancelFunc
	loops  sync.WaitGroup

	dnsUDP    *gonet.UDPConn
	dnsTCP    *gonet.TCPListener
	published []publishedSocket // one for each of cfg.Publish, in its order

	mu      sync.Mutex
	closed  bool
	closers map[io.Closer]struct{} // live connections, closed by Close

	pings chan struct{} // a token for each ping in flight

	tcpFlows, udpFlows, publishedConns, dnsQueries atomic.Uint64
	refused, dialFailures, inDropped               atomic.Uint64
	pingsSent, pingsDropped                        atomic.Uint64
	udpFlowsIn, udpInDropped                       atomic.Uint64
}

// publishedSocket is a published port's host socket: a TCP listener or a
// UDP socket.
type publishedSocket struct {
	ln net.Listener
	pc *net.UDPConn
}

func (p publishedSocket) close() {
	if p.ln != nil {
		p.ln.Close()
	}
	if p.pc != nil {
		p.pc.Close()
	}
}

// New builds the stack, opens the published ports' listeners, and starts
// serving. A port that cannot be published fails New.
func New(cfg Config) (*Stack, error) {
	if cfg.Output == nil {
		return nil, errors.New("netstack: Config.Output is nil")
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{}).DialContext
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Ping == nil {
		cfg.Ping = systemPinger()
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if !cfg.Addresses.Guest.IsValid() {
		cfg.Addresses = Default
	}
	// gVisor logs to stderr by default, which in runwhp is the container's
	// stderr. Its logger is global; the latest stack's Logf gets it.
	gvlog.SetTarget(logEmitter(cfg.Logf))
	s := &Stack{cfg: cfg, in: make(chan []byte, inboundQueue), closers: map[io.Closer]struct{}{},
		pings: make(chan struct{}, pingsInFlight)}

	// Listeners first, so a busy port fails before anything else starts.
	for _, p := range cfg.Publish {
		var ps publishedSocket
		var err error
		if p.Protocol() == "udp" {
			ps.pc, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(p.Host))
		} else {
			ps.ln, err = net.Listen("tcp", p.Host.String())
		}
		if err != nil {
			s.closeListeners()
			return nil, fmt.Errorf("publishing port %s: %w", p, err)
		}
		s.published = append(s.published, ps)
	}

	s.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	s.link = channel.New(inboundQueue, MTU+header.EthernetMinimumSize, tcpip.LinkAddress(cfg.Addresses.GatewayMAC[:]))
	if err := s.setup(); err != nil {
		s.closeListeners()
		s.stack.Destroy()
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())

	s.loops.Add(4 + len(s.published))
	go s.inbound()
	go s.outbound()
	go s.serveDNSUDP()
	go s.serveDNSTCP()
	for i, ps := range s.published {
		if ps.pc != nil {
			go s.serveUDPPort(ps.pc, cfg.Publish[i].Guest)
		} else {
			go s.servePort(ps.ln, cfg.Publish[i].Guest)
		}
	}
	return s, nil
}

func (s *Stack) setup() error {
	st := s.stack
	if err := st.CreateNIC(nicID, ethernet.New(&link{s.link, s.cfg.GuestChecksums})); err != nil {
		return fmt.Errorf("netstack: creating the NIC: %s", err)
	}
	// Promiscuous: take packets for any address, since every destination
	// the guest reaches is behind this stack. Spoofing: answer from those
	// addresses.
	if err := st.SetPromiscuousMode(nicID, true); err != nil {
		return fmt.Errorf("netstack: %s", err)
	}
	if err := st.SetSpoofing(nicID, true); err != nil {
		return fmt.Errorf("netstack: %s", err)
	}
	as := s.cfg.Addresses
	for i, a := range []netip.Addr{as.Gateway, as.DNS} {
		if i == 1 && a == as.Gateway {
			break // DNS at the gateway
		}
		pa := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(a), PrefixLen: as.Subnet.Bits()}}
		if err := st.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			return fmt.Errorf("netstack: adding %s: %s", a, err)
		}
	}
	st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	sack := tcpip.TCPSACKEnabled(true)
	st.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	st.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(st, 0, tcpInFlight, s.forwardTCP).HandlePacket)
	st.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(st, s.forwardUDP).HandlePacket)
	// The ICMP transport has to be registered for its handler to run.
	st.SetTransportProtocolHandler(icmp.ProtocolNumber4, s.forwardICMP)

	dns := tcpip.FullAddress{NIC: nicID, Addr: addr(as.DNS), Port: 53}
	var err error
	if s.dnsUDP, err = gonet.DialUDP(st, &dns, nil, ipv4.ProtocolNumber); err != nil {
		return fmt.Errorf("netstack: DNS over UDP: %w", err)
	}
	if s.dnsTCP, err = gonet.ListenTCP(st, dns, ipv4.ProtocolNumber); err != nil {
		s.dnsUDP.Close()
		return fmt.Errorf("netstack: DNS over TCP: %w", err)
	}
	return nil
}

func addr(a netip.Addr) tcpip.Address { return tcpip.AddrFrom4(a.As4()) }

// link is the NIC's end of the guest's link, whose checksum offloads follow
// the guest's driver.
type link struct {
	*channel.Endpoint
	guestChecksums func() bool
}

// Capabilities tells the stack not to check the checksums of frames from
// the guest: the guest is the only sender on the link, every frame ends in
// this stack, and a guest that may leave checksums partial does. Nor does
// the stack compute them for a guest that takes frames without them. The
// stack asks for every packet, so this follows the driver as it changes.
func (l *link) Capabilities() stack.LinkEndpointCapabilities {
	c := l.Endpoint.Capabilities() | stack.CapabilityRXChecksumOffload
	if l.guestChecksums != nil && l.guestChecksums() {
		c |= stack.CapabilityTXChecksumOffload
	}
	return c
}

// logEmitter passes gVisor's log messages to a Logf.
type logEmitter func(format string, a ...any)

func (f logEmitter) Emit(_ int, _ gvlog.Level, _ time.Time, format string, a ...any) {
	f("gvisor: "+format, a...)
}

// Inject hands the stack a frame from the guest, which the stack keeps. It
// never blocks: when the stack is behind, the frame is dropped, as a full NIC
// queue would drop it.
func (s *Stack) Inject(frame []byte) {
	select {
	case s.in <- frame:
	default:
		s.inDropped.Add(1)
	}
}

// Close stops publishing, closes every connection, and stops the stack.
func (s *Stack) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()

	s.cancel()
	s.closeListeners()
	for c := range closers {
		c.Close()
	}
	s.dnsUDP.Close()
	s.dnsTCP.Close()
	s.stack.Close()
	s.link.Close()
	s.loops.Wait()
	// The stack's own workers stop promptly once it is closed; do not let a
	// straggler hold up the exit.
	done := make(chan struct{})
	go func() {
		s.stack.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// Published returns the ports the stack listens on, as they were bound:
// one asked for as port 0 has the port the system chose.
func (s *Stack) Published() []Port {
	ps := make([]Port, 0, len(s.published))
	for i, sock := range s.published {
		p := s.cfg.Publish[i]
		if sock.pc != nil {
			p.Host = netip.AddrPortFrom(p.Host.Addr(), uint16(sock.pc.LocalAddr().(*net.UDPAddr).Port))
		} else if a, ok := sock.ln.Addr().(*net.TCPAddr); ok {
			p.Host = netip.AddrPortFrom(p.Host.Addr(), uint16(a.Port))
		}
		ps = append(ps, p)
	}
	return ps
}

func (s *Stack) closeListeners() {
	for _, ps := range s.published {
		ps.close()
	}
}

// Stats summarises the stack's activity.
func (s *Stack) Stats() string {
	return fmt.Sprintf("network: %d tcp and %d udp flows out, %d connections and %d udp flows in (%d dropped), %d dns queries, %d pings out (%d dropped), %d refused, %d dial failures, %d frames dropped",
		s.tcpFlows.Load(), s.udpFlows.Load(), s.publishedConns.Load(), s.udpFlowsIn.Load(), s.udpInDropped.Load(), s.dnsQueries.Load(),
		s.pingsSent.Load(), s.pingsDropped.Load(),
		s.refused.Load(), s.dialFailures.Load(), s.inDropped.Load())
}

func (s *Stack) inbound() {
	defer s.loops.Done()
	for {
		select {
		case frame := <-s.in:
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
			s.link.InjectInbound(0, pkt) // the Ethernet layer reads the type
			pkt.DecRef()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Stack) outbound() {
	defer s.loops.Done()
	for {
		pkt := s.link.ReadContext(s.ctx)
		if pkt == nil {
			return
		}
		frame := make([]byte, 0, pkt.Size())
		for _, b := range pkt.AsSlices() {
			frame = append(frame, b...)
		}
		pkt.DecRef()
		s.cfg.Output(frame)
	}
}

// target maps the destination of a guest flow to the host address it is
// carried to. ok is false when the guest may not reach it: the gateway
// without HostGateway, the rest of its own subnet, the default network's and
// those Refuse lists, and addresses that would otherwise end up on the
// host's loopback or broadcast. Root in the guest can address packets to
// 127.0.0.1, so loopback is refused here rather than trusted to the guest's
// routing.
func (s *Stack) target(a tcpip.Address, port uint16) (netip.AddrPort, bool) {
	if port == 0 {
		return netip.AddrPort{}, false
	}
	ip, ok := s.targetAddr(a)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, port), true
}

// targetAddr is target for the address alone, as for a ping.
func (s *Stack) targetAddr(a tcpip.Address) (netip.Addr, bool) {
	if a.Len() != 4 {
		return netip.Addr{}, false
	}
	ip := netip.AddrFrom4(a.As4())
	switch {
	case ip == s.cfg.Addresses.Gateway, ip == Gateway:
		if !s.cfg.HostGateway {
			return netip.Addr{}, false
		}
		ip = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	case s.cfg.Addresses.Subnet.Contains(ip), Subnet.Contains(ip), ip.IsLoopback(), ip.IsUnspecified(), ip.IsMulticast(),
		ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return netip.Addr{}, false
	}
	for _, p := range s.cfg.Refuse {
		if p.Contains(ip) {
			return netip.Addr{}, false
		}
	}
	return ip, true
}

// forwardTCP carries a guest connection to the host. It dials first, so a
// refusal on the host reaches the guest as a reset rather than as a
// connection that opens and closes at once. It runs on a goroutine of its
// own.
func (s *Stack) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst, ok := s.target(id.LocalAddress, id.LocalPort)
	if !ok {
		s.refused.Add(1)
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	host, err := s.cfg.Dial(ctx, "tcp", dst.String())
	cancel()
	if err != nil {
		s.dialFailures.Add(1)
		s.cfg.Logf("tcp to %s: %v", dst, err)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		host.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	s.tcpFlows.Add(1)
	s.splice(gonet.NewTCPConn(&wq, ep), host)
}

// forwardUDP carries a guest UDP flow to the host. It runs on the stack's
// inbound path, so the flow's work happens on goroutines of its own.
// Returning false makes the stack answer with port unreachable.
func (s *Stack) forwardUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst, ok := s.target(id.LocalAddress, id.LocalPort)
	if !ok {
		s.refused.Add(1)
		return false
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		s.cfg.Logf("udp to %s: %s", dst, err)
		return true
	}
	s.udpFlows.Add(1)
	go s.relayUDP(gonet.NewUDPConn(&wq, ep), dst)
	return true
}

// relayUDP passes datagrams both ways between a guest flow and a connected
// host socket until neither side has sent anything for udpIdle.
func (s *Stack) relayUDP(guest *gonet.UDPConn, dst netip.AddrPort) {
	host, err := s.cfg.Dial(s.ctx, "udp", dst.String())
	if err != nil {
		s.dialFailures.Add(1)
		s.cfg.Logf("udp to %s: %v", dst, err)
		guest.Close()
		return
	}
	if !s.track(guest, host) {
		guest.Close()
		host.Close()
		return
	}
	defer s.untrack(guest, host)

	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var once sync.Once
	stop := func() {
		once.Do(func() {
			guest.Close()
			host.Close()
		})
	}
	relay := func(dst, src net.Conn) {
		defer stop()
		buf := make([]byte, 64<<10)
		for {
			src.SetReadDeadline(time.Now().Add(udpIdle))
			n, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < udpIdle {
					continue
				}
				return
			}
			last.Store(time.Now().UnixNano())
			// UDP may lose datagrams anyway; a failed write, such as one
			// after an ICMP error, does not end the flow.
			dst.Write(buf[:n])
		}
	}
	done := make(chan struct{})
	go func() {
		relay(guest, host)
		close(done)
	}()
	relay(host, guest)
	<-done
}

// servePort accepts host connections on a published port and carries each
// to the guest.
func (s *Stack) servePort(ln net.Listener, port uint16) {
	defer s.loops.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.cfg.Logf("accepting on %s: %v", ln.Addr(), err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go s.publish(c, port)
	}
}

func (s *Stack) publish(c net.Conn, port uint16) {
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	guest, err := gonet.DialContextTCP(ctx, s.stack, tcpip.FullAddress{NIC: nicID, Addr: addr(s.cfg.Addresses.Guest), Port: port}, ipv4.ProtocolNumber)
	cancel()
	if err != nil {
		s.cfg.Logf("published port %d: %v", port, err)
		c.Close()
		return
	}
	s.publishedConns.Add(1)
	s.splice(c, guest)
}

// splice copies both ways between two connections, passing on half-closes,
// until both directions are done. An error in either direction ends both.
func (s *Stack) splice(a, b net.Conn) {
	if !s.track(a, b) {
		a.Close()
		b.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, a, b)
	go pipe(&wg, b, a)
	wg.Wait()
	a.Close()
	b.Close()
	s.untrack(a, b)
}

func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	if _, err := io.CopyBuffer(dst, src, make([]byte, 64<<10)); err != nil {
		abort(src)
		abort(dst)
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}

// abort closes a connection, with a reset where the host's TCP allows it.
func abort(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

func (s *Stack) track(cs ...io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	for _, c := range cs {
		s.closers[c] = struct{}{}
	}
	return true
}

func (s *Stack) untrack(cs ...io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cs {
		delete(s.closers, c)
	}
}
