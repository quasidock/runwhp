//go:build windows

package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/volume"
)

var (
	ErrNotFound  = errors.New("no such container")
	ErrAmbiguous = errors.New("more than one container matches")
	ErrRunning   = errors.New("container is running")
	ErrNameInUse = errors.New("name is already in use")
)

// creatingGrace is how long a record may say it is being created without
// its runwhp holding its lock: the time between making the record and
// locking it, or else a runwhp that died meanwhile.
const creatingGrace = time.Minute

// Store is the containers in one runwhp store:
//
//	containers\<id>\config.json   Config
//	containers\<id>\state.json    State
//	containers\<id>\name          the container's name, if it has one
//	containers\<id>\lock          held, exclusively, by the runwhp running it
//	containers\<id>\rw.ext4       the writable layer
//	names\<name>                  the ID of the container called name
type Store struct {
	root string
}

func Open(root string) *Store { return &Store{root: root} }

// Container is one container's record, as read from the store.
type Container struct {
	ID     string
	Name   string // empty when it has none
	Config *Config
	State  State

	dir string
}

// Short is the ID's first 12 hex digits, as Docker shows them.
func (c *Container) Short() string { return c.ID[:12] }

// Dir is the container's directory in the store.
func (c *Container) Dir() string { return c.dir }

// RW is the path of the container's writable layer.
func (c *Container) RW() string { return filepath.Join(c.dir, "rw.ext4") }

// LogPath is the path of the container's log, the file being written; the
// files rotated away from it are beside it, as log.1 and so on.
func (c *Container) LogPath() string { return filepath.Join(c.dir, "log") }

// DiagLogPath is the path of runwhp's own messages about the container,
// when it runs in the background; the file before it is beside it, as
// runwhp.log.1.
func (c *Container) DiagLogPath() string { return filepath.Join(c.dir, "runwhp.log") }

// LogPaths lists the container's logs and runwhp's, rotated files
// included.
func (c *Container) LogPaths() []string {
	var out []string
	for _, pattern := range []string{"log", "log.*", "runwhp.log", "runwhp.log.*"} {
		m, _ := filepath.Glob(filepath.Join(c.dir, pattern))
		out = append(out, m...)
	}
	return out
}

func (c *Container) lockPath() string { return filepath.Join(c.dir, "lock") }

// Locked reports whether a runwhp holds the container's lock, which is to
// say it has a VM.
func (c *Container) Locked() bool {
	p, err := windows.UTF16PtrFromString(c.lockPath())
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true
	}
	if err == nil {
		windows.CloseHandle(h)
	}
	return false
}

// NewID returns a new container ID: 64 random hex digits.
func NewID() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func (s *Store) dir(id string) string { return filepath.Join(s.root, "containers", id) }
func (s *Store) nameFile(name string) string {
	return filepath.Join(s.root, "names", name)
}

// CheckName checks a container name: Docker's rules, and a name Windows can
// keep as a file. Names are case-insensitive.
func CheckName(name string) error {
	if err := volume.CheckName(name); err != nil {
		return fmt.Errorf("invalid container name %q: use letters, digits, _, . and -, starting with a letter or digit", name)
	}
	return nil
}

// Create records a new container, with the state Creating, under cfg.ID
// and, unless it is empty, name. The record is made in the store's tmp
// directory and moved into place whole.
func (s *Store) Create(cfg *Config, name string) (*Container, error) {
	return s.CreateWith(cfg, name, State{Status: Creating})
}

// CreateWith is Create, with st, whose status must be Creating, as the
// container's first state: the process that makes a container and runs it
// records itself in one write.
func (s *Store) CreateWith(cfg *Config, name string, st State) (*Container, error) {
	if !isID(cfg.ID) {
		return nil, fmt.Errorf("invalid container ID %q", cfg.ID)
	}
	if name != "" {
		if err := CheckName(name); err != nil {
			return nil, err
		}
	}
	for _, d := range []string{"tmp", "containers", "names"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			return nil, err
		}
	}
	work, err := os.MkdirTemp(filepath.Join(s.root, "tmp"), "container-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	st.Status = Creating
	c := &Container{ID: cfg.ID, Name: name, Config: cfg, State: st, dir: work}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(work, "config.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(work, "state.json"), c.State); err != nil {
		return nil, err
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(work, "name"), []byte(name), 0o644); err != nil {
			return nil, err
		}
	}
	c.dir = s.dir(cfg.ID)
	if err := os.Rename(work, c.dir); err != nil {
		return nil, fmt.Errorf("recording container %s: %w", c.Short(), err)
	}
	// The name is reserved once the record is in place, so that a name
	// whose container is not there was left by one that is gone.
	if name != "" {
		if err := s.reserve(name, cfg.ID); err != nil {
			c.Name = ""
			s.Remove(c)
			return nil, err
		}
	}
	return c, nil
}

