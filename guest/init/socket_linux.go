//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// socketDir is where init makes the sockets of MountSocket mounts, in the
// initramfs, before binding each over its mount point in the container's
// root: what the container sees is the mount, not a file of its own
// layer.
const socketDir = "/runwhp/sock"

// socketMode is a socket's mode, root's, as Docker's own socket is its
// daemon's: a user other than root reaches it through a proxy of its own,
// as Dev Containers' docker-outside-of-docker runs one.
const socketMode = 0o660

// spliceBuffer is how much a forwarded connection reads at a time, each
// way, and vsockBuffer how much its connection to the host buffers.
const (
	spliceBuffer = 256 << 10
	vsockBuffer  = 2 << 20
)

// listenSocket makes the socket of mount i and starts carrying its
// connections to the host. It returns the socket's path, for mountAll to
// bind over the mount point.
func listenSocket(spec *guestspec.Spec, i int) (string, error) {
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(socketDir, strconv.Itoa(i))
	os.Remove(p)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		return "", fmt.Errorf("making the socket: %w", err)
	}
	if err := os.Chown(p, 0, 0); err != nil {
		l.Close()
		return "", err
	}
	if err := os.Chmod(p, socketMode); err != nil {
		l.Close()
		return "", err
	}
	f := &socketForwarder{token: spec.SocketToken, mount: i, target: spec.Mounts[i].Target, pipe: spec.Mounts[i].Pipe}
	go f.serve(l)
	return p, nil
}

type socketForwarder struct {
	token  string
	mount  int
	target string
	pipe   string

	warned sync.Once
}

func (f *socketForwarder) serve(l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			logf("socket %s: accept: %v", f.target, err)
			return
		}
		go f.forward(c)
	}
}

// forward carries one connection to the host and back.
func (f *socketForwarder) forward(c *net.UnixConn) {
	defer c.Close()
	host, err := f.connect()
	if err != nil {
		debugf("socket %s: %v", f.target, err)
		return
	}
	defer host.Close()
	splice(c, host)
}

// connect opens a connection to the host for the mount's pipe.
func (f *socketForwarder) connect() (*vsockConn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		f.warned.Do(func() { logf("socket %s cannot reach the host: vsock: %v", f.target, err) })
		return nil, err
	}
	// A buffer larger than vsock's 256 KiB lets the host send on while
	// init passes what came before to the client.
	unix.SetsockoptUint64(fd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, vsockBuffer)
	unix.SetsockoptUint64(fd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_SIZE, vsockBuffer)
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: guestspec.PipePort}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("connecting to the host: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	v := &vsockConn{os.NewFile(uintptr(fd), "vsock")}
	hello, _ := json.Marshal(guestspec.SocketHello{Token: f.token, Mount: f.mount})
	if _, err := v.Write(append(hello, '\n')); err != nil {
		v.Close()
		return nil, err
	}
	var status [1]byte
	if _, err := io.ReadFull(v, status[:]); err != nil {
		v.Close()
		return nil, fmt.Errorf("the host closed the connection to %s: %w", f.pipe, err)
	}
	if status[0] != guestspec.SocketOK {
		v.Close()
		return nil, fmt.Errorf("the host could not connect to %s", f.pipe)
	}
	return v, nil
}

// vsockConn is a connected vsock socket, with a half-close.
type vsockConn struct{ *os.File }

func (v *vsockConn) CloseWrite() error {
	rc, err := v.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

// halfCloser is a connection whose sending side can be shut alone.
type halfCloser interface {
	io.ReadWriter
	CloseWrite() error
}

// splice copies a to b and b to a until both have ended, carrying each
// side's half-close to the other.
func splice(a, b halfCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	one := func(dst, src halfCloser) {
		defer wg.Done()
		// In large pieces, and through the buffer, not a ReaderFrom's own.
		if _, err := io.CopyBuffer(struct{ io.Writer }{dst}, src, make([]byte, spliceBuffer)); err != nil && !errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.ECONNRESET) &&
			!errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
			debugf("socket: %v", err)
		}
		dst.CloseWrite()
	}
	go one(a, b)
	go one(b, a)
	wg.Wait()
}
