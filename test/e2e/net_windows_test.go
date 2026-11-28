//go:build windows

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

const hostGateway = "host.docker.internal:host-gateway"

// hostServer serves body at / on the host's 127.0.0.1, which the guest
// reaches only through host-gateway.
func hostServer(t *testing.T, body []byte) (port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	_, port, _ = net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return port
}

func TestNetworkHostGateway(t *testing.T) {
	parallel(t)
	runwhp(t)
	port := hostServer(t, []byte("hello from the host\n"))
	r := run(t, "--add-host", hostGateway, alpine, "wget", "-q", "-O-", "http://host.docker.internal:"+port+"/")
	expect(t, r, "hello from the host\n", 0)
}

// TestNetworkGatewayOptIn checks that the host's loopback stays closed to a
// container that did not ask for host-gateway.
func TestNetworkGatewayOptIn(t *testing.T) {
	parallel(t)
	runwhp(t)
	port := hostServer(t, []byte("secret\n"))
	r := run(t, alpine, "wget", "-q", "-T", "5", "-O-", "http://10.0.2.2:"+port+"/")
	if r.code == 0 || r.stdout != "" || !strings.Contains(r.stderr, "refused") {
		t.Errorf("exit %d, stdout %q, stderr %q; want the connection refused", r.code, r.stdout, r.stderr)
	}
}

func TestNetworkDownload(t *testing.T) {
	parallel(t)
	runwhp(t)
	data := make([]byte, 20<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	port := hostServer(t, data)
	r := run(t, "--add-host", hostGateway, alpine, "sh", "-c", "wget -q -O- http://host.docker.internal:"+port+"/ | sha256sum")
	expect(t, r, hex.EncodeToString(sum[:])+"  -\n", 0)
}

// TestNetworkInternet resolves a name through the host's resolver and
// fetches over TLS, as package managers do.
func TestNetworkInternet(t *testing.T) {
	parallel(t)
	r := run(t, alpine, "sh", "-c", "cat /etc/resolv.conf; wget -q -O /dev/null https://dl-cdn.alpinelinux.org/alpine/ && echo fetched")
	expect(t, r, "nameserver 10.0.2.3\nfetched\n", 0)
}

func TestNetworkNone(t *testing.T) {
	parallel(t)
	expect(t, run(t, "--network", "none", alpine, "ls", "/sys/class/net"), "lo\n", 0)
	r := run(t, "--network", "none", "-p", "8080:80", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "--network none") {
		t.Errorf("--network none with -p: exit %d, stderr %q", r.code, r.stderr)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// background starts runwhp run, killing it if the test ends first or it
// runs for two minutes, so a failure never leaves the test waiting on it.
func background(t *testing.T, stdout, stderr io.Writer, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, runwhp(t), runArgs(args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// TestPublish reads from a published port. The container listens a second
// after it starts; connections before that are closed, and the test tries
// again, as a client of a starting server would.
func TestPublish(t *testing.T) {
	parallel(t)
	port := freePort(t)
	var stderr bytes.Buffer
	cmd := background(t, io.Discard, &stderr, "-p", "127.0.0.1:"+port+":8080", alpine, "sh", "-c",
		"sleep 1; echo hi | nc -l -p 8080")
	var got []byte
	for deadline := time.Now().Add(30 * time.Second); string(got) != "hi\n" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		got, _ = io.ReadAll(c)
		c.Close()
	}
	if string(got) != "hi\n" {
		t.Errorf("read %q", got)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("%v; stderr %q", err, stderr.String())
	}
}

func TestPublishUpload(t *testing.T) {
	parallel(t)
	port := freePort(t)
	data := make([]byte, 20<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	var stdout bytes.Buffer
	var stderr syncBuffer
	// Ready once port 8080 (1F90) is in LISTEN state (0A): a connection made
	// earlier would be closed, and nc takes only one. busybox nc listens on
	// a dual-stack socket, which shows in tcp6.
	cmd := background(t, &stdout, &stderr, "-p", port+":8080", alpine, "sh", "-c",
		`nc -l -p 8080 | sha256sum & until grep -qE ":1F90 [0-9A-F]+:0000 0A" /proc/net/tcp /proc/net/tcp6; do sleep 0.05; done; echo ready >&2; wait`)
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(stderr.String(), "ready"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("container never got ready; stderr %q", stderr.String())
		}
	}
	nc, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*net.TCPConn)
	c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	c.CloseWrite()
	io.Copy(io.Discard, c)
	c.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v; stderr %q", err, stderr.String())
	}
	if want := hex.EncodeToString(sum[:]) + "  -\n"; stdout.String() != want {
		t.Errorf("container hashed %q, want %q", stdout.String(), want)
	}
}

func TestPublishBusyPort(t *testing.T) {
	parallel(t)
	runwhp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	r := run(t, "-p", fmt.Sprintf("%s:80", port), alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "publishing port") {
		t.Errorf("exit %d, stderr %q; want 125 and the port named", r.code, r.stderr)
	}
}

// hostIPv4 is an address of the host's own, not its loopback, that Windows
// itself can ping, which the guest's pings reach through the host.
func hostIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			ip := ipn.IP.String()
			if exec.Command("ping", "-n", "1", "-w", "1000", ip).Run() == nil {
				return ip
			}
		}
	}
	t.Skip("the host has no address of its own that answers a ping")
	return ""
}

