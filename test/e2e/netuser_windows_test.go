//go:build windows

package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newNetwork makes a network of the test's own, removed when the test
// ends, after its containers.
func newNetwork(t *testing.T) string {
	t.Helper()
	var b [4]byte
	rand.Read(b[:])
	name := "e2e-net-" + hex.EncodeToString(b[:])
	if r := cli(t, "network", "create", name); r.code != 0 {
		t.Fatalf("network create: %+v", r)
	}
	t.Cleanup(func() {
		if r := cli(t, "network", "rm", name); r.code != 0 {
			t.Errorf("removing network %s after the test: %+v", name, r)
		}
	})
	return name
}

// serving runs a container in the background on the networks given in
// args, answering each TCP connection on port 5000 with its name, and
// returns that name.
func serving(t *testing.T, args ...string) string {
	t.Helper()
	name := newName(t)
	detached(t, append(append([]string{"--name", name}, args...), alpine, "sh", "-c", "nc -lk -p 5000 -e echo "+name)...)
	return name
}

// waitServing waits until a client on network net can reach name's port
// 5000, as a container in the background may not listen yet.
func waitServing(t *testing.T, net, name string) {
	t.Helper()
	r := run(t, "--network", net, alpine, "sh", "-c",
		"for i in $(seq 50); do r=$(nc -w 1 "+name+" 5000 </dev/null) && [ -n \"$r\" ] && exit 0; sleep 0.2; done; exit 1")
	if r.code != 0 {
		t.Fatalf("%s never answered on %s: %+v", name, net, r)
	}
}

type networkStateJSON struct {
	NetworkMode string `json:"networkMode"`
	Networks    map[string]struct {
		IPAddress  string   `json:"ipAddress"`
		MacAddress string   `json:"macAddress"`
		Gateway    string   `json:"gateway"`
		DNSNames   []string `json:"dnsNames"`
	} `json:"networks"`
}

func networksOf(t *testing.T, ref string) networkStateJSON {
	t.Helper()
	r := cli(t, "state", ref)
	var s networkStateJSON
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &s) != nil {
		t.Fatalf("state %s: %+v", ref, r)
	}
	return s
}

func TestUserNetworkCommands(t *testing.T) {
	s := newOwnStore(t)
	id := strings.TrimSpace(s.ok("network", "create", "front").stdout)
	if len(id) != 64 {
		t.Fatalf("create printed %q", id)
	}
	for args, want := range map[string]string{
		"network create front":                     "network with name front already exists",
		"network create bridge":                    "operation is not permitted on predefined bridge network",
		"network create -d macvlan back":           "network driver macvlan is not supported",
		"network create --subnet 10.9.0.0/16 back": "--subnet not supported",
		"network rm user":                          "operation is not permitted on predefined user network",
		"network rm nosuch":                        "network nosuch not found",
		"network inspect nosuch":                   "network nosuch not found",
	} {
		if r := s.cli("", strings.Fields(args)...); r.code != 1 || !strings.Contains(r.stderr, want) {
			t.Errorf("%s: %+v, want %q", args, r, want)
		}
	}
	ls := s.ok("network", "ls").stdout
	if want := "NETWORK ID     NAME    DRIVER   SCOPE\n" + id[:12] + "   front   bridge   local\n"; !strings.HasPrefix(ls, want) ||
		!strings.Contains(ls, "   none    null     local\n") || !strings.Contains(ls, "   user    user     local\n") {
		t.Errorf("ls:\n%s", ls)
	}
	if q := s.ok("network", "ls", "-q", "--no-trunc").stdout; !strings.HasPrefix(q, id+"\n") || strings.Count(q, "\n") != 3 {
		t.Errorf("ls -q --no-trunc:\n%s", q)
	}

	// A running container shows in inspect, and keeps the network from
	// being removed; once it has stopped, it does not.
	name := "web"
	s.ok(append([]string{"run", "-d", "--name", name, "--network", "front"}, withFlags([]string{alpine, "sleep", "300"})...)...)
	var nets []struct {
		Name, Id, Driver string
		IPAM             struct {
			Config []struct{ Subnet, Gateway string }
		}
		Containers map[string]struct{ Name, MacAddress, IPv4Address string }
	}
	r := s.ok("network", "inspect", "front")
	if err := json.Unmarshal([]byte(r.stdout), &nets); err != nil || len(nets) != 1 {
		t.Fatalf("inspect: %v\n%s", err, r.stdout)
	}
	n := nets[0]
	if n.Name != "front" || n.Id != id || n.Driver != "bridge" || len(n.IPAM.Config) != 1 || !strings.HasSuffix(n.IPAM.Config[0].Subnet, ".0.0/16") || len(n.Containers) != 1 {
		t.Errorf("inspect: %+v", n)
	}
	for _, c := range n.Containers {
		if c.Name != name || !strings.HasSuffix(c.IPv4Address, ".0.2/16") || !strings.HasPrefix(c.MacAddress, "02:42:") {
			t.Errorf("container in inspect: %+v", c)
		}
	}
	if r := s.cli("", "network", "rm", "front"); r.code != 1 || !strings.Contains(r.stderr, `network front has active endpoints (name:"web"`) {
		t.Errorf("rm with a running member: %+v", r)
	}
	if r := s.cli("y\n", "network", "prune"); !strings.Contains(r.stderr, "remove all custom networks not used by at least one container") || r.stdout != "" {
		t.Errorf("prune with a running member: %+v", r)
	}
	s.ok("stop", "-t", "0", name)
	if r := s.ok("network", "prune", "-f"); r.stdout != "Deleted Networks:\nfront\n\n" {
		t.Errorf("prune: %q", r.stdout)
	}
	// The stopped container keeps the network's name, and fails to start
	// without it, as with Docker.
	if r := s.cli("", "start", name); r.code == 0 || !strings.Contains(r.stderr, "network front not found") {
		t.Errorf("start without its network: %+v", r)
	}
}