// reserve makes names\<name> hold id, unless another container has the
// name. An entry left by a container that is gone is taken over.
func (s *Store) reserve(name, id string) error {
	p := s.nameFile(name)
	for try := 0; ; try++ {
		err := createWith(p, []byte(id))
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrExist) || try > 0 {
			return err
		}
		owner, rerr := readShared(p)
		if rerr != nil {
			return err
		}
		if string(owner) == id {
			return nil
		}
		if _, serr := os.Stat(s.dir(string(owner))); serr == nil || len(owner) == 0 {
			return fmt.Errorf("the name %q is already in use by container %.12s: %w", name, owner, ErrNameInUse)
		}
		os.Remove(p)
	}
}

// release removes names\<name> if it holds id.
func (s *Store) release(name, id string) {
	p := s.nameFile(name)
	if owner, err := readShared(p); err == nil && string(owner) == id {
		os.Remove(p)
	}
}

// Get reads the record of the container with the full ID id, without
// looking at whether it still runs.
func (s *Store) Get(id string) (*Container, error) {
	if !isID(id) {
		return nil, ErrNotFound
	}
	dir := s.dir(id)
	data, err := readShared(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%.12s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	c := &Container{ID: id, Config: &Config{}, dir: dir}
	if err := json.Unmarshal(data, c.Config); err != nil {
		return nil, fmt.Errorf("container %.12s: bad config.json: %w", id, err)
	}
	if data, err = readShared(filepath.Join(dir, "state.json")); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &c.State); err != nil {
		return nil, fmt.Errorf("container %.12s: bad state.json: %w", id, err)
	}
	if name, err := readShared(filepath.Join(dir, "name")); err == nil {
		c.Name = string(name)
	}
	return c, nil
}

// Resolve finds the container ref names: by its full ID, else its name,
// else a unique prefix of its ID. The state it returns is current: a
// container whose runwhp died is recorded as stopped.
func (s *Store) Resolve(ref string) (*Container, error) {
	c, err := s.resolve(ref)
	if err != nil {
		return nil, err
	}
	s.refresh(c)
	return c, nil
}

func (s *Store) resolve(ref string) (*Container, error) {
	if ref == "" {
		return nil, fmt.Errorf("no container given: %w", ErrNotFound)
	}
	if isID(ref) {
		if c, err := s.Get(ref); err == nil {
			return c, nil
		}
	}
	if CheckName(ref) == nil {
		if id, err := readShared(s.nameFile(ref)); err == nil {
			if c, err := s.Get(string(id)); err == nil && strings.EqualFold(c.Name, ref) {
				return c, nil
			}
		}
	}
	prefix := strings.ToLower(ref)
	if _, err := hex.DecodeString(prefix + strings.Repeat("0", len(prefix)%2)); err != nil || len(prefix) > 64 {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var match []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			match = append(match, id)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	case 1:
		return s.Get(match[0])
	}
	return nil, fmt.Errorf("%s: %w", ref, ErrAmbiguous)
}

func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "containers"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// refresh brings c's state up to date with whether a runwhp still holds
// its lock. A container whose runwhp died without recording how it ended
// is recorded as stopped.
func (s *Store) refresh(c *Container) {
	if !c.State.Active() || c.Locked() {
		return
	}
	if c.State.Status == Creating && time.Since(c.Config.Created) < creatingGrace {
		return
	}
	l, err := s.lock(c, false)
	if err != nil {
		return
	}
	defer l.Close()
	data, err := readShared(filepath.Join(c.dir, "state.json"))
	if err != nil {
		return
	}
	var st State
	if json.Unmarshal(data, &st) != nil {
		return
	}
	if st.Active() {
		// A record that was never finished has nothing to say; a container
		// being started again had ended before. One waiting to be started
		// again had ended, and how is recorded.
		switch {
		case st.Status == Restarting:
			st.Error = "runwhp's process ended while the container waited to be started again"
		case st.Status != Creating || !st.FinishedAt.IsZero():
			st.ExitCode = 255
			st.Error = "runwhp's process ended without recording how the container ended"
		}
		st.Status, st.Pid, st.GuestPid = Stopped, 0, 0
		if st.FinishedAt.IsZero() {
			st.FinishedAt = time.Now().UTC()
		}
		l.WriteState(st)
	}
	c.State = st
}

