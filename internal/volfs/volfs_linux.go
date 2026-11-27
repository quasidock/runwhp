// Package volfs serves a directory of the guest's own, a shared volume's
// ext4 file system, to other guests over FUSE, as virtiofsd's passthrough
// does: each FUSE request becomes the same system call on the directory's
// files, made as root on behalf of the caller, whose permissions the
// calling guest's kernel has already checked (virtio-fs mounts with
// default_permissions).
//
// Each guest using the volume has a Session of its own, with its own node
// ids and file handles, over one Volume shared by all: the files, and the
// locks on them, are the same for every guest.
package volfs

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/fuse"
)

// Volume is the directory served, shared by every session.
type Volume struct {
	root   int // O_PATH descriptor of the directory
	rootRD int // the directory opened for reading, for statfs and syncfs
	ino    uint64

	waitMu sync.Mutex
	wakes  map[uint64]chan struct{} // inode -> closed when its locks change
}

// Open opens the directory at dir to serve. It raises the process's limit
// on open files as far as it goes, since a session holds a descriptor for
// every file its guest has looked up.
func Open(dir string) (*Volume, error) {
	var lim unix.Rlimit
	if b, err := os.ReadFile("/proc/sys/fs/nr_open"); err == nil {
		if n, err := strconv.ParseUint(string(trimNL(b)), 10, 64); err == nil {
			lim = unix.Rlimit{Cur: n, Max: n}
			unix.Setrlimit(unix.RLIMIT_NOFILE, &lim)
		}
	}
	root, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	rd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(root)
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(rd, &st); err != nil {
		unix.Close(root)
		unix.Close(rd)
		return nil, err
	}
	return &Volume{root: root, rootRD: rd, ino: st.Ino, wakes: map[uint64]chan struct{}{}}, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// Close closes the volume's directory. Sessions must be closed first.
func (v *Volume) Close() error {
	unix.Close(v.rootRD)
	return unix.Close(v.root)
}

// wake tells those waiting for a lock on ino that its locks have changed.
func (v *Volume) wake(ino uint64) {
	v.waitMu.Lock()
	if ch, ok := v.wakes[ino]; ok {
		close(ch)
		delete(v.wakes, ino)
	}
	v.waitMu.Unlock()
}

// changed returns a channel closed when ino's locks next change.
func (v *Volume) changed(ino uint64) <-chan struct{} {
	v.waitMu.Lock()
	defer v.waitMu.Unlock()
	ch, ok := v.wakes[ino]
	if !ok {
		ch = make(chan struct{})
		v.wakes[ino] = ch
	}
	return ch
}

// Session is one guest's view of the volume: a fuse.FileSystem, with
// fuse.Xattrs, fuse.Locker, fuse.Allocator, fuse.Seeker, fuse.RangeCopier
// and fuse.Syncer.
type Session struct {
	v *Volume

	mu      sync.Mutex
	nodes   map[uint64]*node
	handles map[uint64]*handle
	nextFh  uint64
	closed  chan struct{}
}

type node struct {
	id     uint64
	fd     int // O_PATH|O_NOFOLLOW; the root's is the volume's
	ino    uint64
	mode   uint32 // type bits
	lookup uint64
	// owners holds, for each POSIX lock owner with locks on the file, the
	// descriptor holding them, as open file description locks.
	owners map[uint64]int
}

type handle struct {
	fd   int
	node *node
	dir  *dirStream // for directories
}

// NewSession starts a guest's session.
func (v *Volume) NewSession() *Session {
	s := &Session{v: v, nodes: map[uint64]*node{}, handles: map[uint64]*handle{}, closed: make(chan struct{})}
	s.nodes[fuse.RootID] = &node{id: fuse.RootID, fd: v.root, ino: v.ino, mode: fuse.ModeDir, lookup: 1, owners: map[uint64]int{}}
	return s
}

// Close ends the session: its files are closed and its locks let go, and
// its requests waiting for a lock fail.
func (s *Session) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return
	default:
	}
	close(s.closed)
	var inos []uint64
	for _, h := range s.handles {
		unix.Close(h.fd)
		inos = append(inos, h.node.ino)
	}
	for _, n := range s.nodes {
		for _, fd := range n.owners {
			unix.Close(fd)
		}
		if len(n.owners) > 0 {
			inos = append(inos, n.ino)
		}
		if n.id != fuse.RootID {
			unix.Close(n.fd)
		}
	}
	s.nodes, s.handles = map[uint64]*node{}, map[uint64]*handle{}
	s.mu.Unlock()
	for _, ino := range inos {
		s.v.wake(ino)
	}
}

