//go:build windows

package network

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

func testRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	os.MkdirAll(base, 0o755)
	dir, err := os.MkdirTemp(base, "network")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testStore(t *testing.T) *Store {
	s := Open(testRoot(t))
	s.HostPrefixes = func() []netip.Prefix { return nil }
	return s
}

func TestCreate(t *testing.T) {
	s := testStore(t)
	a, err := s.Create("front")
	if err != nil {
		t.Fatal(err)
	}
	if a.Subnet.String() != "172.18.0.0/16" || a.Gateway.String() != "172.18.0.1" || a.Driver != "bridge" || len(a.ID) != 64 {
		t.Errorf("first network: %+v", a)
	}
	s.HostPrefixes = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.19.4.0/24")} }
	b, err := s.Create("back")
	if err != nil || b.Subnet.String() != "172.20.0.0/16" {
		t.Errorf("second, past the host's 172.19: %+v %v", b, err)
	}
	for name, want := range map[string]string{
		"front": "network with name front already exists",
		"FRONT": "network with name front already exists",
		"host":  "operation is not permitted on predefined host network",
		"x":     "invalid network name",
		"a b":   "invalid network name",
	} {
		if _, err := s.Create(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", name, err, want)
		}
	}

	for ref, want := range map[string]*Network{"front": a, "Front": a, a.ID: a, a.ID[:5]: a, b.ID[:12]: b} {
		if n, err := s.Resolve(ref); err != nil || n.ID != want.ID {
			t.Errorf("Resolve(%q) = %+v, %v", ref, n, err)
		}
	}
	if n, err := s.Resolve("bridge"); err != nil || n.Name != "user" {
		t.Errorf("bridge: %+v %v", n, err)
	}
	if _, err := s.Resolve("nosuch"); !errors.Is(err, ErrNotFound) || err.Error() != "network nosuch not found" {
		t.Errorf("nosuch: %v", err)
	}
	list, err := s.List()
	var names []string
	for _, n := range list {
		names = append(names, n.Name)
	}
	if err != nil || strings.Join(names, " ") != "back front none user" {
		t.Errorf("List: %q %v", names, err)
	}
	if got := s.Subnets(); len(got) != 2 {
		t.Errorf("Subnets: %v", got)
	}

	if _, err := s.Remove("user"); !errors.Is(err, ErrPredefined) {
		t.Errorf("removing user: %v", err)
	}
	if n, err := s.Remove("front"); err != nil || n.ID != a.ID {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Resolve("front"); !errors.Is(err, ErrNotFound) {
		t.Errorf("front after removal: %v", err)
	}
	// Its name and its subnet are free again.
	if c, err := s.Create("front"); err != nil || c.Subnet != a.Subnet || c.ID == a.ID {
		t.Errorf("made again: %+v %v", c, err)
	}
}

func join(t *testing.T, s *Store, n *Network, cid string, ip string) *Lease {
	t.Helper()
	r := Request{Network: n.ID, Name: n.Name}
	if ip != "" {
		r.IP = netip.MustParseAddr(ip)
	}
	ls, err := s.Join(cid, "", `\\.\pipe\x`, []Request{r})
	if err != nil {
		t.Fatal(err)
	}
	return ls[0]
}

func TestLeases(t *testing.T) {
	s := testStore(t)
	n, err := s.Create("net")
	if err != nil {
		t.Fatal(err)
	}
	a, b := container.NewID(), container.NewID()
	la := join(t, s, n, a, "")
	lb := join(t, s, n, b, "")
	if la.IP.String() != "172.18.0.2" || lb.IP.String() != "172.18.0.3" || la.MAC() != [6]byte{2, 0x42, 172, 18, 0, 2} {
		t.Errorf("leases %v %v", la.IP, lb.IP)
	}
	eps, err := s.Endpoints(n.ID)
	if err != nil || len(eps) != 2 {
		t.Fatalf("endpoints %+v %v", eps, err)
	}
	if _, err := s.Join(a, "", "p", []Request{{Network: n.ID, Name: "net"}}); err == nil {
		t.Error("a container joined twice")
	}
	if _, err := s.Join(container.NewID(), "", "p", []Request{{Network: n.ID, Name: "net", IP: lb.IP}}); !errors.Is(err, ErrInUse) {
		t.Errorf("taken address: %v", err)
	}
	if _, err := s.Join(container.NewID(), "", "p", []Request{{Network: n.ID, Name: "net", IP: netip.MustParseAddr("10.1.1.1")}}); err == nil ||
		!strings.Contains(err.Error(), "invalid config for network net: invalid endpoint settings:\nno configured subnet contains IP address 10.1.1.1") {
		t.Errorf("address elsewhere: %v", err)
	}
	if _, err := s.Join(container.NewID(), "", "p", []Request{{Network: container.NewID(), Name: "gone"}}); err == nil || err.Error() != "network gone not found" {
		t.Errorf("missing network: %v", err)
	}
	// rm refuses a network with live endpoints, naming them.
	if _, err := s.Remove("net"); !errors.Is(err, ErrActive) || !strings.Contains(err.Error(), "error while removing network: network net has active endpoints (name:") {
		t.Errorf("Remove with endpoints: %v", err)
	}
	if ok, err := s.RemoveIfUnused(n); ok || err != nil {
		t.Errorf("RemoveIfUnused with endpoints: %v %v", ok, err)
	}
	la.Release()
	// The lowest free address goes to the next.
	if lc := join(t, s, n, container.NewID(), ""); lc.IP.String() != "172.18.0.2" {
		t.Errorf("after release: %v", lc.IP)
	} else {
		lc.Release()
	}
	if ep, ok := s.EndpointOf(n.ID, b); !ok || ep.IP != lb.IP {
		t.Errorf("EndpointOf: %+v %v", ep, ok)
	}
	lb.Release()
	if ok, err := s.RemoveIfUnused(n); !ok || err != nil {
		t.Errorf("RemoveIfUnused without: %v %v", ok, err)
	}

	// All or none: a second request that fails gives the first back.
	m, _ := s.Create("mm")
	o, _ := s.Create("oo")
	_, err = s.Join(a, "", "p", []Request{{Network: m.ID, Name: "mm"}, {Network: o.ID, Name: "oo", IP: netip.MustParseAddr("1.2.3.4")}})
	if err == nil {
		t.Fatal("a bad second request joined")
	}
	if eps, _ := s.Endpoints(m.ID); len(eps) != 0 {
		t.Errorf("first lease kept: %+v", eps)
	}
}

func TestLeasesAtOnce(t *testing.T) {
	s := testStore(t)
	n, err := s.Create("busy")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	ips := map[netip.Addr]bool{}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ls, err := s.Join(container.NewID(), "", "p", []Request{{Network: n.ID, Name: n.Name}})
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(ls[0].Release)
			mu.Lock()
			if ips[ls[0].IP] {
				t.Errorf("%v leased twice", ls[0].IP)
			}
			ips[ls[0].IP] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ips) != 32 || !ips[netip.MustParseAddr("172.18.0.33")] {
		t.Errorf("%d addresses", len(ips))
	}
}

