//go:build windows

package network

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/storefile"
	"github.com/quasidock/runwhp/internal/volume"
)

// Store is the networks in a runwhp store.
type Store struct {
	root string
	// HostPrefixes are the subnets the host's interfaces are on, which
	// no network may overlap; nil means the host's own. Tests replace it.
	HostPrefixes func() []netip.Prefix
}

func Open(root string) *Store { return &Store{root: root} }

func (s *Store) dir() string             { return filepath.Join(s.root, "networks") }
func (s *Store) netDir(id string) string { return filepath.Join(s.dir(), id) }
func (s *Store) nameFile(name string) string {
	return filepath.Join(s.dir(), "names", strings.ToLower(name))
}
func (s *Store) endpointsDir(id string) string { return filepath.Join(s.netDir(id), "endpoints") }

// CheckName checks a network name, as container names are checked.
func CheckName(name string) error {
	if err := volume.CheckName(name); err != nil {
		return fmt.Errorf("invalid network name %q: use two or more letters, digits, _ . or -, starting with a letter or digit", name)
	}
	return nil
}

// lock takes the networks' lock, which every change to the networks or
// their endpoints is made under. Windows lets go of it when the process
// ends, however it ends.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir(), "lock")
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "lock", Path: path, Err: err}
	}
	return func() {
		windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		windows.CloseHandle(h)
	}, nil
}

// Create makes a network called name, with the first free subnet.
func (s *Store) Create(name string) (*Network, error) {
	return s.CreateWith(name, nil)
}

// CreateWith is Create, the network labelled with labels.
func (s *Store) CreateWith(name string, labels map[string]string) (*Network, error) {
	if err := predefinedError(name); err != nil {
		return nil, err
	}
	if err := CheckName(name); err != nil {
		return nil, err
	}
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()

	if n, err := s.byName(name); err == nil {
		return nil, fmt.Errorf("network with name %s %w", n.Name, ErrExists)
	}
	nets, err := s.records()
	if err != nil {
		return nil, err
	}
	taken := []netip.Prefix{netstack.Subnet}
	for _, n := range nets {
		taken = append(taken, n.Subnet)
	}
	hosts := s.HostPrefixes
	if hosts == nil {
		hosts = hostPrefixes
	}
	taken = append(taken, hosts()...)
	subnet, err := pickSubnet(taken)
	if err != nil {
		return nil, err
	}
	n := &Network{ID: container.NewID(), Name: name, Created: time.Now(), Driver: "bridge", Subnet: subnet, Gateway: subnet.Addr().Next(), Labels: labels}

	// Made aside and moved into place whole, then named.
	tmp, err := os.MkdirTemp(s.dir(), "tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	data, _ := json.MarshalIndent(n, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "network.json"), data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(tmp, "endpoints"), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, s.netDir(n.ID)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.nameFile(name)), 0o755); err != nil {
		return nil, err
	}
	err = storefile.Create(s.nameFile(name), []byte(n.ID))
	if errors.Is(err, fs.ErrExist) {
		// Left by a network that is gone: the lock is held, so no one
		// else is naming one.
		os.Remove(s.nameFile(name))
		err = storefile.Create(s.nameFile(name), []byte(n.ID))
	}
	if err != nil {
		os.RemoveAll(s.netDir(n.ID))
		return nil, err
	}
	return n, nil
}

// hostPrefixes are the subnets of the host's interfaces, less loopback
// and link-local ones.
func hostPrefixes() []netip.Prefix {
	var ps []netip.Prefix
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok || !ip.Unmap().Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			bits, _ := ipn.Mask.Size()
			ps = append(ps, netip.PrefixFrom(ip.Unmap(), bits).Masked())
		}
	}
	return ps
}

