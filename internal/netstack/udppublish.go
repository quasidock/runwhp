package netstack

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// udpFlowsPerPort bounds the clients a published UDP port serves at once;
// datagrams from more are dropped until a flow ends.
const udpFlowsPerPort = 1024

// udpFlowIn is a client's flow to a published UDP port: a socket in the
// stack, from the gateway as a published TCP port's connections are, to
// the guest's port, whose replies go back to the client.
type udpFlowIn struct {
	guest  *gonet.UDPConn
	last   atomic.Int64 // when anything last went either way, in UnixNano
	closed atomic.Bool
}

func (f *udpFlowIn) Close() error {
	f.closed.Store(true)
	return f.guest.Close()
}

// serveUDPPort carries datagrams from clients of a published UDP port to
// the guest, each client in a flow of its own that ends when neither side
// has sent anything for udpIdle, as Docker's proxy keeps them.
func (s *Stack) serveUDPPort(pc *net.UDPConn, port uint16) {
	defer s.loops.Done()
	var mu sync.Mutex
	flows := map[netip.AddrPort]*udpFlowIn{}
	guest := tcpip.FullAddress{NIC: nicID, Addr: addr(s.cfg.Addresses.Guest), Port: port}
	buf := make([]byte, 64<<10)
	for {
		n, client, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.cfg.Logf("reading on %s: %v", pc.LocalAddr(), err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		mu.Lock()
		f := flows[client]
		if f == nil {
			if len(flows) >= udpFlowsPerPort {
				mu.Unlock()
				s.udpInDropped.Add(1)
				continue
			}
			c, err := gonet.DialUDP(s.stack, nil, &guest, ipv4.ProtocolNumber)
			if err != nil {
				mu.Unlock()
				s.cfg.Logf("published port %d/udp: %v", port, err)
				continue
			}
			f = &udpFlowIn{guest: c}
			if !s.track(f) {
				mu.Unlock()
				c.Close()
				return
			}
			f.last.Store(time.Now().UnixNano())
			flows[client] = f
			s.udpFlowsIn.Add(1)
			go func() {
				s.replyUDP(pc, f, client)
				mu.Lock()
				delete(flows, client)
				mu.Unlock()
				s.untrack(f)
				f.Close()
			}()
		}
		mu.Unlock()
		f.last.Store(time.Now().UnixNano())
		// A guest that is not listening yet answers port unreachable, which
		// the flow's next read reports; it does not end the flow.
		f.guest.Write(buf[:n])
	}
}

// replyUDP sends the guest's replies on a flow back to its client, until
// the flow has been idle for udpIdle or is closed.
func (s *Stack) replyUDP(pc *net.UDPConn, f *udpFlowIn, client netip.AddrPort) {
	buf := make([]byte, 64<<10)
	for {
		f.guest.SetReadDeadline(time.Now().Add(udpIdle))
		n, err := f.guest.Read(buf)
		if f.closed.Load() || s.ctx.Err() != nil || errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if time.Since(time.Unix(0, f.last.Load())) >= udpIdle {
					return
				}
				continue
			}
			// Port unreachable, from a guest not listening yet: the next
			// datagram may find it listening.
			time.Sleep(time.Millisecond)
			continue
		}
		f.last.Store(time.Now().UnixNano())
		pc.WriteToUDPAddrPort(buf[:n], client)
	}
}
