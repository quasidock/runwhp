//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// The embedded DNS's rules live in a table of runwhp's own, so a container's
// iptables, which works in tables of its own names, leaves them alone, as
// Docker's DOCKER_OUTPUT chain is left alone by a container's own rules.
const (
	nftTable = "runwhp"
	nftChain = "output"
	nfAccept = 1 // NF_ACCEPT
	// natDstPriority is NF_IP_PRI_NAT_DST, -100, as a uint32: where DNAT
	// runs on the output hook, nftables's "dstnat".
	natDstPriority = 1<<32 - 100
)

// installDNSRedirect has the kernel send what the container sends to
// 127.0.0.11:53 to the embedded DNS's own ports, as Docker does in each
// container's namespace, so that the container may bind port 53 itself:
// one nftables batch of a table, a nat chain on the output hook, and a
// DNAT rule each for UDP and TCP. Replies are un-NATed by conntrack.
func installDNSRedirect(udpPort, tcpPort uint16) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	batch, acks := dnsRedirectBatch(udpPort, tcpPort)
	if err := unix.Sendto(fd, batch, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for acks > 0 {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Type != unix.NLMSG_ERROR || len(m.Data) < 4 {
				continue
			}
			if errno := int32(binary.NativeEndian.Uint32(m.Data)); errno != 0 {
				return fmt.Errorf("nftables message %d: %w", m.Header.Seq, syscall.Errno(-errno))
			}
			acks--
		}
	}
	return nil
}

// dnsRedirectBatch encodes the batch, and says how many acknowledgements
// its messages ask for.
func dnsRedirectBatch(udpPort, tcpPort uint16) ([]byte, int) {
	var b nlBuilder
	b.batch(unix.NFNL_MSG_BATCH_BEGIN)
	b.message(unix.NFT_MSG_NEWTABLE, unix.NLM_F_CREATE, func() {
		b.str(unix.NFTA_TABLE_NAME, nftTable)
	})
	b.message(unix.NFT_MSG_NEWCHAIN, unix.NLM_F_CREATE, func() {
		b.str(unix.NFTA_CHAIN_TABLE, nftTable)
		b.str(unix.NFTA_CHAIN_NAME, nftChain)
		b.nest(unix.NFTA_CHAIN_HOOK, func() {
			b.u32(unix.NFTA_HOOK_HOOKNUM, unix.NF_INET_LOCAL_OUT)
			b.u32(unix.NFTA_HOOK_PRIORITY, natDstPriority)
		})
		b.u32(unix.NFTA_CHAIN_POLICY, nfAccept)
		b.str(unix.NFTA_CHAIN_TYPE, "nat")
	})
	for _, r := range []struct {
		proto uint8
		port  uint16
	}{{unix.IPPROTO_UDP, udpPort}, {unix.IPPROTO_TCP, tcpPort}} {
		b.message(unix.NFT_MSG_NEWRULE, unix.NLM_F_CREATE|unix.NLM_F_APPEND, func() {
			b.str(unix.NFTA_RULE_TABLE, nftTable)
			b.str(unix.NFTA_RULE_CHAIN, nftChain)
			b.nest(unix.NFTA_RULE_EXPRESSIONS, func() {
				// ip daddr 127.0.0.11
				b.payload(unix.NFT_PAYLOAD_NETWORK_HEADER, 16, 4)
				b.cmpEq(embeddedDNS[:])
				// meta l4proto udp (or tcp)
				b.expr("meta", func() {
					b.u32(unix.NFTA_META_DREG, unix.NFT_REG_1)
					b.u32(unix.NFTA_META_KEY, unix.NFT_META_L4PROTO)
				})
				b.cmpEq([]byte{r.proto})
				// th dport 53
				b.payload(unix.NFT_PAYLOAD_TRANSPORT_HEADER, 2, 2)
				b.cmpEq([]byte{0, 53})
				// dnat to 127.0.0.11:port
				b.immediate(unix.NFT_REG_1, embeddedDNS[:])
				b.immediate(unix.NFT_REG_2, binary.BigEndian.AppendUint16(nil, r.port))
				b.expr("nat", func() {
					b.u32(unix.NFTA_NAT_TYPE, unix.NFT_NAT_DNAT)
					b.u32(unix.NFTA_NAT_FAMILY, unix.NFPROTO_IPV4)
					b.u32(unix.NFTA_NAT_REG_ADDR_MIN, unix.NFT_REG_1)
					b.u32(unix.NFTA_NAT_REG_PROTO_MIN, unix.NFT_REG_2)
				})
			})
		})
	}
	b.batch(unix.NFNL_MSG_BATCH_END)
	return b.buf, b.acks
}

