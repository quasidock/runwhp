//go:build windows

// Package volume keeps runwhp's named volumes: each one a directory in the
// store's volumes directory holding a sparse ext4 disk image, disk.ext4, and
// a record of it, volume.json.
//
// A volume's disk is attached to a container alone as a block device, its
// runwhp opening the disk exclusively, so that a second one gets
// disk.ErrInUse. A volume that several containers use at once is shared:
// its disk is attached to a guest of its own, its server, in a runwhp of
// its own, which serves the file system to every container using it. Once
// shared, a volume stays shared, as volume.json records.
package volume

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
	"github.com/quasidock/runwhp/internal/storefile"
)

var (
	ErrNotFound = errors.New("no such volume")
	ErrExists   = errors.New("volume already exists")
	ErrInUse    = errors.New("volume is in use by a running container")
)

// InUseError says a container cannot have a volume another has: one
// holding the volume's disk, as a volume not shared is held.
type InUseError struct {
	Name string
	// Holder names the container holding it, if known.
	Holder string
	// Hint, if set, says what to do about it.
	Hint string
}

func (e *InUseError) Error() string {
	msg := "volume " + e.Name + " is in use by another container"
	if e.Holder != "" {
		msg = "volume " + e.Name + " is in use by container " + e.Holder
	}
	if e.Hint != "" {
		msg += ": " + e.Hint
	}
	return msg
}

func (e *InUseError) Unwrap() error { return ErrInUse }

// Volume is one named volume.
type Volume struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"` // the file system's size; the disk takes only what is written
	// Labels are volume create --label's, as Docker keeps them.
	Labels map[string]string `json:"labels,omitempty"`
	// Shared says the volume's server serves it to every container using
	// it, rather than one container's runwhp having its disk.
	Shared bool `json:"shared,omitempty"`

	dir string
}

// Options are what a volume is made with.
type Options struct {
	Size   int64
	Labels map[string]string
	Shared bool
}

// Disk is the path of the volume's disk image.
func (v *Volume) Disk() string { return filepath.Join(v.dir, "disk.ext4") }

// OnDisk returns how many bytes of the host's disk the volume takes.
func (v *Volume) OnDisk() (int64, error) { return disk.Allocated(v.Disk()) }

// InUse reports whether a running container is using the volume: one
// holding its disk, or the volume's server serving any.
func (v *Volume) InUse() (bool, error) {
	busy, err := disk.InUse(v.Disk())
	if err != nil || !busy {
		return busy, err
	}
	st, err := v.ServerStatus()
	if err != nil {
		return true, nil // a container's runwhp has the disk
	}
	return st.Clients > 0, nil
}

// Store is the volumes in one runwhp store.
type Store struct {
	root string // the store's root
}

func Open(root string) *Store { return &Store{root: root} }

func (s *Store) dir(name string) string { return filepath.Join(s.root, "volumes", name) }

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// reservedNames are device names Windows will not have as file names, with
// or without an extension.
var reservedNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for i := 1; i <= 9; i++ {
		reservedNames[fmt.Sprintf("com%d", i)] = true
		reservedNames[fmt.Sprintf("lpt%d", i)] = true
	}
}

// CheckName checks a volume name: Docker's rules, two or more letters,
// digits, underscores, dots and dashes starting with a letter or digit, and
// what Windows needs of a directory name.
func CheckName(name string) error {
	switch {
	case !validName.MatchString(name):
		return fmt.Errorf("invalid volume name %q: use two or more letters, digits, _ . or -, starting with a letter or digit", name)
	case len(name) > 128:
		return fmt.Errorf("invalid volume name %q: at most 128 characters", name)
	case strings.HasSuffix(name, "."):
		return fmt.Errorf("invalid volume name %q: Windows drops a trailing dot", name)
	}
	base, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToLower(base)] {
		return fmt.Errorf("invalid volume name %q: Windows reserves %s for a device", name, base)
	}
	return nil
}