// TestPing pings beyond the host, the host's own address being the one
// sure to answer without the internet: the guest's echo goes out from the
// host and its reply comes back from where it went. An address the guest
// may not reach is not answered; a TTL of 1 runs out at the gateway, the
// first hop of traceroute's ICMP probes; and host-gateway on a network of
// the user's reaches the host's loopback.
func TestPing(t *testing.T) {
	parallel(t)
	ip := hostIPv4(t)
	r := run(t, alpine, "sh", "-c",
		"ping -c 2 -W 2 "+ip+" | grep -c 'bytes from "+ip+"'; "+
			"ping -c 1 -W 1 10.0.2.99 >/dev/null || echo unanswered; "+
			"traceroute -I -n -q 1 -w 2 -m 2 "+ip+" 2>/dev/null | tail -n +2 | awk '{print $1, $2}'")
	expect(t, r, "2\nunanswered\n1 10.0.2.2\n2 "+ip+"\n", 0)

	net := newNetwork(t)
	r = run(t, "--network", net, "--add-host", hostGateway, alpine, "sh", "-c",
		"ping -c 1 -W 2 host.docker.internal | grep -c 'bytes from 10.0.2.2'; ping -c 1 -W 2 "+ip+" | grep -c 'bytes from'")
	expect(t, r, "1\n1\n", 0)
}

// udpEcho is a container's UDP server on port 9999, answering each
// datagram upper-cased.
const udpEcho = `
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("", 9999))
while True:
    d, a = s.recvfrom(65535)
    s.sendto(d.upper(), a)
`

// udpPonger is a container's UDP server on port 9999, for images with
// busybox alone, whose nc cannot run a program for a UDP peer: nc answers
// the first client with a "pong" line a fifth of a second.
const udpPonger = "while true; do echo pong; sleep 0.2; done | nc -lu -p 9999"

func freeUDPPort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	return port
}

// askUDP sends msg to addr until it is answered, as a client of a
// starting server would, and returns the answer.
func askUDP(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 64<<10)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		c.Write([]byte(msg))
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, err := c.Read(buf); err == nil {
			return string(buf[:n])
		}
	}
	t.Fatalf("nothing answered %q at %s", msg, addr)
	return ""
}

