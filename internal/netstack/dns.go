package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolver is the part of *net.Resolver the DNS server uses. Asking the
// host's resolver, rather than forwarding packets to a DNS server, means the
// guest sees what the host sees: its hosts file, VPN and per-domain servers,
// and corporate resolvers.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

const (
	dnsTTL        = 60 // seconds; the host's resolver does not report TTLs
	dnsTimeout    = 10 * time.Second
	dnsMaxUDP     = 512  // reply size without EDNS
	dnsEDNSSize   = 1232 // the EDNS payload size this server offers
	dnsInFlight   = 64
	dnsTCPTimeout = 10 * time.Second
)

func (s *Stack) serveDNSUDP() {
	defer s.loops.Done()
	buf := make([]byte, 64<<10)
	sem := make(chan struct{}, dnsInFlight)
	for {
		n, from, err := s.dnsUDP.ReadFrom(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		select {
		case sem <- struct{}{}:
		default:
			continue // too many lookups in progress; the client retries
		}
		s.dnsQueries.Add(1)
		go func() {
			defer func() { <-sem }()
			if reply := answerDNS(s.ctx, s.cfg.Resolver, query, true); reply != nil {
				s.dnsUDP.WriteTo(reply, from)
			}
		}()
	}
}

func (s *Stack) serveDNSTCP() {
	defer s.loops.Done()
	for {
		c, err := s.dnsTCP.Accept()
		if err != nil {
			return
		}
		go s.dnsConn(c)
	}
}

// dnsConn answers length-prefixed queries on one TCP connection until the
// client closes it or goes quiet.
func (s *Stack) dnsConn(c net.Conn) {
	if !s.track(c) {
		c.Close()
		return
	}
	defer func() {
		c.Close()
		s.untrack(c)
	}()
	for {
		c.SetDeadline(time.Now().Add(dnsTCPTimeout))
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(c, query); err != nil {
			return
		}
		s.dnsQueries.Add(1)
		reply := answerDNS(s.ctx, s.cfg.Resolver, query, false)
		if reply == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(reply)))
		if _, err := c.Write(append(out, reply...)); err != nil {
			return
		}
	}
}

// answerDNS builds the reply to one query, or returns nil when the message
// deserves none. Over UDP a reply too large for the client is cut down to
// its question with the TC bit set, so the client retries over TCP.
func answerDNS(ctx context.Context, r Resolver, query []byte, udp bool) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	reply := dnsmessage.Message{Header: dnsmessage.Header{
		ID: h.ID, Response: true, OpCode: h.OpCode,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
	}}
	questions, err := p.AllQuestions()
	if err != nil {
		reply.RCode = dnsmessage.RCodeFormatError
		return pack(&reply)
	}
	reply.Questions = questions

	limit, edns := dnsMaxUDP, false
	if p.SkipAllAnswers() == nil && p.SkipAllAuthorities() == nil {
		for {
			rh, err := p.AdditionalHeader()
			if err != nil {
				break
			}
			if rh.Type == dnsmessage.TypeOPT {
				edns = true
				limit = max(dnsMaxUDP, min(int(rh.Class), dnsEDNSSize))
			}
			if p.SkipAdditional() != nil {
				break
			}
		}
	}
	if edns {
		var opt dnsmessage.ResourceHeader
		opt.SetEDNS0(dnsEDNSSize, dnsmessage.RCodeSuccess, false)
		reply.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
	}

	switch {
	case h.OpCode != 0:
		reply.RCode = dnsmessage.RCodeNotImplemented
	case len(questions) != 1:
		reply.RCode = dnsmessage.RCodeFormatError
	default:
		ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
		reply.Answers, reply.RCode = lookup(ctx, r, questions[0])
		cancel()
	}
	out := pack(&reply)
	if udp && len(out) > limit {
		reply.Answers = nil
		reply.Truncated = true
		out = pack(&reply)
	}
	return out
}

func pack(m *dnsmessage.Message) []byte {
	out, err := m.Pack()
	if err != nil {
		// Something in an answer could not be encoded; say so rather than
		// leave the client waiting.
		m.Answers, m.RCode = nil, dnsmessage.RCodeServerFailure
		out, _ = m.Pack()
	}
	return out
}