func TestUserNetworkByName(t *testing.T) {
	net := newNetwork(t)
	server := serving(t, "--network", net, "--network-alias", "srv", "--hostname", "srvhost")
	waitServing(t, net, server)
	short := stateOf(t, server).ID[:12]
	ip := networksOf(t, server).Networks[net].IPAddress
	if !strings.HasSuffix(ip, ".0.2") {
		t.Errorf("first address %q", ip)
	}

	// By name, in any case, alias, short ID, hostname, NAME.NETWORK and
	// address; and ping, which only containers on a network answer.
	script := ""
	for _, n := range []string{server, strings.ToUpper(server), "srv", short, "srvhost", server + "." + net, ip} {
		script += "echo $(nc -w 2 " + n + " 5000 </dev/null); "
	}
	script += "ping -c 2 -W 2 srv >/dev/null && echo pinged; nslookup " + ip + " | grep -o 'name = .*'; grep -c . /etc/hosts; cat /etc/resolv.conf"
	r := run(t, "--network", net, alpine, "sh", "-c", script)
	want := strings.Repeat(server+"\n", 7) + "pinged\nname = " + server + "." + net + "\n7\nnameserver " +
		strings.TrimSuffix(ip, ".2") + ".1\noptions ndots:0\n"
	expect(t, r, want, 0)

	// 20 MB each way, and a UDP datagram.
	data := make([]byte, 20<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "data"), data, 0o644)
	sink := newName(t)
	detached(t, "--name", sink, "--network", net, alpine, "sh", "-c",
		"nc -lu -p 6001 > /tmp/udp & nc -lk -p 6002 -e sh -c 'cat /tmp/sum /tmp/udp' & nc -l -p 6000 | sha256sum > /tmp/sum; wait")
	r = run(t, "--network", net, "-v", dir+":/data:ro", alpine, "sh", "-c",
		"for i in $(seq 50); do nc -w 1 -z "+sink+" 6002 && break; sleep 0.2; done; nc -w 3 "+sink+" 6000 < /data/data; echo datagram | nc -u -w 1 "+sink+" 6001; "+
			"for i in $(seq 25); do out=$(nc -w 2 "+sink+" 6002 </dev/null 2>/dev/null); case \"$out\" in *datagram*) echo \"$out\"; exit 0;; esac; sleep 0.2; done; echo \"$out\"")
	expect(t, r, hex.EncodeToString(sum[:])+"  -\ndatagram\n", 0)
	r = run(t, "--network", net, alpine, "sh", "-c", "nc -w 5 "+sink+" 7000 </dev/null; echo $?")
	if !strings.HasSuffix(r.stdout, "1\n") {
		t.Errorf("a closed port: %+v", r)
	}
}