// TestPublishUDP publishes a UDP port, on a random host port, as -p and -P
// do with Docker: datagrams from clients on the host reach the container's
// server, each client's answers come back to it, and port and state name
// the port with its protocol.
func TestPublishUDP(t *testing.T) {
	parallel(t)
	name := newName(t)
	detached(t, "--name", name, "-p", "9999/udp", "--expose", "9998/udp", "-P", "python:3.12-slim", "python3", "-c", udpEcho)
	r := cli(t, "port", name, "9999/udp")
	addr := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Fatalf("port 9999/udp: %+v", r)
	}
	if got := askUDP(t, addr, "hello"); got != "HELLO" {
		t.Errorf("got %q", got)
	}
	done := make(chan string, 2)
	for _, msg := range []string{"one", "two"} {
		go func() { done <- askUDP(t, addr, msg) }()
	}
	if a, b := <-done, <-done; a+b != "ONETWO" && a+b != "TWOONE" {
		t.Errorf("two clients at once got %q and %q", a, b)
	}
	r = cli(t, "port", name)
	if !strings.Contains(r.stdout, "9999/udp -> "+addr) || !strings.Contains(r.stdout, "9998/udp -> 127.0.0.1:") {
		t.Errorf("port: %q", r.stdout)
	}
	if r := cli(t, "port", name, "9999"); r.code == 0 || !strings.Contains(r.stderr, "no public port '9999/tcp'") {
		t.Errorf("port 9999, TCP: %+v", r)
	}
	if s := stateOf(t, name); !slices.Contains(s.Ports, addr+"->9999/udp") {
		t.Errorf("state's ports %q, want %s->9999/udp", s.Ports, addr)
	}
}

// TestPublishAddress sets the address of ports published without one, as
// Docker's daemon's --ip does, here to another loopback address, which
// Windows Firewall does not ask about; an address given stays, and a
// setting that is not an address fails the run.
func TestPublishAddress(t *testing.T) {
	parallel(t)
	setEnv(t, "RUNWHP_PUBLISH_ADDRESS", "127.0.0.2")
	name := newName(t)
	detached(t, "--name", name, "-p", "8080", "-p", "127.0.0.1::8081", alpine, "sh", "-c", "nc -lk -p 8081 -e echo served & exec nc -lk -p 8080 -e echo served")
	for guest, prefix := range map[string]string{"8080": "127.0.0.2:", "8081": "127.0.0.1:"} {
		addr := strings.TrimSpace(cli(t, "port", name, guest).stdout)
		if !strings.HasPrefix(addr, prefix) {
			t.Fatalf("port %s at %q, want %s", guest, addr, prefix)
		}
		var got string
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && got != "served\n"; time.Sleep(100 * time.Millisecond) {
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				b, _ := io.ReadAll(c)
				c.Close()
				got = string(b)
			}
		}
		if got != "served\n" {
			t.Errorf("%s served %q", addr, got)
		}
	}
	setEnv(t, "RUNWHP_PUBLISH_ADDRESS", "nowhere")
	if r := run(t, "-p", "80", alpine, "true"); r.code != 125 || !strings.Contains(r.stderr, `RUNWHP_PUBLISH_ADDRESS="nowhere" is not an IP address`) {
		t.Errorf("a bad setting: %+v", r)
	}
}

// TestDNSDefaultNetwork gives a container on the default network DNS
// servers, search domains and options of its own, which resolv.conf lists
// as given, as Docker's does on its default bridge; and refuses them with
// --network none, as Docker does.
func TestDNSDefaultNetwork(t *testing.T) {
	parallel(t)
	r := run(t, "--dns", "10.0.2.3", "--dns", "192.0.2.53", "--dns-search", "corp.test", "--dns-option", "ndots:2", "--dns-option", "timeout:1",
		alpine, "sh", "-c", "cat /etc/resolv.conf; nslookup localhost. | grep -o 'Address: 127.0.0.1'")
	expect(t, r, "nameserver 10.0.2.3\nnameserver 192.0.2.53\nsearch corp.test\noptions ndots:2 timeout:1\nAddress: 127.0.0.1\n", 0)
	r = run(t, "--network", "none", "--dns", "1.1.1.1", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "conflicting options: dns and the network mode") {
		t.Errorf("--network none --dns: %+v", r)
	}
}
