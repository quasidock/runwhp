//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

const (
	// embeddedUDPWait outlasts the host's own wait for the resolver, so the
	// host's answer, even SERVFAIL, is what the container gets.
	embeddedUDPWait  = 12 * time.Second
	embeddedTCPIdle  = 30 * time.Second
	embeddedInFlight = 128
)

// startEmbeddedDNS serves DNS at 127.0.0.11, as Docker's embedded server
// does on a user-defined network, by passing every query on to upstream,
// the host's server at the network's gateway, which knows the containers'
// names. It listens on ports the kernel chooses and has the kernel redirect
// port 53 to them, as Docker does, so that the container may serve port 53
// itself. 127.0.0.11 needs no address of its own: all of 127/8 is lo's.
func startEmbeddedDNS(upstream netip.Addr) error {
	pc, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4(embeddedDNS), 0)))
	if err != nil {
		return err
	}
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4(embeddedDNS), 0)))
	if err != nil {
		pc.Close()
		return err
	}
	udpPort := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	tcpPort := uint16(ln.Addr().(*net.TCPAddr).Port)
	if err := installDNSRedirect(udpPort, tcpPort); err != nil {
		pc.Close()
		ln.Close()
		return fmt.Errorf("redirecting port 53: %w", err)
	}
	up := netip.AddrPortFrom(upstream, 53)
	go relayDNSUDP(pc, up)
	go relayDNSTCP(ln, up)
	return nil
}

// relayDNSUDP answers each query with what upstream answers it with, a
// fresh socket a query, so that answers cannot cross.
func relayDNSUDP(pc *net.UDPConn, upstream netip.AddrPort) {
	slots := make(chan struct{}, embeddedInFlight)
	for {
		buf := make([]byte, 64<<10)
		n, client, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logf("embedded DNS: %v", err)
			}
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			continue // as a busy server would, drop it: the client asks again
		}
		go func(query []byte) {
			defer func() { <-slots }()
			c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(upstream))
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(embeddedUDPWait))
			if _, err := c.Write(query); err != nil {
				return
			}
			reply := make([]byte, 64<<10)
			n, err := c.Read(reply)
			if err != nil {
				return
			}
			pc.WriteToUDPAddrPort(reply[:n], client)
		}(buf[:n])
	}
}

// relayDNSTCP carries each connection to upstream, DNS over TCP being a
// stream of length-prefixed messages either way.
func relayDNSTCP(ln *net.TCPListener, upstream netip.AddrPort) {
	for {
		c, err := ln.AcceptTCP()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logf("embedded DNS: %v", err)
			}
			return
		}
		go func() {
			defer c.Close()
			u, err := net.DialTCP("tcp4", nil, net.TCPAddrFromAddrPort(upstream))
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{})
			go func() {
				copyIdle(u, c)
				u.CloseWrite()
				close(done)
			}()
			copyIdle(c, u)
			c.CloseWrite()
			<-done
		}()
	}
}

// copyIdle copies until src ends or neither side has moved anything for
// embeddedTCPIdle.
func copyIdle(dst, src net.Conn) {
	buf := make([]byte, 16<<10)
	for {
		src.SetReadDeadline(time.Now().Add(embeddedTCPIdle))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				src.Close()
			}
			return
		}
	}
}
