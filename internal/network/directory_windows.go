//go:build windows

package network

import (
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netswitch"
)

// How a Directory keeps up: a snapshot older than staleAfter is read again
// in the background when used, and one that misses an address is read
// again in the background at most every missInterval. DNS, which may wait,
// reads it again at once on a miss once it is older than dnsFresh.
const (
	staleAfter   = 2 * time.Second
	missInterval = 50 * time.Millisecond
	dnsFresh     = 250 * time.Millisecond
)

// Directory is what one container knows of the live guests on one network:
// a snapshot of their endpoints, and their names, read from the store and
// read again as it goes stale. It is a netswitch.Directory, which never
// waits for the disk, and a netswitch.NameDirectory.
type Directory struct {
	store *Store
	names *container.Store
	net   *Network
	self  string // the container's ID
	now   func() time.Time

	snap       atomic.Pointer[snapshot]
	refreshing atomic.Bool
	lastMiss   atomic.Int64 // unix nanoseconds
	mu         sync.Mutex   // serializes reads of the store
	wg         sync.WaitGroup
}

type snapshot struct {
	at     time.Time
	byIP   map[netip.Addr]Endpoint // every live endpoint, this one's too
	byName map[string][]netip.Addr // lower-case names
	nameOf map[netip.Addr]string
	peers  []netswitch.Peer // every one but this
}

// Directory is the directory of network n, for container cid.
func (s *Store) Directory(n *Network, cid string) *Directory {
	d := &Directory{store: s, names: container.Open(s.root), net: n, self: cid, now: time.Now}
	d.snap.Store(&snapshot{})
	return d
}

// Refresh reads the network's endpoints again, now.
func (d *Directory) Refresh() {
	d.mu.Lock()
	defer d.mu.Unlock()
	eps, err := d.store.Endpoints(d.net.ID)
	if err != nil {
		return
	}
	sn := &snapshot{at: d.now(), byIP: map[netip.Addr]Endpoint{}, byName: map[string][]netip.Addr{}, nameOf: map[netip.Addr]string{}}
	for _, e := range eps {
		sn.byIP[e.IP] = e
		if e.Container != d.self {
			sn.peers = append(sn.peers, e.Peer())
		}
		name := d.names.Name(e.Container)
		var keys []string
		if name != "" {
			keys = append(keys, name)
			sn.nameOf[e.IP] = name + "." + d.net.Name
		} else {
			sn.nameOf[e.IP] = e.Container[:12] + "." + d.net.Name
		}
		keys = append(keys, e.Aliases...)
		keys = append(keys, e.Container[:12])
		if e.Hostname != "" {
			keys = append(keys, e.Hostname)
		}
		seen := map[string]bool{}
		for _, k := range keys {
			for _, k := range []string{strings.ToLower(k), strings.ToLower(k + "." + d.net.Name)} {
				if !seen[k] {
					seen[k] = true
					sn.byName[k] = append(sn.byName[k], e.IP)
				}
			}
		}
	}
	d.snap.Store(sn)
}

// refreshSoon reads the endpoints again in the background, unless that is
// under way.
func (d *Directory) refreshSoon() {
	if !d.refreshing.CompareAndSwap(false, true) {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.refreshing.Store(false)
		d.Refresh()
	}()
}

// current is the snapshot, with a refresh started if it is stale.
func (d *Directory) current() *snapshot {
	sn := d.snap.Load()
	if d.now().Sub(sn.at) > staleAfter {
		d.refreshSoon()
	}
	return sn
}

// missed starts a refresh for a lookup that found nothing, unless one did
// lately.
func (d *Directory) missed() {
	now := d.now().UnixNano()
	last := d.lastMiss.Load()
	if now-last < int64(missInterval) || !d.lastMiss.CompareAndSwap(last, now) {
		return
	}
	d.refreshSoon()
}

// Lookup implements netswitch.Directory.
func (d *Directory) Lookup(ip netip.Addr) (netswitch.Peer, bool) {
	e, ok := d.current().byIP[ip]
	if !ok || e.Container == d.self {
		d.missed()
		return netswitch.Peer{}, false
	}
	return e.Peer(), true
}

// Peers implements netswitch.Directory.
func (d *Directory) Peers() []netswitch.Peer { return d.current().peers }

// Failed is told of a peer a link to which failed: it may have gone.
func (d *Directory) Failed(netswitch.Peer) { d.missed() }

// Resolve implements netswitch.NameDirectory.
func (d *Directory) Resolve(name string) []netip.Addr {
	key := strings.ToLower(name)
	sn := d.current()
	if ips := sn.byName[key]; len(ips) > 0 {
		return ips
	}
	if d.now().Sub(sn.at) > dnsFresh {
		d.Refresh()
		return d.snap.Load().byName[key]
	}
	return nil
}

// NameOf implements netswitch.NameDirectory.
func (d *Directory) NameOf(ip netip.Addr) (string, bool) {
	name, ok := d.current().nameOf[ip]
	return name, ok
}

// Endpoint is the live endpoint of container cid, as it was last read.
func (d *Directory) Endpoint(cid string) (Endpoint, bool) {
	for _, e := range d.current().byIP {
		if e.Container == cid {
			return e, true
		}
	}
	return Endpoint{}, false
}

// Close waits for a refresh under way.
func (d *Directory) Close() { d.wg.Wait() }