// embeddedDNS is Docker's address for its embedded DNS server.
var embeddedDNS = [4]byte{127, 0, 0, 11}

// nlBuilder encodes netfilter netlink messages: netlink headers in host
// order, a struct nfgenmsg, and attributes, nested ones flagged so, whose
// integers nftables takes in network order.
type nlBuilder struct {
	buf  []byte
	seq  uint32
	acks int
}

func (b *nlBuilder) header(typ, flags uint16, family uint8, resID uint16) int {
	b.seq++
	start := len(b.buf)
	ne := binary.NativeEndian
	b.buf = ne.AppendUint32(b.buf, 0) // nlmsg_len, filled in by end
	b.buf = ne.AppendUint16(b.buf, typ)
	b.buf = ne.AppendUint16(b.buf, flags)
	b.buf = ne.AppendUint32(b.buf, b.seq)
	b.buf = ne.AppendUint32(b.buf, 0)
	b.buf = append(b.buf, family, unix.NFNETLINK_V0)
	b.buf = binary.BigEndian.AppendUint16(b.buf, resID)
	return start
}

func (b *nlBuilder) end(start int) {
	binary.NativeEndian.PutUint32(b.buf[start:], uint32(len(b.buf)-start))
}

// batch is a batch's beginning or end, which addresses the nftables
// subsystem.
func (b *nlBuilder) batch(typ uint16) {
	b.end(b.header(typ, unix.NLM_F_REQUEST, unix.AF_UNSPEC, unix.NFNL_SUBSYS_NFTABLES))
}

// message is one nftables message, for IPv4, acknowledged.
func (b *nlBuilder) message(typ uint16, flags uint16, attrs func()) {
	start := b.header(unix.NFNL_SUBSYS_NFTABLES<<8|typ, unix.NLM_F_REQUEST|unix.NLM_F_ACK|flags, unix.NFPROTO_IPV4, 0)
	attrs()
	b.end(start)
	b.acks++
}

func (b *nlBuilder) attr(typ uint16, data []byte) {
	ne := binary.NativeEndian
	b.buf = ne.AppendUint16(b.buf, uint16(4+len(data)))
	b.buf = ne.AppendUint16(b.buf, typ)
	b.buf = append(b.buf, data...)
	b.pad()
}

func (b *nlBuilder) pad() {
	for len(b.buf)%4 != 0 {
		b.buf = append(b.buf, 0)
	}
}

func (b *nlBuilder) str(typ uint16, s string) { b.attr(typ, append([]byte(s), 0)) }

func (b *nlBuilder) u32(typ uint16, v uint32) { b.attr(typ, binary.BigEndian.AppendUint32(nil, v)) }

func (b *nlBuilder) nest(typ uint16, attrs func()) {
	start := len(b.buf)
	b.buf = append(b.buf, 0, 0, 0, 0)
	attrs()
	binary.NativeEndian.PutUint16(b.buf[start:], uint16(len(b.buf)-start))
	binary.NativeEndian.PutUint16(b.buf[start+2:], typ|unix.NLA_F_NESTED)
}

// expr is one expression of a rule: its name and its data.
func (b *nlBuilder) expr(name string, data func()) {
	b.nest(unix.NFTA_LIST_ELEM, func() {
		b.str(unix.NFTA_EXPR_NAME, name)
		b.nest(unix.NFTA_EXPR_DATA, data)
	})
}

// payload loads n bytes at offset from the packet's base into register 1.
func (b *nlBuilder) payload(base, offset, n uint32) {
	b.expr("payload", func() {
		b.u32(unix.NFTA_PAYLOAD_DREG, unix.NFT_REG_1)
		b.u32(unix.NFTA_PAYLOAD_BASE, base)
		b.u32(unix.NFTA_PAYLOAD_OFFSET, offset)
		b.u32(unix.NFTA_PAYLOAD_LEN, n)
	})
}

// cmpEq ends the rule's match unless register 1 holds v.
func (b *nlBuilder) cmpEq(v []byte) {
	b.expr("cmp", func() {
		b.u32(unix.NFTA_CMP_SREG, unix.NFT_REG_1)
		b.u32(unix.NFTA_CMP_OP, unix.NFT_CMP_EQ)
		b.nest(unix.NFTA_CMP_DATA, func() { b.attr(unix.NFTA_DATA_VALUE, v) })
	})
}

// immediate loads v into a register.
func (b *nlBuilder) immediate(reg uint32, v []byte) {
	b.expr("immediate", func() {
		b.u32(unix.NFTA_IMMEDIATE_DREG, reg)
		b.nest(unix.NFTA_IMMEDIATE_DATA, func() { b.attr(unix.NFTA_DATA_VALUE, v) })
	})
}