func TestUserNetworkIsolation(t *testing.T) {
	a, b := newNetwork(t), newNetwork(t)
	// Name them so that a sorts first: it is the primary network.
	if a > b {
		a, b = b, a
	}
	x := serving(t, "--network", a)
	y := serving(t, "--network", b)
	waitServing(t, a, x)
	waitServing(t, b, y)
	yIP := networksOf(t, y).Networks[b].IPAddress

	// On both: reaches both, its default route through a's gateway.
	r := run(t, "--network", b, "--network", a, alpine, "sh", "-c",
		"nc -w 2 "+x+" 5000 </dev/null; nc -w 2 "+y+" 5000 </dev/null; ip route | grep default; ls /sys/class/net")
	aGW := strings.TrimSuffix(networksOf(t, x).Networks[a].IPAddress, ".2") + ".1"
	expect(t, r, x+"\n"+y+"\n"+"default via "+aGW+" dev eth0 \neth0\neth1\nlo\n", 0)

	// On a alone: y's name and address are out of reach, the outside and
	// the host are not.
	port := hostServer(t, []byte("host\n"))
	r = run(t, "--network", a, "--add-host", hostGateway, alpine, "sh", "-c",
		"nslookup "+y+" >/dev/null 2>&1 || echo no name; ping -c 1 -W 1 "+yIP+" >/dev/null || echo no route; nc -w 2 "+yIP+" 5000 </dev/null || echo no connection; "+
			"wget -q -O- http://host.docker.internal:"+port+"/")
	expect(t, r, "no name\nno route\nno connection\nhost\n", 0)
}

func TestUserNetworkIP(t *testing.T) {
	net := newNetwork(t)
	subnet := strings.TrimSuffix(networkSubnet(t, net), "0.0/16")
	server := serving(t, "--network", net, "--ip", subnet+"7.7")
	waitServing(t, net, server)
	if ip := networksOf(t, server).Networks[net].IPAddress; ip != subnet+"7.7" {
		t.Errorf("address %q", ip)
	}
	r := run(t, "--network", net, "--ip", subnet+"7.7", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "already in use") {
		t.Errorf("the same --ip: %+v", r)
	}
	r = run(t, "--network", net, "--ip", "10.99.0.5", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "no configured subnet contains IP address 10.99.0.5") {
		t.Errorf("--ip outside the subnet: %+v", r)
	}
}

func networkSubnet(t *testing.T, net string) string {
	t.Helper()
	var nets []struct {
		IPAM struct{ Config []struct{ Subnet string } }
	}
	r := cli(t, "network", "inspect", net)
	if json.Unmarshal([]byte(r.stdout), &nets) != nil || len(nets) != 1 || len(nets[0].IPAM.Config) != 1 {
		t.Fatalf("inspect %s: %+v", net, r)
	}
	return nets[0].IPAM.Config[0].Subnet
}

func TestUserNetworkConnect(t *testing.T) {
	a, b := newNetwork(t), newNetwork(t)
	if a > b {
		a, b = b, a
	}
	// Created: connected, it boots again, and starts on both.
	c := newName(t)
	created(t, "--name", c, "--network", a, alpine, "sh", "-c", "ls /sys/class/net; nc -lk -p 5000 -e echo "+c)
	if r := cli(t, "network", "connect", "--alias", "cc", b, c); r.code != 0 {
		t.Fatalf("connect a created container: %+v", r)
	}
	if r := cli(t, "network", "connect", b, c); r.code != 1 || !strings.Contains(r.stderr, "already exists in network "+b) {
		t.Errorf("connect twice: %+v", r)
	}
	if r := cli(t, "start", c); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitServing(t, b, "cc")
	waitLog(t, c, "eth0\neth1\nlo\n")

	// Running: refused.
	if r := cli(t, "network", "disconnect", b, c); r.code != 1 || !strings.Contains(r.stderr, "cannot connect a running container to a network") {
		t.Errorf("disconnect a running container: %+v", r)
	}
	// Stopped: changed for the next start.
	cli(t, "stop", "-t", "0", c)
	if r := cli(t, "network", "disconnect", a, c); r.code != 0 {
		t.Fatalf("disconnect a stopped container: %+v", r)
	}
	if r := cli(t, "network", "disconnect", a, c); r.code != 1 || !strings.Contains(r.stderr, "is not connected to network "+a) {
		t.Errorf("disconnect twice: %+v", r)
	}
	if s := networksOf(t, c); s.NetworkMode != a || len(s.Networks) != 1 {
		t.Errorf("after disconnect: %+v", s)
	}
	// Off its last network, it has loopback alone.
	cli(t, "network", "disconnect", b, c)
	if r := cli(t, "start", c); r.code != 0 {
		t.Fatalf("start off every network: %+v", r)
	}
	waitLog(t, c, "eth0\neth1\nlo\nlo\n")
	none := newName(t)
	created(t, "--name", none, "--network", "none", alpine, "true")
	if r := cli(t, "network", "connect", a, none); r.code != 1 || !strings.Contains(r.stderr, "private (none) mode") {
		t.Errorf("connect --network none: %+v", r)
	}
}

