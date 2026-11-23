//go:build windows

package hostfs

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// Share is one host directory or file the guest sees.
type Share struct {
	Path     string // a host directory, or a host file when File is set
	File     bool
	ReadOnly bool
	// Target is where the guest mounts the share. Windows links whose
	// target lies inside the share are shown pointing there.
	Target string
}

// Default modes of files without WSL metadata. Files get the execute bits
// because Windows keeps none, and a script checked out on Windows would
// otherwise not run.
const (
	defaultDirMode  = 0o755
	defaultFileMode = 0o755
)

type share struct {
	Share
	root path   // NT path of Path, with links in it resolved
	win  string // Win32 path of Path
	eas  bool   // the volume keeps extended attributes, so WSL metadata
	node *node
}

// node is a host file the guest knows by node id. Nodes are keyed by their
// file id, so a file reached through two names, or through two spellings of
// one name on a case-insensitive directory, is one inode in the guest.
type node struct {
	id      uint64
	share   *share // nil for the root of all shares
	parent  *node  // nil for a share's root
	name    string // host name within parent
	key     fileID
	lookups uint64 // lookups the kernel has not forgotten
	kids    int    // nodes whose parent this is
	pinned  bool   // the root and the shares' roots stay forever
	// dir is set for directories, and only names in them are looked up.
	// A link to a directory is not one: the guest resolves links itself,
	// and the host must never follow one.
	dir bool
	// slow is set when the file's last stat said Windows may have to fetch
	// its data before it can be opened or read.
	slow bool
}

type nodeKey struct {
	share *share
	id    fileID
}

type handle struct {
	h   windows.Handle // 0 for a directory
	dir *dirHandle
}

// FS is a fuse.FileSystem over the shares.
type FS struct {
	shares []*share
	root   *node
	start  time.Time
	logf   func(format string, a ...any)

	mu      sync.Mutex
	nodes   map[uint64]*node
	byKey   map[nodeKey]*node
	nextID  uint64
	handles map[uint64]*handle
	nextFH  uint64

	// owner is the default owner, uid<<32 | gid, of files without WSL
	// metadata. Init sets it through guestspec.OwnerXattr.
	owner atomic.Uint64
}

// New opens the shares. logf, when set, reports problems the guest only
// sees as an errno.
func New(shares []Share, logf func(format string, a ...any)) (*FS, error) {
	fs := &FS{
		start:   time.Now(),
		logf:    logf,
		nodes:   map[uint64]*node{},
		byKey:   map[nodeKey]*node{},
		nextID:  fuse.RootID + 1,
		handles: map[uint64]*handle{},
		nextFH:  1,
	}
	if fs.logf == nil {
		fs.logf = func(string, ...any) {}
	}
	fs.root = &node{id: fuse.RootID, pinned: true}
	fs.nodes[fuse.RootID] = fs.root
	for _, s := range shares {
		sh, err := openShare(s)
		if err != nil {
			return nil, err
		}
		st, err := statPath(sh.root)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path, err)
		}
		n := &node{id: fs.nextID, share: sh, key: st.key, pinned: true, dir: !s.File}
		fs.nextID++
		fs.nodes[n.id] = n
		fs.byKey[nodeKey{sh, st.key}] = n
		sh.node = n
		fs.shares = append(fs.shares, sh)
	}
	return fs, nil
}

// openShare resolves a share's path, following links in it, since the user
// named it, and checks that it is what the share says.
func openShare(s Share) (*share, error) {
	name, err := windows.UTF16PtrFromString(s.Path)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s.Path, err)
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	isDir := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if s.File && isDir {
		return nil, fmt.Errorf("%s is a directory", s.Path)
	}
	if !s.File && !isDir {
		return nil, fmt.Errorf("%s is not a directory", s.Path)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return nil, fmt.Errorf("%s: resolving the path: %w", s.Path, err)
	}
	win := windows.UTF16ToString(buf[:n])
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return &share{
		Share: s,
		root:  ntPath(win),
		win:   win,
		eas:   flags&windows.FILE_SUPPORTS_EXTENDED_ATTRIBUTES != 0,
	}, nil
}

// Close closes every handle the guest left open.
func (fs *FS) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for fh, h := range fs.handles {
		if h.h != 0 {
			windows.CloseHandle(h.h)
		}
		delete(fs.handles, fh)
	}
	return nil
}

// get returns node id.
func (fs *FS) get(id uint64) (*node, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := fs.nodes[id]
	if n == nil {
		return nil, fuse.ESTALE
	}
	return n, nil
}

// getPath returns node id and its host path.
func (fs *FS) getPath(id uint64) (*node, path, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := fs.nodes[id]
	if n == nil {
		return nil, "", fuse.ESTALE
	}
	if n == fs.root {
		return n, "", nil
	}
	return n, fs.pathLocked(n), nil
}