// holdLease, in a child, leases an address and prints it, then holds it
// until it is killed or, with exit, exits at once without releasing it.
func holdLease() bool {
	v := os.Getenv("RUNWHP_NETWORK_HOLD")
	if v == "" {
		return false
	}
	parts := strings.Split(v, "|")
	s := Open(parts[0])
	s.HostPrefixes = func() []netip.Prefix { return nil }
	ls, err := s.Join(parts[2], "", "p", []Request{{Network: parts[1], Name: "n"}})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println("leased", ls[0].IP)
	if len(parts) > 3 && parts[3] == "exit" {
		os.Exit(0) // without Release
	}
	time.Sleep(time.Minute)
	os.Exit(0)
	return true
}

func startLeaseHolder(t *testing.T, s *Store, n *Network, cid string, exit bool) (*exec.Cmd, string) {
	t.Helper()
	v := s.root + "|" + n.ID + "|" + cid
	if exit {
		v += "|exit"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeasesAcrossProcesses$")
	cmd.Env = append(os.Environ(), "RUNWHP_NETWORK_HOLD="+v)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if !strings.HasPrefix(line, "leased ") {
		cmd.Process.Kill()
		t.Fatalf("child said %q", line)
	}
	return cmd, strings.TrimSpace(strings.TrimPrefix(line, "leased "))
}

func TestLeasesAcrossProcesses(t *testing.T) {
	if holdLease() {
		return
	}
	s := testStore(t)
	n, err := s.Create("shared")
	if err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	defer func() {
		for _, c := range cmds {
			c.Process.Kill()
			c.Wait()
		}
	}()
	var got []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd, ip := startLeaseHolder(t, s, n, container.NewID(), false)
			mu.Lock()
			cmds = append(cmds, cmd)
			got = append(got, ip)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(got)
	if strings.Join(got, " ") != "172.18.0.2 172.18.0.3 172.18.0.4 172.18.0.5" {
		t.Errorf("leased %q", got)
	}
	// Live in this process's eyes.
	if eps, _ := s.Endpoints(n.ID); len(eps) != 4 {
		t.Errorf("%d live endpoints", len(eps))
	}
	// A killed holder's lease is dead at once, and its address goes to the
	// next to ask.
	victim := cmds[0]
	victim.Process.Kill()
	victim.Wait()
	cmds = cmds[1:]
	waitUntil(t, "the endpoint to die", func() bool { eps, _ := s.Endpoints(n.ID); return len(eps) == 3 })
	l := join(t, s, n, container.NewID(), "")
	defer l.Release()
	free := map[string]bool{"172.18.0.2": true, "172.18.0.3": true, "172.18.0.4": true, "172.18.0.5": true}
	if !free[l.IP.String()] {
		t.Errorf("the dead holder's address was not reclaimed: got %v", l.IP)
	}

	// One that exits without releasing leaves a file, which is not live.
	cid := container.NewID()
	cmd, _ := startLeaseHolder(t, s, n, cid, true)
	cmd.Wait()
	if _, err := os.Stat(filepath.Join(s.endpointsDir(n.ID), cid+".json")); err != nil {
		t.Fatalf("no file left: %v", err)
	}
	if _, ok := s.EndpointOf(n.ID, cid); ok {
		t.Error("an exited holder's endpoint is live")
	}
	// The same container boots again: its old file is reclaimed.
	l2 := join(t, s, n, cid, "")
	l2.Release()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDirectory(t *testing.T) {
	s := testStore(t)
	n, err := s.Create("dir")
	if err != nil {
		t.Fatal(err)
	}
	cs := container.Open(s.root)
	self, peer := container.NewID(), container.NewID()
	// The peer has a name in the container store.
	cfg := &container.Config{ID: peer}
	if _, err := cs.Create(cfg, "Web"); err != nil {
		t.Fatal(err)
	}
	ls, err := s.Join(self, "", "p", []Request{{Network: n.ID, Name: n.Name}})
	if err != nil {
		t.Fatal(err)
	}
	defer ls[0].Release()

	clock := time.Unix(1000, 0)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	d := s.Directory(n, self)
	d.now = now
	defer d.Close()

	pl, err := s.Join(peer, "webhost", "p", []Request{{Network: n.ID, Name: n.Name, Aliases: []string{"api"}}})
	if err != nil {
		t.Fatal(err)
	}
	ip := pl[0].IP

	// Not known yet: a miss starts a refresh, and the next lookup finds it.
	if _, ok := d.Lookup(ip); ok {
		t.Error("found before any refresh")
	}
	waitUntil(t, "the refresh", func() bool { p, ok := d.Lookup(ip); return ok && p.Container == peer })
	if ps := d.Peers(); len(ps) != 1 || ps[0].IP != ip {
		t.Errorf("Peers %+v", ps)
	}
	if _, ok := d.Lookup(ls[0].IP); ok {
		t.Error("Lookup found the container itself")
	}
	for _, name := range []string{"web", "WEB", "api", "webhost", peer[:12], "web.dir", "api.dir"} {
		if got := d.Resolve(name); len(got) != 1 || got[0] != ip {
			t.Errorf("Resolve(%q) = %v", name, got)
		}
	}
	if got := d.Resolve(self[:12]); len(got) != 1 || got[0] != ls[0].IP {
		t.Errorf("own short ID: %v", got)
	}
	if name, ok := d.NameOf(ip); !ok || name != "Web.dir" {
		t.Errorf("NameOf %q %v", name, ok)
	}

	// The peer goes: lookups go on finding it until the snapshot is stale,
	// then a refresh drops it.
	pl[0].Release()
	if _, ok := d.Lookup(ip); !ok {
		t.Error("dropped before the snapshot went stale")
	}
	advance(3 * time.Second)
	d.Lookup(ip) // starts the refresh
	waitUntil(t, "the peer to go", func() bool { _, ok := d.Lookup(ip); return !ok })

	// DNS looks again at once for a name it does not know.
	pl2, _ := s.Join(peer, "", "p", []Request{{Network: n.ID, Name: n.Name}})
	defer pl2[0].Release()
	advance(time.Second)
	if got := d.Resolve("web"); len(got) != 1 {
		t.Errorf("DNS after a restart: %v", got)
	}
}