// Get returns the volume called name. Names are case-insensitive, as the
// host's file names are; the volume keeps the case it was created with.
func (s *Store) Get(name string) (*Volume, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	dir := s.dir(name)
	data, err := storefile.Read(filepath.Join(dir, "volume.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	v := &Volume{dir: dir}
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("volume %s: %w", name, err)
	}
	return v, nil
}

// SetShared marks the volume called name shared, from now on, and reports
// whether it was not already.
func (s *Store) SetShared(name string) (bool, error) {
	v, err := s.Get(name)
	if err != nil {
		return false, err
	}
	if v.Shared {
		return false, nil
	}
	v.Shared = true
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := storefile.Replace(filepath.Join(v.dir, "volume.json"), append(data, '\n')); err != nil {
		return false, fmt.Errorf("volume %s: %w", v.Name, err)
	}
	return true, nil
}

// Create makes a new volume with a file system of size bytes. A volume
// is made in the store's tmp directory and moved into place whole, so a
// volume is either there and complete or not there at all.
func (s *Store) Create(name string, size int64) (*Volume, error) {
	return s.CreateWith(name, size, nil)
}

// CreateWith is Create, the volume labelled with labels.
func (s *Store) CreateWith(name string, size int64, labels map[string]string) (*Volume, error) {
	return s.CreateOpts(name, Options{Size: size, Labels: labels})
}

// CreateOpts makes a new volume as o says.
func (s *Store) CreateOpts(name string, o Options) (*Volume, error) {
	size, labels := o.Size, o.Labels
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if _, err := s.Get(name); err == nil {
		return nil, fmt.Errorf("%s: %w", name, ErrExists)
	}
	tmp := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.root, "volumes"), 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(tmp, "volume-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	v := &Volume{Name: name, CreatedAt: time.Now().UTC().Truncate(time.Second), Size: size, Labels: labels, Shared: o.Shared, dir: work}
	f, err := disk.Create(v.Disk(), size, disk.Exclusive)
	if err != nil {
		return nil, fmt.Errorf("creating volume %s: %w", name, err)
	}
	label := name
	if len(label) > 16 {
		label = label[:16]
	}
	err = ext4.Format(f, size, ext4.Options{Journal: true, Label: label})
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("formatting volume %s: %w", name, err)
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "volume.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	v.dir = s.dir(name)
	if err := os.Rename(work, v.dir); err != nil {
		if _, gerr := s.Get(name); gerr == nil {
			return nil, fmt.Errorf("%s: %w", name, ErrExists)
		}
		return nil, fmt.Errorf("creating volume %s: %w", name, err)
	}
	return v, nil
}

// Ensure returns the volume called name, creating it with a file system of
// size bytes if there is none, and reports whether it did.
func (s *Store) Ensure(name string, size int64) (*Volume, bool, error) {
	return s.EnsureWith(name, size, nil)
}

// EnsureWith is Ensure, a volume it makes labelled with labels.
func (s *Store) EnsureWith(name string, size int64, labels map[string]string) (*Volume, bool, error) {
	return s.EnsureOpts(name, Options{Size: size, Labels: labels})
}

// EnsureOpts returns the volume called name, making it as o says if there
// is none, and reports whether it did. An existing volume o asks to share
// is marked shared.
func (s *Store) EnsureOpts(name string, o Options) (*Volume, bool, error) {
	v, err := s.Get(name)
	if err == nil {
		if o.Shared && !v.Shared {
			if _, err := s.SetShared(name); err != nil {
				return nil, false, err
			}
			v.Shared = true
		}
		return v, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return v, false, err
	}
	v, err = s.CreateOpts(name, o)
	if errors.Is(err, ErrExists) {
		// Another runwhp made it first.
		return s.EnsureOpts(name, o)
	}
	return v, err == nil, err
}

// List returns every volume, sorted by name.
func (s *Store) List() ([]*Volume, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "volumes"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Volume
	for _, e := range entries {
		if !e.IsDir() || CheckName(e.Name()) != nil {
			continue
		}
		v, err := s.Get(e.Name())
		if err != nil {
			continue // a volume being removed
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Remove deletes the volume called name and everything in it, unless a
// running container is using it.
func (s *Store) Remove(name string) error {
	v, err := s.Get(name)
	if err != nil {
		return err
	}
	// A server serving no container has the disk only until its grace
	// ends: stop it now. One serving any keeps it, and the rename below
	// fails.
	v.StopIdleServer()
	// Move it aside first, so that it is gone or complete, never half
	// removed. Windows will not rename a directory holding a file that
	// is open without FILE_SHARE_DELETE, as a volume in use is.
	var b [8]byte
	rand.Read(b[:])
	trash := filepath.Join(s.root, "tmp", "removed-"+hex.EncodeToString(b[:]))
	os.MkdirAll(filepath.Dir(trash), 0o755)
	if err := os.Rename(v.dir, trash); err != nil {
		if busy, _ := v.InUse(); busy {
			return fmt.Errorf("%s: %w", v.Name, ErrInUse)
		}
		return fmt.Errorf("removing volume %s: %w", v.Name, err)
	}
	return os.RemoveAll(trash)
}