// lookup answers one question through the resolver. IPv6 is not routed to
// the guest, so AAAA questions get an empty answer, which sends clients
// straight to IPv4.
func lookup(ctx context.Context, r Resolver, q dnsmessage.Question) ([]dnsmessage.Resource, dnsmessage.RCode) {
	if q.Class != dnsmessage.ClassINET {
		return nil, dnsmessage.RCodeNotImplemented
	}
	// Without the final dot, the host applies its own DNS suffixes to
	// single-label names, as it would for its own programs.
	name := strings.TrimSuffix(q.Name.String(), ".")
	hdr := dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: dnsTTL}
	var out []dnsmessage.Resource
	add := func(body dnsmessage.ResourceBody) { out = append(out, dnsmessage.Resource{Header: hdr, Body: body}) }

	switch q.Type {
	case dnsmessage.TypeA:
		ips, err := r.LookupNetIP(ctx, "ip4", name)
		if err != nil {
			return nil, rcode(err)
		}
		for _, ip := range ips {
			if ip = ip.Unmap(); ip.Is4() {
				add(&dnsmessage.AResource{A: ip.As4()})
			}
		}
	case dnsmessage.TypeAAAA:
	case dnsmessage.TypeCNAME:
		cname, err := r.LookupCNAME(ctx, name)
		if err != nil {
			return nil, rcode(err)
		}
		if target, ok := dnsName(cname); ok && !strings.EqualFold(target.String(), fqdn(name)) {
			add(&dnsmessage.CNAMEResource{CNAME: target})
		}
	case dnsmessage.TypePTR:
		ip, ok := reverseIPv4(name)
		if !ok {
			return nil, dnsmessage.RCodeNameError
		}
		names, err := r.LookupAddr(ctx, ip.String())
		if err != nil {
			return nil, rcode(err)
		}
		for _, n := range names {
			if target, ok := dnsName(n); ok {
				add(&dnsmessage.PTRResource{PTR: target})
			}
		}
	case dnsmessage.TypeMX:
		mxs, err := r.LookupMX(ctx, name)
		if err != nil {
			return nil, rcode(err)
		}
		for _, mx := range mxs {
			if target, ok := dnsName(mx.Host); ok {
				add(&dnsmessage.MXResource{Pref: mx.Pref, MX: target})
			}
		}
	case dnsmessage.TypeTXT:
		txts, err := r.LookupTXT(ctx, name)
		if err != nil {
			return nil, rcode(err)
		}
		for _, t := range txts {
			add(&dnsmessage.TXTResource{TXT: split255(t)})
		}
	case dnsmessage.TypeSRV:
		_, srvs, err := r.LookupSRV(ctx, "", "", name)
		if err != nil {
			return nil, rcode(err)
		}
		for _, srv := range srvs {
			if target, ok := dnsName(srv.Target); ok {
				add(&dnsmessage.SRVResource{Priority: srv.Priority, Weight: srv.Weight, Port: srv.Port, Target: target})
			}
		}
	default:
		return nil, dnsmessage.RCodeNotImplemented
	}
	return out, dnsmessage.RCodeSuccess
}

// rcode turns a resolver error into a response code: NXDOMAIN when the name
// does not exist, SERVFAIL for anything else.
func rcode(err error) dnsmessage.RCode {
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return dnsmessage.RCodeNameError
	}
	return dnsmessage.RCodeServerFailure
}

func fqdn(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

func dnsName(name string) (dnsmessage.Name, bool) {
	n, err := dnsmessage.NewName(fqdn(name))
	return n, err == nil
}

// reverseIPv4 reads the address out of a name like 4.3.2.1.in-addr.arpa.
func reverseIPv4(name string) (netip.Addr, bool) {
	rest, ok := strings.CutSuffix(strings.ToLower(name), ".in-addr.arpa")
	if !ok {
		return netip.Addr{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 4 {
		return netip.Addr{}, false
	}
	for i, j := 0, 3; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	ip, err := netip.ParseAddr(strings.Join(parts, "."))
	return ip, err == nil && ip.Is4()
}

// split255 cuts a TXT record into the 255-byte strings DNS carries.
func split255(s string) []string {
	var out []string
	for len(s) > 255 {
		out = append(out, s[:255])
		s = s[255:]
	}
	return append(out, s)
}