// errno converts a system call's error to what the guest gets.
func errno(err error) error {
	if err == nil {
		return nil
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return fuse.Errno(e)
	}
	return err
}

func (s *Session) node(id uint64) (*node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, fuse.ESTALE
	}
	return n, nil
}

func (s *Session) handle(fh uint64) (*handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.handles[fh]
	if !ok {
		return nil, fuse.EBADF
	}
	return h, nil
}

// checkName refuses names that are not one element of a path.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return fuse.EINVAL
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == 0 {
			return fuse.EINVAL
		}
	}
	return nil
}

// procPath is a path that opens what fd refers to.
func procPath(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

func attrOf(st *unix.Stat_t) fuse.Attr {
	return fuse.Attr{
		Ino:    st.Ino,
		Size:   uint64(st.Size),
		Blocks: uint64(st.Blocks),
		Atime:  time.Unix(st.Atim.Sec, st.Atim.Nsec),
		Mtime:  time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
		Ctime:  time.Unix(st.Ctim.Sec, st.Ctim.Nsec),
		Mode:   st.Mode,
		Nlink:  uint32(st.Nlink),
		UID:    st.Uid,
		GID:    st.Gid,
		Rdev:   uint32(st.Rdev),
	}
}

func fstat(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(fd, "", &st, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

// entry looks up name in parent, counting one lookup of the node it
// returns.
func (s *Session) entry(parent *node, name string) (fuse.Entry, error) {
	fd, err := unix.Openat(parent.fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	return s.adopt(fd)
}

// adopt makes fd, an O_PATH descriptor, a node of the session, or counts
// one more lookup of the node already there for its file, closing fd.
func (s *Session) adopt(fd int) (fuse.Entry, error) {
	st, err := fstat(fd)
	if err != nil {
		unix.Close(fd)
		return fuse.Entry{}, errno(err)
	}
	id := st.Ino
	if st.Ino == s.v.ino {
		id = fuse.RootID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		unix.Close(fd)
		return fuse.Entry{}, fuse.EIO
	default:
	}
	if n, ok := s.nodes[id]; ok {
		unix.Close(fd)
		n.lookup++
	} else {
		s.nodes[id] = &node{id: id, fd: fd, ino: st.Ino, mode: st.Mode & fuse.ModeType, lookup: 1, owners: map[uint64]int{}}
	}
	return fuse.Entry{Node: id, Attr: attrOf(&st)}, nil
}

func (s *Session) Lookup(c fuse.Context, parent uint64, name string) (fuse.Entry, error) {
	if err := checkName(name); err != nil {
		return fuse.Entry{}, err
	}
	p, err := s.node(parent)
	if err != nil {
		return fuse.Entry{}, err
	}
	return s.entry(p, name)
}

func (s *Session) Forget(id, n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nd, ok := s.nodes[id]
	if !ok || id == fuse.RootID {
		return
	}
	if nd.lookup > n {
		nd.lookup -= n
		return
	}
	for _, fd := range nd.owners {
		unix.Close(fd)
	}
	unix.Close(nd.fd)
	delete(s.nodes, id)
}

func (s *Session) Getattr(c fuse.Context, id uint64) (fuse.Attr, error) {
	n, err := s.node(id)
	if err != nil {
		return fuse.Attr{}, err
	}
	st, err := fstat(n.fd)
	if err != nil {
		return fuse.Attr{}, errno(err)
	}
	return attrOf(&st), nil
}

func (s *Session) Setattr(c fuse.Context, id uint64, in fuse.SetattrIn) (fuse.Attr, error) {
	n, err := s.node(id)
	if err != nil {
		return fuse.Attr{}, err
	}
	hfd := -1
	if in.Valid&fuse.SetHandle != 0 {
		if h, err := s.handle(in.Handle); err == nil {
			hfd = h.fd
		}
	}
	if in.Valid&fuse.SetMode != 0 {
		if n.mode == fuse.ModeLink {
			return fuse.Attr{}, fuse.EOPNOTSUPP
		}
		if err := unix.Chmod(procPath(n.fd), in.Mode&0o7777); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if in.Valid&(fuse.SetUID|fuse.SetGID) != 0 {
		uid, gid := -1, -1
		if in.Valid&fuse.SetUID != 0 {
			uid = int(in.UID)
		}
		if in.Valid&fuse.SetGID != 0 {
			gid = int(in.GID)
		}
		if err := unix.Fchownat(n.fd, "", uid, gid, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if in.Valid&fuse.SetSize != 0 {
		if hfd >= 0 {
			err = unix.Ftruncate(hfd, int64(in.Size))
		} else {
			err = unix.Truncate(procPath(n.fd), int64(in.Size))
		}
		if err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if in.Valid&(fuse.SetAtime|fuse.SetMtime|fuse.SetAtimeNow|fuse.SetMtimeNow) != 0 {
		ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Nsec: unix.UTIME_OMIT}}
		switch {
		case in.Valid&fuse.SetAtimeNow != 0:
			ts[0] = unix.Timespec{Nsec: unix.UTIME_NOW}
		case in.Valid&fuse.SetAtime != 0:
			ts[0] = unix.NsecToTimespec(in.Atime.UnixNano())
		}
		switch {
		case in.Valid&fuse.SetMtimeNow != 0:
			ts[1] = unix.Timespec{Nsec: unix.UTIME_NOW}
		case in.Valid&fuse.SetMtime != 0:
			ts[1] = unix.NsecToTimespec(in.Mtime.UnixNano())
		}
		if err := unix.UtimesNanoAt(n.fd, "", ts, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	return s.Getattr(c, id)
}

func (s *Session) Readlink(c fuse.Context, id uint64) (string, error) {
	n, err := s.node(id)
	if err != nil {
		return "", err
	}
	buf := make([]byte, unix.PathMax)
	m, err := unix.Readlinkat(n.fd, "", buf)
	if err != nil {
		return "", errno(err)
	}
	return string(buf[:m]), nil
}

// as runs f on a thread whose file system ids are the caller's, so what f
// makes belongs to the caller, as it would have if the caller had made it
// itself: its owner, and its group, which the kernel takes from a setgid
// directory. The thread keeps the capabilities that let root reach any
// file, as the caller's kernel has checked its access already, and which a
// change of fsuid from 0 drops.
func as(c fuse.Context, f func() error) error {
	if c.UID == 0 && c.GID == 0 {
		return f()
	}
	runtime.LockOSThread()
	if err := setfsids(int(c.UID), int(c.GID)); err != nil {
		resetfsids()
		runtime.UnlockOSThread()
		return err
	}
	err := f()
	if resetfsids() {
		runtime.UnlockOSThread()
	}
	// Otherwise the thread stays locked, and goes with the goroutine,
	// rather than serve anyone else under the caller's ids.
	return err
}

const (
	capDacOverride   = 1
	capDacReadSearch = 2
	capFowner        = 3
	capFsetid        = 4
)

func setfsids(uid, gid int) error {
	// setfsgid first: once the fsuid is not root, the thread may no
	// longer change its fsgid.
	if _, err := unix.SetfsgidRetGid(gid); err != nil {
		return err
	}
	if prev, _ := unix.SetfsgidRetGid(-1); prev != gid {
		return fmt.Errorf("setfsgid %d did not take", gid)
	}
	if _, err := unix.SetfsuidRetUid(uid); err != nil {
		return err
	}
	if prev, _ := unix.SetfsuidRetUid(-1); prev != uid {
		return fmt.Errorf("setfsuid %d did not take", uid)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return err
	}
	data[0].Effective |= 1<<capDacOverride | 1<<capDacReadSearch | 1<<capFowner | 1<<capFsetid
	data[0].Effective &= data[0].Permitted
	return unix.Capset(&hdr, &data[0])
}

// resetfsids gives the thread root's file system ids back, which also
// raises its file system capabilities again, and reports whether it did.
func resetfsids() bool {
	unix.SetfsuidRetUid(0)
	unix.SetfsgidRetGid(0)
	u, _ := unix.SetfsuidRetUid(-1)
	g, _ := unix.SetfsgidRetGid(-1)
	return u == 0 && g == 0
}

// made looks up what a call of the caller's just made in parent.
func (s *Session) made(parent *node, name string) (fuse.Entry, error) {
	return s.entry(parent, name)
}

func (s *Session) Symlink(c fuse.Context, parent uint64, name, target string) (fuse.Entry, error) {
	p, err := s.parent(parent, name)
	if err != nil {
		return fuse.Entry{}, err
	}
	if err := as(c, func() error { return unix.Symlinkat(target, p.fd, name) }); err != nil {
		return fuse.Entry{}, errno(err)
	}
	return s.made(p, name)
}

func (s *Session) Mknod(c fuse.Context, parent uint64, name string, mode, rdev uint32) (fuse.Entry, error) {
	p, err := s.parent(parent, name)
	if err != nil {
		return fuse.Entry{}, err
	}
	if err := as(c, func() error { return unix.Mknodat(p.fd, name, mode, int(rdev)) }); err != nil {
		return fuse.Entry{}, errno(err)
	}
	return s.made(p, name)
}

func (s *Session) Mkdir(c fuse.Context, parent uint64, name string, mode uint32) (fuse.Entry, error) {
	p, err := s.parent(parent, name)
	if err != nil {
		return fuse.Entry{}, err
	}
	if err := as(c, func() error { return unix.Mkdirat(p.fd, name, mode) }); err != nil {
		return fuse.Entry{}, errno(err)
	}
	return s.made(p, name)
}

// parent returns the node of a directory in which name is to be made or
// removed.
func (s *Session) parent(id uint64, name string) (*node, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return s.node(id)
}

func (s *Session) Unlink(c fuse.Context, parent uint64, name string) error {
	p, err := s.parent(parent, name)
	if err != nil {
		return err
	}
	return errno(unix.Unlinkat(p.fd, name, 0))
}

func (s *Session) Rmdir(c fuse.Context, parent uint64, name string) error {
	p, err := s.parent(parent, name)
	if err != nil {
		return err
	}
	return errno(unix.Unlinkat(p.fd, name, unix.AT_REMOVEDIR))
}

func (s *Session) Rename(c fuse.Context, parent uint64, name string, newParent uint64, newName string, flags uint32) error {
	p, err := s.parent(parent, name)
	if err != nil {
		return err
	}
	np, err := s.parent(newParent, newName)
	if err != nil {
		return err
	}
	// RENAME_WHITEOUT leaves a device node, which only root may make: the
	// caller's kernel has checked the caller may, so the rename is root's.
	return errno(unix.Renameat2(p.fd, name, np.fd, newName, uint(flags)))
}

func (s *Session) Link(c fuse.Context, id, newParent uint64, newName string) (fuse.Entry, error) {
	n, err := s.node(id)
	if err != nil {
		return fuse.Entry{}, err
	}
	np, err := s.parent(newParent, newName)
	if err != nil {
		return fuse.Entry{}, err
	}
	if err := unix.Linkat(n.fd, "", np.fd, newName, unix.AT_EMPTY_PATH); err != nil {
		return fuse.Entry{}, errno(err)
	}
	return s.entry(np, newName)
}

// openFlags are the flags a guest's open passes on: the kernel has made
// any file already, and the server's own descriptors are closed on exec.
func openFlags(flags uint32) int {
	f := int(flags) &^ (unix.O_CREAT | unix.O_EXCL | unix.O_NOCTTY)
	return f | unix.O_CLOEXEC | unix.O_NOFOLLOW
}

func (s *Session) addHandle(fd int, n *node, dir *dirStream) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextFh++
	s.handles[s.nextFh] = &handle{fd: fd, node: n, dir: dir}
	return s.nextFh
}

func (s *Session) Open(c fuse.Context, id uint64, flags uint32) (uint64, error) {
	n, err := s.node(id)
	if err != nil {
		return 0, err
	}
	if n.mode == fuse.ModeLink {
		return 0, fuse.ELOOP
	}
	fd, err := unix.Open(procPath(n.fd), openFlags(flags)&^unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, errno(err)
	}
	return s.addHandle(fd, n, nil), nil
}

func (s *Session) Create(c fuse.Context, parent uint64, name string, flags, mode uint32) (fuse.Entry, uint64, error) {
	p, err := s.parent(parent, name)
	if err != nil {
		return fuse.Entry{}, 0, err
	}
	fd := -1
	err = as(c, func() error {
		var err error
		fd, err = unix.Openat(p.fd, name, openFlags(flags)|unix.O_CREAT|int(flags)&unix.O_EXCL, mode)
		return err
	})
	if err != nil {
		return fuse.Entry{}, 0, errno(err)
	}
	pfd, err := unix.Open(procPath(fd), unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(fd)
		return fuse.Entry{}, 0, errno(err)
	}
	e, err := s.adopt(pfd)
	if err != nil {
		unix.Close(fd)
		return fuse.Entry{}, 0, err
	}
	n, err := s.node(e.Node)
	if err != nil {
		unix.Close(fd)
		return fuse.Entry{}, 0, err
	}
	return e, s.addHandle(fd, n, nil), nil
}

func (s *Session) Read(c fuse.Context, id, fh uint64, off int64, buf []byte) (int, error) {
	h, err := s.handle(fh)
	if err != nil {
		return 0, err
	}
	total := 0
	for total < len(buf) {
		n, err := unix.Pread(h.fd, buf[total:], off+int64(total))
		if err != nil {
			if total > 0 {
				break
			}
			return 0, errno(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total, nil
}

func (s *Session) Write(c fuse.Context, id, fh uint64, off int64, data []byte) (int, error) {
	h, err := s.handle(fh)
	if err != nil {
		return 0, err
	}
	// On a descriptor opened with O_APPEND, pwrite appends, as the guest
	// asked: so do writes from several guests.
	n, err := unix.Pwrite(h.fd, data, off)
	if err != nil {
		return 0, errno(err)
	}
	return n, nil
}

func (s *Session) Flush(c fuse.Context, id, fh uint64) error { return nil }

func (s *Session) Fsync(c fuse.Context, id, fh uint64, datasync bool) error {
	h, err := s.handle(fh)
	if err != nil {
		return err
	}
	if datasync {
		return errno(unix.Fdatasync(h.fd))
	}
	return errno(unix.Fsync(h.fd))
}

// Release closes an open file, which lets go of its flock(2) locks.
func (s *Session) Release(id, fh uint64) {
	s.mu.Lock()
	h, ok := s.handles[fh]
	delete(s.handles, fh)
	s.mu.Unlock()
	if ok {
		unix.Close(h.fd)
		s.v.wake(h.node.ino)
	}
}

func (s *Session) Opendir(c fuse.Context, id uint64) (uint64, error) {
	n, err := s.node(id)
	if err != nil {
		return 0, err
	}
	fd, err := unix.Open(procPath(n.fd), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, errno(err)
	}
	return s.addHandle(fd, n, &dirStream{}), nil
}

func (s *Session) Releasedir(id, fh uint64) { s.Release(id, fh) }

func (s *Session) Statfs(c fuse.Context, id uint64) (fuse.Statfs, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(s.v.rootRD, &st); err != nil {
		return fuse.Statfs{}, errno(err)
	}
	return fuse.Statfs{
		Blocks: st.Blocks, Bfree: st.Bfree, Bavail: st.Bavail,
		Files: st.Files, Ffree: st.Ffree,
		Bsize: uint32(st.Bsize), Frsize: uint32(st.Frsize), Namelen: uint32(st.Namelen),
	}, nil
}

// xattrPath is where a node's extended attributes are reached: through
// the file itself, which a symlink cannot be opened as.
func (s *Session) xattrPath(id uint64) (string, error) {
	n, err := s.node(id)
	if err != nil {
		return "", err
	}
	if n.mode == fuse.ModeLink {
		return "", fuse.EOPNOTSUPP
	}
	return procPath(n.fd), nil
}

func (s *Session) Setxattr(c fuse.Context, id uint64, name string, value []byte, flags uint32) error {
	p, err := s.xattrPath(id)
	if err != nil {
		return err
	}
	return errno(unix.Setxattr(p, name, value, int(flags)))
}

func (s *Session) Getxattr(c fuse.Context, id uint64, name string) ([]byte, error) {
	p, err := s.xattrPath(id)
	if err != nil {
		return nil, err
	}
	return readXattr(func(b []byte) (int, error) { return unix.Getxattr(p, name, b) })
}

func (s *Session) Listxattr(c fuse.Context, id uint64) ([]byte, error) {
	p, err := s.xattrPath(id)
	if err != nil {
		if err == fuse.EOPNOTSUPP {
			return nil, nil // a symlink lists none
		}
		return nil, err
	}
	return readXattr(func(b []byte) (int, error) { return unix.Listxattr(p, b) })
}

// readXattr reads a value whose size may change between asking it and
// reading it.
func readXattr(get func([]byte) (int, error)) ([]byte, error) {
	for range 4 {
		n, err := get(nil)
		if err != nil {
			return nil, errno(err)
		}
		b := make([]byte, n)
		n, err = get(b)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, errno(err)
		}
		return b[:n], nil
	}
	return nil, fuse.ERANGE
}

func (s *Session) Removexattr(c fuse.Context, id uint64, name string) error {
	p, err := s.xattrPath(id)
	if err != nil {
		return err
	}
	return errno(unix.Removexattr(p, name))
}

func (s *Session) Fallocate(c fuse.Context, id, fh uint64, off, length uint64, mode uint32) error {
	h, err := s.handle(fh)
	if err != nil {
		return err
	}
	return errno(unix.Fallocate(h.fd, mode, int64(off), int64(length)))
}

func (s *Session) Lseek(c fuse.Context, id, fh uint64, off uint64, whence uint32) (uint64, error) {
	h, err := s.handle(fh)
	if err != nil {
		return 0, err
	}
	got, err := unix.Seek(h.fd, int64(off), int(whence))
	if err != nil {
		return 0, errno(err)
	}
	return uint64(got), nil
}

func (s *Session) CopyFileRange(c fuse.Context, nodeIn, fhIn, offIn, nodeOut, fhOut, offOut, length uint64, flags uint64) (uint64, error) {
	in, err := s.handle(fhIn)
	if err != nil {
		return 0, err
	}
	out, err := s.handle(fhOut)
	if err != nil {
		return 0, err
	}
	ri, ro := int64(offIn), int64(offOut)
	n, err := unix.CopyFileRange(in.fd, &ri, out.fd, &ro, int(min(length, 1<<30)), int(flags))
	if err != nil {
		return 0, errno(err)
	}
	return uint64(n), nil
}

// KillSuidgid clears a regular file's setuid bit, and its setgid bit if
// the group may execute it, as Linux does when someone who may not keep
// them writes to it. The server writes as root, which may, so it does what
// the guest's kernel asks. File capabilities the server's own kernel
// removes on every write.
func (s *Session) KillSuidgid(c fuse.Context, id, fh uint64) error {
	n, err := s.node(id)
	if err != nil {
		return err
	}
	st, err := fstat(n.fd)
	if err != nil {
		return errno(err)
	}
	if st.Mode&fuse.ModeType != fuse.ModeFile {
		return nil
	}
	mode := st.Mode & 0o7777
	kill := uint32(unix.S_ISUID)
	if mode&unix.S_IXGRP != 0 {
		kill |= unix.S_ISGID
	}
	if mode&kill == 0 {
		return nil
	}
	return errno(unix.Chmod(procPath(n.fd), mode&^kill))
}

func (s *Session) Syncfs(c fuse.Context) error {
	return errno(unix.Syncfs(s.v.rootRD))
}

var (
	_ fuse.FileSystem  = (*Session)(nil)
	_ fuse.Xattrs      = (*Session)(nil)
	_ fuse.Locker      = (*Session)(nil)
	_ fuse.Allocator   = (*Session)(nil)
	_ fuse.Seeker      = (*Session)(nil)
	_ fuse.RangeCopier = (*Session)(nil)
	_ fuse.Syncer      = (*Session)(nil)
)

var _ fuse.SuidgidKiller = (*Session)(nil)
