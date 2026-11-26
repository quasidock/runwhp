//go:build windows

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/storefile"
)

// Request asks for a lease on one network for a container booting.
type Request struct {
	// Network is the network's ID, and Name what the container's
	// configuration calls it, for errors.
	Network, Name string
	// IP is the address asked for; the zero Addr means any.
	IP      netip.Addr
	Aliases []string
}

// Lease is a container's endpoint on a network, held while its VM runs.
type Lease struct {
	Endpoint
	Net *Network

	h    windows.Handle
	path string
}

// Join leases an address on each network reqs ask for, for the container,
// whose runwhp takes links at pipe: all of them, or none. The leases last
// until Release, or until the process ends.
func (s *Store) Join(cid, hostname, pipe string, reqs []Request) ([]*Lease, error) {
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	var leases []*Lease
	for _, r := range reqs {
		l, err := s.join(cid, hostname, pipe, r)
		if err != nil {
			for _, l := range leases {
				l.Release()
			}
			return nil, err
		}
		leases = append(leases, l)
	}
	return leases, nil
}

func (s *Store) join(cid, hostname, pipe string, r Request) (*Lease, error) {
	n, err := s.Get(r.Network)
	if err != nil {
		return nil, NotFound(r.Name)
	}
	dir := s.endpointsDir(n.ID)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	used := map[netip.Addr]bool{}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// An endpoint whose runwhp has let go of it is reclaimed; a live
		// one cannot be removed.
		if os.Remove(p) == nil {
			continue
		}
		if strings.TrimSuffix(e.Name(), ".json") == cid {
			return nil, fmt.Errorf("container %.12s is already on network %s", cid, n.Name)
		}
		if ep, err := readEndpoint(p); err == nil {
			used[ep.IP] = true
		}
	}
	ip, err := pickAddress(n, r.IP, used)
	if err != nil {
		if r.IP.IsValid() && !errors.Is(err, ErrInUse) {
			return nil, fmt.Errorf("invalid config for network %s: %w", n.Name, err)
		}
		return nil, err
	}
	ep := Endpoint{Network: n.ID, Container: cid, ID: container.NewID(), IP: ip, Hostname: hostname, Aliases: r.Aliases, Pipe: pipe}
	path := filepath.Join(dir, cid+".json")
	h, err := createHeld(path)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(ep)
	var done uint32
	if err := windows.WriteFile(h, data, &done, nil); err != nil {
		windows.CloseHandle(h)
		os.Remove(path)
		return nil, &os.PathError{Op: "write", Path: path, Err: err}
	}
	return &Lease{Endpoint: ep, Net: n, h: h, path: path}, nil
}

// createHeld makes a new file and keeps it open, sharing it for reading
// only: no one can delete it, nor open it to, while the handle is open.
// The container's last endpoint file, just removed, may linger while a
// reader has it open, where deletes do not have POSIX semantics: it tries
// again for a while.
func createHeld(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	for try := 0; ; try++ {
		h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return h, nil
		}
		pending := errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_DELETE_PENDING) || errors.Is(err, windows.ERROR_FILE_EXISTS)
		if !pending || try == 50 {
			return 0, &os.PathError{Op: "create", Path: path, Err: err}
		}
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			os.Remove(path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Release gives the lease up.
func (l *Lease) Release() {
	if l.h == 0 {
		return
	}
	windows.CloseHandle(l.h)
	l.h = 0
	os.Remove(l.path)
}

// live reports whether the runwhp that made an endpoint file still holds
// it: it cannot be opened for deleting while it does.
func live(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err == nil {
		windows.CloseHandle(h)
		return false
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func readEndpoint(path string) (Endpoint, error) {
	var ep Endpoint
	data, err := storefile.Read(path)
	if err != nil {
		return ep, err
	}
	err = json.Unmarshal(data, &ep)
	return ep, err
}

// Endpoints are the live endpoints on the network with the ID id.
func (s *Store) Endpoints(id string) ([]Endpoint, error) {
	dir := s.endpointsDir(id)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var eps []Endpoint
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") || !live(p) {
			continue
		}
		// One just made may not be written yet; it is skipped until it is.
		if ep, err := readEndpoint(p); err == nil && ep.IP.IsValid() {
			eps = append(eps, ep)
		}
	}
	return eps, nil
}

// EndpointOf is the live endpoint of container cid on the network with the
// ID id, if it has one.
func (s *Store) EndpointOf(id, cid string) (Endpoint, bool) {
	p := filepath.Join(s.endpointsDir(id), cid+".json")
	if !live(p) {
		return Endpoint{}, false
	}
	ep, err := readEndpoint(p)
	return ep, err == nil && ep.IP.IsValid()
}