func (fs *FS) pathLocked(n *node) path {
	if n.parent == nil {
		return n.share.root
	}
	return fs.pathLocked(n.parent).child(n.name)
}

// child resolves the parent of a new or named entry, refusing what may not
// have children or may not change.
func (fs *FS) child(parent uint64, name string, write bool) (*node, path, error) {
	fs.mu.Lock()
	p := fs.nodes[parent]
	var pp path
	var dir bool
	if p != nil && p != fs.root {
		pp, dir = fs.pathLocked(p), p.dir
	}
	fs.mu.Unlock()
	switch {
	case p == nil:
		return nil, "", fuse.ESTALE
	case p == fs.root && write:
		return nil, "", fuse.EROFS
	case p == fs.root:
		return p, "", nil
	case !dir:
		return nil, "", fuse.ENOTDIR
	}
	if write && p.share.ReadOnly {
		return nil, "", fuse.EROFS
	}
	hn, err := hostName(name)
	if err != nil {
		return nil, "", err
	}
	return p, pp.child(hn), nil
}

// attachLocked returns the node for the file st, called name in parent,
// creating it if the guest has none. It does not count a lookup.
func (fs *FS) attachLocked(parent *node, name string, st *fileStat) *node {
	k := nodeKey{parent.share, st.key}
	if n := fs.byKey[k]; n != nil {
		n.dir = st.isDir() && !st.isLink()
		n.slow = st.recalls()
		if n.parent == nil { // a share's root, seen through a link
			return n
		}
		if n.parent != parent {
			old := n.parent
			old.kids--
			parent.kids++
			n.parent = parent
			fs.reapLocked(old)
		}
		n.name = name
		return n
	}
	n := &node{id: fs.nextID, share: parent.share, parent: parent, name: name, key: st.key, dir: st.isDir() && !st.isLink(),
		slow: st.recalls()}
	fs.nextID++
	parent.kids++
	fs.nodes[n.id] = n
	fs.byKey[k] = n
	return n
}

// reapLocked drops n, and then its ancestors, once nothing refers to them.
func (fs *FS) reapLocked(n *node) {
	for n != nil && !n.pinned && n.lookups == 0 && n.kids == 0 {
		delete(fs.nodes, n.id)
		if k := (nodeKey{n.share, n.key}); fs.byKey[k] == n {
			delete(fs.byKey, k)
		}
		p := n.parent
		if p != nil {
			p.kids--
		}
		n = p
	}
}

// forgetKeyLocked stops the key of a deleted file leading to its node, so
// that a new file that reuses the id gets a node of its own.
func (fs *FS) forgetKeyLocked(sh *share, st *fileStat) {
	if st.nlink > 1 {
		return // other names still lead to the file
	}
	k := nodeKey{sh, st.key}
	if n := fs.byKey[k]; n != nil && !n.pinned {
		delete(fs.byKey, k)
	}
}

// entry counts a lookup of the file key, called name in parent, and returns
// its entry.
func (fs *FS) entry(parent *node, name string, st *fileStat) fuse.Entry {
	fs.mu.Lock()
	n := fs.attachLocked(parent, name, st)
	n.lookups++
	fs.mu.Unlock()
	return fuse.Entry{Node: n.id, Attr: fs.attr(st)}
}

func (fs *FS) Lookup(c fuse.Context, parent uint64, name string) (fuse.Entry, error) {
	p, target, err := fs.child(parent, name, false)
	if err != nil {
		return fuse.Entry{}, err
	}
	if p == fs.root {
		sh, err := fs.shareNamed(name)
		if err != nil {
			return fuse.Entry{}, err
		}
		st, err := statPath(sh.root)
		if err != nil {
			return fuse.Entry{}, errno(err)
		}
		fs.mu.Lock()
		sh.node.lookups++
		fs.mu.Unlock()
		return fuse.Entry{Node: sh.node.id, Attr: fs.attr(&st)}, nil
	}
	st, err := statPath(target)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	return fs.entry(p, baseName(target), &st), nil
}

func baseName(p path) string {
	return string(p[strings.LastIndexByte(string(p), '\\')+1:])
}

func (fs *FS) shareNamed(name string) (*share, error) {
	i, err := strconv.Atoi(name)
	if err != nil || i < 0 || i >= len(fs.shares) || strconv.Itoa(i) != name {
		return nil, fuse.ENOENT
	}
	return fs.shares[i], nil
}

func (fs *FS) Forget(id, nlookup uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := fs.nodes[id]
	if n == nil {
		return
	}
	n.lookups -= min(nlookup, n.lookups)
	fs.reapLocked(n)
}