func TestUserNetworkRestart(t *testing.T) {
	net := newNetwork(t)
	name := newName(t)
	cmd := exec.Command(runwhp(t), append([]string{"run", "-d", "--name", name, "--network", net, "--restart", "always"},
		withFlags([]string{alpine, "sh", "-c", bootCounter("timeout 2 nc -lk -p 5000 -e sh -c 'cat /boots'")})...)...)
	cmd.Env = append(os.Environ(), "RUNWHP_RESTART_BACKOFF=10ms")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run -d: %v %s", err, out)
	}
	waitState(t, name, time.Minute, "restarted", func(s stateJSON) bool { return s.RestartCount >= 2 })
	r := run(t, "--network", net, alpine, "sh", "-c",
		"for i in $(seq 50); do r=$(nc -w 1 "+name+" 5000 </dev/null) && [ -n \"$r\" ] && echo up && exit 0; sleep 0.2; done; exit 1")
	expect(t, r, "up\n", 0)
}

func TestUserNetworkPeerKilled(t *testing.T) {
	net := newNetwork(t)
	victim := serving(t, "--network", net)
	waitServing(t, net, victim)
	ip := networksOf(t, victim).Networks[net].IPAddress
	if err := exec.Command("taskkill", "/F", "/PID", fmt.Sprint(stateOf(t, victim).Pid)).Run(); err != nil {
		t.Fatal(err)
	}
	// Its address is free at once, and goes to the next container.
	next := serving(t, "--network", net)
	waitServing(t, net, next)
	if got := networksOf(t, next).Networks[net].IPAddress; got != ip {
		t.Errorf("the next container got %s, not the killed one's %s", got, ip)
	}
	start := time.Now()
	r := run(t, "--network", net, alpine, "sh", "-c", "nc -w 2 "+ip+" 5000 </dev/null")
	if r.stdout != next+"\n" || time.Since(start) > 10*time.Second {
		t.Errorf("the address now: %+v in %v", r, time.Since(start))
	}
}

func TestUserNetworkPublish(t *testing.T) {
	a, b := newNetwork(t), newNetwork(t)
	port := freePort(t)
	name := serving(t, "--network", a, "--network", b, "-p", "127.0.0.1:"+port+":5000")
	waitServing(t, a, name)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out, err := exec.Command("curl", "-s", "--max-time", "2", "telnet://127.0.0.1:"+port).Output()
		if err == nil || strings.TrimSpace(string(out)) == name {
			if strings.TrimSpace(string(out)) != name {
				t.Errorf("published port answered %q", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published port never answered: %v", err)
		}
	}
}

func TestUserNetworkThroughput(t *testing.T) {
	net := newNetwork(t)
	sink := newName(t)
	detached(t, "--name", sink, "--network", net, alpine, "sh", "-c", "while true; do nc -l -p 6000 >/dev/null; done")
	r := run(t, "--network", net, alpine, "sh", "-c",
		"for i in $(seq 50); do nc -w 1 -z "+sink+" 6000 && break; sleep 0.2; done; "+
			"s=$(cut -d' ' -f1 /proc/uptime); dd if=/dev/zero bs=1M count=200 2>/dev/null | nc -w 3 "+sink+" 6000; e=$(cut -d' ' -f1 /proc/uptime); echo $s $e")
	var s, e float64
	if _, err := fmt.Sscan(r.stdout, &s, &e); err != nil || r.code != 0 {
		t.Fatalf("%+v", r)
	}
	t.Logf("200 MB to another container in %.2f s", e-s)
	if e-s > 10 {
		t.Errorf("200 MB took %.2f s", e-s)
	}
}
