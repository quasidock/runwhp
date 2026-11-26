//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan *net.UnixConn, 1)
	go func() {
		c, _ := l.AcceptUnix()
		done <- c
	}()
	a, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	b := <-done
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(10 * time.Second))
	b.SetDeadline(time.Now().Add(10 * time.Second))
	return a, b
}

// TestSplice carries each side's half-close to the other, so that a
// client can end its request and still read the answer.
func TestSplice(t *testing.T) {
	client, inner := unixPair(t)
	outer, server := unixPair(t)
	go splice(inner, outer)

	io.WriteString(client, "request")
	client.CloseWrite()
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "request" {
		t.Fatalf("the server read %q, %v", got, err)
	}
	io.WriteString(server, "answer")
	server.CloseWrite()
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "answer" {
		t.Fatalf("the client read %q, %v", got, err)
	}
}

// TestListenSocket makes a mount's socket root's, mode 0660, and closes
// connections it cannot carry to the host, which a test has no vsock for.
func TestListenSocket(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, to give the socket to root")
	}
	spec := &guestspec.Spec{SocketToken: "x", Mounts: []guestspec.Mount{{Kind: guestspec.MountSocket, Target: "/var/run/docker.sock", Pipe: `\\.\pipe\p`}}}
	p, err := listenSocket(spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != socketMode {
		t.Fatalf("socket %v, %v", fi.Mode(), err)
	}
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(c); len(got) != 0 || err != nil {
		t.Errorf("read %q, %v: want the connection closed", got, err)
	}
}
