//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The batch is a begin, a table, a chain, a rule each for UDP and TCP, and
// an end; the four between ask for acknowledgements, and each rule DNATs to
// its own port.
func TestDNSRedirectBatch(t *testing.T) {
	batch, acks := dnsRedirectBatch(40000, 40001)
	msgs, err := syscall.ParseNetlinkMessage(batch)
	if err != nil {
		t.Fatal(err)
	}
	nft := func(m uint16) uint16 { return unix.NFNL_SUBSYS_NFTABLES<<8 | m }
	want := []uint16{unix.NFNL_MSG_BATCH_BEGIN, nft(unix.NFT_MSG_NEWTABLE), nft(unix.NFT_MSG_NEWCHAIN),
		nft(unix.NFT_MSG_NEWRULE), nft(unix.NFT_MSG_NEWRULE), unix.NFNL_MSG_BATCH_END}
	if len(msgs) != len(want) || acks != 4 {
		t.Fatalf("%d messages, %d acks", len(msgs), acks)
	}
	for i, m := range msgs {
		if m.Header.Type != want[i] || m.Header.Seq != uint32(i+1) {
			t.Errorf("message %d: type %#x seq %d, want %#x", i, m.Header.Type, m.Header.Seq, want[i])
		}
		if ack := m.Header.Flags&unix.NLM_F_ACK != 0; ack != (i > 0 && i < 5) {
			t.Errorf("message %d: ack %v", i, ack)
		}
	}
	for i, port := range []uint16{40000, 40001} {
		rule := msgs[3+i].Data
		if !bytes.Contains(rule, binary.BigEndian.AppendUint16(nil, port)) || !bytes.Contains(rule, []byte("nat\x00")) {
			t.Errorf("rule %d does not DNAT to port %d", i, port)
		}
	}
}

// fakeUpstream answers DNS queries over UDP and TCP with the query
// reversed, so an answer shows which query it answers.
func fakeUpstream(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); ln.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(reverse(buf[:n]), from)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				q, _ := io.ReadAll(c)
				c.Write(reverse(q))
			}()
		}
	}()
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
}

func reverse(b []byte) []byte {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return r
}

// The relays pass each query on and its answer back, UDP queries at once
// each to its own client, and TCP as a stream, half-close included.
func TestEmbeddedDNSRelay(t *testing.T) {
	up := fakeUpstream(t)
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	defer ln.Close()
	go relayDNSUDP(pc, up)
	go relayDNSTCP(ln, up)

	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(q []byte) {
			c, err := net.DialUDP("udp4", nil, pc.LocalAddr().(*net.UDPAddr))
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			c.Write(q)
			buf := make([]byte, 512)
			n, err := c.Read(buf)
			if err == nil && !bytes.Equal(buf[:n], reverse(q)) {
				err = io.ErrUnexpectedEOF
			}
			done <- err
		}([]byte{byte(i), 'q', 'u', 'e', 'r', 'y'})
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Errorf("udp: %v", err)
		}
	}

	c, err := net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("\x00\x05hello"))
	c.CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "olleh\x05\x00" {
		t.Errorf("tcp: %q, %v", got, err)
	}
}
