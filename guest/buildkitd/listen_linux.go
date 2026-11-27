//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/bklog"
	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// vsockBuffer is each connection's buffer, as init's sockets have: a
// build context crossing at the default 256 KiB waits for credit often.
const vsockBuffer = 2 << 20

// vsockListener accepts vsock connections on a port of the guest's.
type vsockListener struct {
	fd   int
	port uint32
	once sync.Once
}

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 64); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &vsockListener{fd: fd, port: port}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		nfd, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
			continue
		}
		if err != nil {
			return nil, err
		}
		unix.SetsockoptUint64(nfd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, vsockBuffer)
		unix.SetsockoptUint64(nfd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_SIZE, vsockBuffer)
		remote := vsockAddr{port: 0}
		if vm, ok := sa.(*unix.SockaddrVM); ok {
			remote = vsockAddr{cid: vm.CID, port: vm.Port}
		}
		return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), fd: nfd, local: vsockAddr{port: l.port}, remote: remote}, nil
	}
}

func (l *vsockListener) Close() error {
	var err error
	l.once.Do(func() { err = unix.Close(l.fd) })
	return err
}

func (l *vsockListener) Addr() net.Addr { return vsockAddr{port: l.port} }

type vsockAddr struct{ cid, port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("vsock:%d:%d", a.cid, a.port) }

// vsockConn is a vsock connection as a net.Conn, its file in the runtime's
// poller.
type vsockConn struct {
	*os.File
	fd            int
	local, remote vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }
func (c *vsockConn) CloseWrite() error    { return unix.Shutdown(c.fd, unix.SHUT_WR) }

// bufferedConn is a connection whose first bytes were read past its hello.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// connListener hands the gRPC server the connections whose hello asked for
// BuildKit's API.
type connListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{addr: addr, conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }

func (l *connListener) hand(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.closed:
		c.Close()
	}
}

// helloTimeout bounds how long a connection may take to say what it is.
const helloTimeout = 10 * time.Second

// serveHellos reads each connection's hello and hands it on: to the gRPC
// server, to the session manager, or answers a ping.
func serveHellos(ctx context.Context, l net.Listener, grpcConns *connListener, sm *session.Manager) {
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				bklog.L.Errorf("vsock: accept: %v", err)
			}
			grpcConns.Close()
			return
		}
		go func() {
			hello, conn, err := readHello(c)
			if err != nil {
				bklog.L.Warnf("vsock: %v", err)
				c.Close()
				return
			}
			switch hello.Kind {
			case guestspec.BuildKitGRPC:
				grpcConns.hand(conn)
			case guestspec.BuildKitSession:
				if err := sm.HandleConn(ctx, conn, hello.Headers); err != nil {
					bklog.L.Warnf("session: %v", err)
				}
				conn.Close()
			case guestspec.BuildKitPing:
				io.WriteString(conn, "ok\n")
				conn.Close()
			default:
				bklog.L.Warnf("vsock: a connection asked for %q", hello.Kind)
				conn.Close()
			}
		}()
	}
}

// readHello reads a connection's first line, its hello, and returns the
// connection with what followed it.
func readHello(c net.Conn) (guestspec.BuildKitHello, net.Conn, error) {
	var h guestspec.BuildKitHello
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	r := bufio.NewReaderSize(c, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return h, nil, fmt.Errorf("reading a connection's hello: %w", err)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, nil, fmt.Errorf("a connection's hello: %w", err)
	}
	c.SetReadDeadline(time.Time{})
	return h, &bufferedConn{Conn: c, r: r}, nil
}