func (fs *FS) Getattr(c fuse.Context, id uint64) (fuse.Attr, error) {
	n, p, err := fs.getPath(id)
	if err != nil {
		return fuse.Attr{}, err
	}
	if n == fs.root {
		return fs.rootAttr(), nil
	}
	st, err := statPath(p)
	if err != nil {
		return fuse.Attr{}, errno(err)
	}
	fs.mu.Lock()
	n.slow = st.recalls()
	fs.mu.Unlock()
	return fs.attr(&st), nil
}

// rootAttr describes the root of all shares, which only init sees.
func (fs *FS) rootAttr() fuse.Attr {
	return fuse.Attr{Ino: fuse.RootID, Mode: fuse.ModeDir | 0o555, Nlink: 2,
		Atime: fs.start, Mtime: fs.start, Ctime: fs.start}
}

// attr turns what Windows knows about a file into what stat(2) reports.
func (fs *FS) attr(st *fileStat) fuse.Attr {
	owner := fs.owner.Load()
	a := fuse.Attr{
		Ino:    st.key.ino(),
		Size:   uint64(st.size),
		Blocks: uint64(st.alloc+511) / 512,
		Atime:  fromFiletime(st.accessed),
		Mtime:  fromFiletime(st.written),
		Ctime:  fromFiletime(st.changed),
		Nlink:  max(st.nlink, 1),
		UID:    uint32(owner >> 32),
		GID:    uint32(owner),
	}
	typ, perm := uint32(fuse.ModeFile), uint32(defaultFileMode)
	switch {
	case st.isLink():
		typ, perm = fuse.ModeLink, 0o777
	case st.isDir():
		typ, perm = fuse.ModeDir, defaultDirMode
	case st.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		switch st.tag {
		case tagAfUnix:
			typ = fuse.ModeSocket
		case tagLxFIFO:
			typ = fuse.ModeFIFO
		case tagLxChr:
			typ = fuse.ModeChar
		case tagLxBlk:
			typ = fuse.ModeBlock
		}
	}
	switch {
	case st.lxFlags&lxHasMode != 0 && typ != fuse.ModeLink:
		perm = st.mode & 0o7777
	case typ == fuse.ModeFile && st.attrs&windows.FILE_ATTRIBUTE_READONLY != 0:
		perm &^= 0o222
	}
	if st.lxFlags&lxHasUID != 0 {
		a.UID = st.uid
	}
	if st.lxFlags&lxHasGID != 0 {
		a.GID = st.gid
	}
	if (typ == fuse.ModeChar || typ == fuse.ModeBlock) && st.lxFlags&lxHasDevice != 0 {
		a.Rdev = encodeDev(st.devMajor, st.devMinor)
	}
	a.Mode = typ | perm
	return a
}

// encodeDev and decodeDev convert device numbers to and from the encoding
// FUSE uses, the kernel's new_encode_dev.
func encodeDev(major, minor uint32) uint32 {
	return minor&0xff | major<<8 | (minor&^0xff)<<12
}

func decodeDev(dev uint32) (major, minor uint32) {
	return (dev & 0xfff00) >> 8, dev&0xff | (dev>>12)&0xfff00
}

func (fs *FS) Setxattr(c fuse.Context, id uint64, name string, value []byte, flags uint32) error {
	if id != fuse.RootID || name != guestspec.OwnerXattr {
		return fuse.EOPNOTSUPP
	}
	uid, gid, ok := strings.Cut(string(value), ":")
	u, err1 := strconv.ParseUint(uid, 10, 32)
	g, err2 := strconv.ParseUint(gid, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return fuse.EINVAL
	}
	fs.owner.Store(u<<32 | g)
	return nil
}

func (fs *FS) Statfs(c fuse.Context, id uint64) (fuse.Statfs, error) {
	n, err := fs.get(id)
	if err != nil {
		return fuse.Statfs{}, err
	}
	sh := n.share
	if sh == nil {
		if len(fs.shares) == 0 {
			return fuse.Statfs{Bsize: 4096, Frsize: 4096, Namelen: maxName}, nil
		}
		sh = fs.shares[0]
	}
	dir := sh.win
	if sh.File {
		dir = dir[:strings.LastIndexByte(dir, '\\')]
	}
	name, err := windows.UTF16PtrFromString(dir + `\`)
	if err != nil {
		return fuse.Statfs{}, fuse.EINVAL
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &avail, &total, &free); err != nil {
		return fuse.Statfs{}, errno(err)
	}
	const bsize = 4096
	// Windows has no inode limit; report as many as there are blocks.
	return fuse.Statfs{Blocks: total / bsize, Bfree: free / bsize, Bavail: avail / bsize,
		Files: total / bsize, Ffree: free / bsize, Bsize: bsize, Frsize: bsize, Namelen: maxName}, nil
}

// Slow implements fuse.Slower: requests about a file Windows may have to
// fetch from elsewhere before opening or reading it, as for a OneDrive
// placeholder, may take seconds.
func (fs *FS) Slow(id uint64) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := fs.nodes[id]
	return n != nil && n.slow
}