// Get is the network with the full ID id.
func (s *Store) Get(id string) (*Network, error) {
	for _, b := range []string{User, None} {
		if n := Builtin(b); n.ID == id {
			return n, nil
		}
	}
	if len(id) != 64 || strings.ContainsAny(id, `\/.:`) {
		return nil, NotFound(id)
	}
	data, err := storefile.Read(filepath.Join(s.netDir(id), "network.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, NotFound(id)
	}
	if err != nil {
		return nil, err
	}
	n := &Network{}
	if err := json.Unmarshal(data, n); err != nil {
		return nil, fmt.Errorf("network %.12s: bad network.json: %w", id, err)
	}
	return n, nil
}

func (s *Store) byName(name string) (*Network, error) {
	if CheckName(name) != nil {
		return nil, NotFound(name)
	}
	id, err := storefile.Read(s.nameFile(name))
	if err != nil {
		return nil, NotFound(name)
	}
	n, err := s.Get(string(id))
	if err != nil || !strings.EqualFold(n.Name, name) {
		return nil, NotFound(name)
	}
	return n, nil
}

// Resolve finds the network ref names: a built-in one by name, else one by
// its full ID, its name, or a unique prefix of its ID.
func (s *Store) Resolve(ref string) (*Network, error) {
	if n := Builtin(ref); n != nil {
		return n, nil
	}
	if n, err := s.Get(ref); err == nil {
		return n, nil
	}
	if n, err := s.byName(ref); err == nil {
		return n, nil
	}
	prefix := strings.ToLower(ref)
	if prefix == "" || strings.Trim(prefix, "0123456789abcdef") != "" {
		return nil, NotFound(ref)
	}
	nets, err := s.List()
	if err != nil {
		return nil, err
	}
	var found []*Network
	for _, n := range nets {
		if strings.HasPrefix(n.ID, prefix) {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 0:
		return nil, NotFound(ref)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("network %s is ambiguous (%d matches found based on ID prefix)", ref, len(found))
}

// records are the user's networks, unsorted.
func (s *Store) records() ([]*Network, error) {
	entries, err := os.ReadDir(s.dir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nets []*Network
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 64 {
			continue
		}
		if n, err := s.Get(e.Name()); err == nil {
			nets = append(nets, n)
		}
	}
	return nets, nil
}

// List is every network, the built-in ones included, by name.
func (s *Store) List() ([]*Network, error) {
	nets, err := s.records()
	if err != nil {
		return nil, err
	}
	nets = append(nets, Builtin(User), Builtin(None))
	SortByName(nets)
	return nets, nil
}

// Remove removes a network no container is running on. A container that
// is not running keeps its network's name in its configuration, and fails
// to start without it, as with Docker.
func (s *Store) Remove(ref string) (*Network, error) {
	n, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if n.IsBuiltin() {
		return nil, predefinedError(n.Name)
	}
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	return n, s.removeLocked(n)
}

// RemoveIfUnused removes a network if no container is running on it, and
// reports whether it did, for prunes, which check again under the lock.
func (s *Store) RemoveIfUnused(n *Network) (bool, error) {
	release, err := s.lock()
	if err != nil {
		return false, err
	}
	defer release()
	err = s.removeLocked(n)
	if errors.Is(err, ErrActive) || errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) removeLocked(n *Network) error {
	if _, err := s.Get(n.ID); err != nil {
		return err
	}
	live, err := s.Endpoints(n.ID)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		cs := container.Open(s.root)
		var desc []string
		for _, e := range live {
			desc = append(desc, fmt.Sprintf("name:%q id:%q", cs.Name(e.Container), e.ID[:12]))
		}
		return fmt.Errorf("error while removing network: network %s %w (%s)", n.Name, ErrActive, strings.Join(desc, ", "))
	}
	var b [6]byte
	rand.Read(b[:])
	aside := filepath.Join(s.dir(), "removed-"+hex.EncodeToString(b[:]))
	if err := os.Rename(s.netDir(n.ID), aside); err != nil {
		return err
	}
	if id, err := storefile.Read(s.nameFile(n.Name)); err == nil && string(id) == n.ID {
		os.Remove(s.nameFile(n.Name))
	}
	return os.RemoveAll(aside)
}

// Subnets are the subnets of every user-defined network, which a guest's
// stack must not carry to the host.
func (s *Store) Subnets() []netip.Prefix {
	nets, _ := s.records()
	var ps []netip.Prefix
	for _, n := range nets {
		ps = append(ps, n.Subnet)
	}
	return ps
}