// List returns every container, newest first, with its state current. It
// also removes what runwhps that died left behind: containers that were to
// be removed when they stopped, and records that were never finished.
func (s *Store) List() ([]*Container, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var out []*Container
	for _, id := range ids {
		c, err := s.Get(id)
		if err != nil {
			continue // being removed
		}
		// A record whose runwhp died while making it was never finished; one
		// that had stopped before and was being started again is kept.
		unfinished := c.State.Status == Creating && c.State.FinishedAt.IsZero()
		s.refresh(c)
		if !c.State.Active() && (c.Config.AutoRemove || unfinished) {
			s.Remove(c)
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Config.Created.After(out[j].Config.Created) })
	return out, nil
}

// Remove deletes a container and everything it holds, unless a runwhp
// holds its lock.
func (s *Store) Remove(c *Container) error {
	if c.Locked() {
		return fmt.Errorf("container %s: %w", c.Short(), ErrRunning)
	}
	// Move it aside first, so that it is gone or whole, never half removed.
	// Windows will not rename a directory holding a file that is open
	// without FILE_SHARE_DELETE, as the lock of a running container is.
	var b [8]byte
	rand.Read(b[:])
	trash := filepath.Join(s.root, "tmp", "removed-"+hex.EncodeToString(b[:]))
	os.MkdirAll(filepath.Dir(trash), 0o755)
	var err error
	for try := 0; try < 3; try++ {
		if err = os.Rename(c.dir, trash); err == nil || errors.Is(err, os.ErrNotExist) {
			break
		}
		if c.Locked() {
			return fmt.Errorf("container %s: %w", c.Short(), ErrRunning)
		}
		time.Sleep(100 * time.Millisecond) // an antivirus scanner, say
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", c.Short(), ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("removing container %s: %w", c.Short(), err)
	}
	if c.Name != "" {
		s.release(c.Name, c.ID)
	}
	return os.RemoveAll(trash)
}

// Rename gives a container a new name, which may differ from its old one
// only in case.
func (s *Store) Rename(c *Container, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := s.reserve(name, c.ID); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(c.dir, "name"), []byte(name)); err != nil {
		if !strings.EqualFold(name, c.Name) {
			s.release(name, c.ID)
		}
		return err
	}
	if c.Name != "" && !strings.EqualFold(name, c.Name) {
		s.release(c.Name, c.ID)
	}
	if strings.EqualFold(name, c.Name) {
		// The names file keeps the case it was made with.
		writeAtomic(s.nameFile(name), []byte(c.ID))
	}
	c.Name = name
	return nil
}

// Referencing returns the containers that mount the named volume.
func (s *Store) Referencing(volumeName string) ([]*Container, error) {
	cs, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*Container
	for _, c := range cs {
		for _, v := range c.Config.Volumes {
			if strings.EqualFold(v.Name, volumeName) {
				out = append(out, c)
				break
			}
		}
	}
	return out, nil
}

// Lock is a container's lock, held by the runwhp that runs it.
type Lock struct {
	c *Container
	h windows.Handle
}

// Lock takes c's lock, or returns ErrRunning if another runwhp holds it.
func (s *Store) Lock(c *Container) (*Lock, error) { return s.lock(c, true) }

func (s *Store) lock(c *Container, retry bool) (*Lock, error) {
	p, err := windows.UTF16PtrFromString(c.lockPath())
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	for try := 0; ; try++ {
		h, err = windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || !retry || try == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, fmt.Errorf("container %s: %w", c.Short(), ErrRunning)
	}
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, fmt.Errorf("%s: %w", c.Short(), ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("locking container %s: %w", c.Short(), err)
	}
	return &Lock{c: c, h: h}, nil
}

// WriteState records st as the container's state.
func (l *Lock) WriteState(st State) error {
	if err := writeJSON(filepath.Join(l.c.dir, "state.json"), st); err != nil {
		return err
	}
	l.c.State = st
	return nil
}

// Close releases the lock.
func (l *Lock) Close() error {
	if l == nil || l.h == 0 {
		return nil
	}
	err := windows.CloseHandle(l.h)
	l.h = 0
	return err
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}
