// Package fuse serves the FUSE protocol, as Linux's virtio-fs driver speaks
// it, from a FileSystem. It knows nothing of virtio or of the host's files, so
// it builds and is tested on any OS.
//
// The layouts here follow include/uapi/linux/fuse.h. The server negotiates
// protocol 7.31, so the kernel sends every request in its full, current form,
// without the extensions later minor versions added.
package fuse

import (
	"encoding/binary"
	"strconv"
	"time"
)

var le = binary.LittleEndian

// Protocol version this server speaks.
const (
	kernelVersion = 7
	minorVersion  = 31
)

// RootID is the node id of the file system's root.
const RootID = 1

// Opcodes.
const (
	opLookup      = 1
	opForget      = 2
	opGetattr     = 3
	opSetattr     = 4
	opReadlink    = 5
	opSymlink     = 6
	opMknod       = 8
	opMkdir       = 9
	opUnlink      = 10
	opRmdir       = 11
	opRename      = 12
	opLink        = 13
	opOpen        = 14
	opRead        = 15
	opWrite       = 16
	opStatfs      = 17
	opRelease     = 18
	opFsync       = 20
	opSetxattr    = 21
	opFlush       = 25
	opInit        = 26
	opOpendir     = 27
	opReaddir     = 28
	opReleasedir  = 29
	opFsyncdir    = 30
	opCreate      = 35
	opInterrupt   = 36
	opDestroy     = 38
	opBatchForget = 42
	opReaddirplus = 44
	opRename2     = 45
	maxOpcode     = 64
)

var opNames = map[uint32]string{
	opLookup: "lookup", opForget: "forget", opGetattr: "getattr", opSetattr: "setattr", opReadlink: "readlink",
	opSymlink: "symlink", opMknod: "mknod", opMkdir: "mkdir", opUnlink: "unlink", opRmdir: "rmdir",
	opRename: "rename", opLink: "link", opOpen: "open", opRead: "read", opWrite: "write", opStatfs: "statfs",
	opRelease: "release", opFsync: "fsync", opSetxattr: "setxattr", 22: "getxattr", 23: "listxattr",
	24: "removexattr", opFlush: "flush", opInit: "init", opOpendir: "opendir", opReaddir: "readdir",
	opReleasedir: "releasedir", opFsyncdir: "fsyncdir", 34: "access", opCreate: "create",
	opInterrupt: "interrupt", opDestroy: "destroy", opBatchForget: "batch_forget", 43: "fallocate",
	opReaddirplus: "readdirplus", opRename2: "rename2", 46: "lseek", 47: "copy_file_range", 50: "syncfs",
	51: "tmpfile", 52: "statx",
}

func opName(op uint32) string {
	if n, ok := opNames[op]; ok {
		return n
	}
	return "op" + strconv.Itoa(int(op))
}

// INIT flags.
const (
	initAsyncRead       = 1 << 0
	initAtomicOTrunc    = 1 << 3
	initBigWrites       = 1 << 5
	initAutoInvalData   = 1 << 12
	initDoReaddirplus   = 1 << 13
	initReaddirplusAuto = 1 << 14
	initWritebackCache  = 1 << 16
	initParallelDirops  = 1 << 18
	initMaxPages        = 1 << 22
	initCacheSymlinks   = 1 << 23

	wantInitFlags = initAsyncRead | initAtomicOTrunc | initBigWrites | initAutoInvalData | initDoReaddirplus |
		initReaddirplusAuto | initParallelDirops | initMaxPages | initCacheSymlinks
)

// MaxPages is how many pages one READ or WRITE may carry. Each page takes a
// descriptor, so with 256-entry queues two such requests fit at once.
const MaxPages = 128

const pageSize = 4096

// Open reply flags.
const fopenKeepCache = 1 << 1

// SETATTR valid bits.
const (
	SetMode     = 1 << 0
	SetUID      = 1 << 1
	SetGID      = 1 << 2
	SetSize     = 1 << 3
	SetAtime    = 1 << 4
	SetMtime    = 1 << 5
	SetHandle   = 1 << 6
	SetAtimeNow = 1 << 7
	SetMtimeNow = 1 << 8
	SetCtime    = 1 << 10
)

// RENAME2 flags.
const (
	RenameNoreplace = 1 << 0
	RenameExchange  = 1 << 1
)

// File type bits of a mode, as in st_mode.
const (
	ModeType   = 0o170000
	ModeSocket = 0o140000
	ModeLink   = 0o120000
	ModeFile   = 0o100000
	ModeBlock  = 0o060000
	ModeDir    = 0o040000
	ModeChar   = 0o020000
	ModeFIFO   = 0o010000
)

// Open flags the kernel passes, as in open(2).
const (
	OAccMode = 0o3
	ORdonly  = 0o0
	OWronly  = 0o1
	ORdwr    = 0o2
	OCreat   = 0o100
	OExcl    = 0o200
	OTrunc   = 0o1000
	OAppend  = 0o2000
)

// Sizes of the fixed parts of requests and replies.
const (
	inHeaderLen  = 40
	outHeaderLen = 16
	attrLen      = 88
	entryOutLen  = 40 + attrLen
	attrOutLen   = 16 + attrLen
	direntLen    = 24 // without the name
)

// Errno is a Linux error number.
type Errno int32

const (
	EPERM        Errno = 1
	ENOENT       Errno = 2
	EIO          Errno = 5
	EBADF        Errno = 9
	EACCES       Errno = 13
	EBUSY        Errno = 16
	EEXIST       Errno = 17
	EXDEV        Errno = 18
	ENOTDIR      Errno = 20
	EISDIR       Errno = 21
	EINVAL       Errno = 22
	EFBIG        Errno = 27
	ENOSPC       Errno = 28
	EROFS        Errno = 30
	EMLINK       Errno = 31
	ERANGE       Errno = 34
	ENAMETOOLONG Errno = 36
	ENOSYS       Errno = 38
	ENOTEMPTY    Errno = 39
	ELOOP        Errno = 40
	ENODATA      Errno = 61
	EPROTO       Errno = 71
	EOPNOTSUPP   Errno = 95
	ESTALE       Errno = 116
)

var errnoNames = map[Errno]string{
	EPERM: "EPERM", ENOENT: "ENOENT", EIO: "EIO", EBADF: "EBADF", EACCES: "EACCES", EBUSY: "EBUSY",
	EEXIST: "EEXIST", EXDEV: "EXDEV", ENOTDIR: "ENOTDIR", EISDIR: "EISDIR", EINVAL: "EINVAL", EFBIG: "EFBIG",
	ENOSPC: "ENOSPC", EROFS: "EROFS", EMLINK: "EMLINK", ERANGE: "ERANGE", ENAMETOOLONG: "ENAMETOOLONG",
	ENOSYS: "ENOSYS", ENOTEMPTY: "ENOTEMPTY", ELOOP: "ELOOP", ENODATA: "ENODATA", EPROTO: "EPROTO",
	EOPNOTSUPP: "EOPNOTSUPP", ESTALE: "ESTALE",
}

func (e Errno) Error() string {
	if n, ok := errnoNames[e]; ok {
		return n
	}
	return "errno " + strconv.Itoa(int(e))
}

// Context is who made a request: the calling process's fsuid, fsgid and pid
// in the guest.
type Context struct {
	UID, GID, PID uint32
}

// Attr is a file's attributes, as stat(2) reports them.
type Attr struct {
	Ino                 uint64
	Size                uint64
	Blocks              uint64 // in 512-byte units
	Atime, Mtime, Ctime time.Time
	Mode                uint32 // type and permission bits
	Nlink               uint32
	UID, GID            uint32
	Rdev                uint32
}

// Entry is a name's node and the node's attributes.
type Entry struct {
	Node uint64
	Attr Attr
}

// SetattrIn is what SETATTR asks to change. Valid says which fields are set.
type SetattrIn struct {
	Valid        uint32
	Handle       uint64
	Size         uint64
	Atime, Mtime time.Time
	Mode         uint32
	UID, GID     uint32
}

// DirEntry is one directory entry.
type DirEntry struct {
	Name string
	// Off is the offset of the entry after this one, which the kernel passes
	// back to continue the listing.
	Off uint64
	// Entry carries the entry's node and attributes. For READDIR only
	// Attr.Ino and the type bits of Attr.Mode are used. For READDIRPLUS a
	// Node of 0 tells the kernel not to keep the entry, as for "." and "..".
	Entry Entry
}

// Statfs is what statfs(2) reports.
type Statfs struct {
	Blocks, Bfree, Bavail uint64 // in units of Frsize
	Files, Ffree          uint64
	Bsize, Frsize         uint32
	Namelen               uint32
}

// reader decodes a request's arguments. Reading past the end sets bad and
// yields zeros.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || len(r.b) < n {
		r.bad = true
		return make([]byte, n)
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}

func (r *reader) u32() uint32 { return le.Uint32(r.take(4)) }
func (r *reader) u64() uint64 { return le.Uint64(r.take(8)) }
func (r *reader) skip(n int)  { r.take(n) }

// name reads a NUL-terminated string.
func (r *reader) name() string {
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.bad = true
	return ""
}

// writer encodes a reply's arguments.
type writer struct{ b []byte }

func (w *writer) u16(v uint16) { w.b = le.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = le.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = le.AppendUint64(w.b, v) }
func (w *writer) pad(n int)    { w.b = append(w.b, make([]byte, n)...) }

func splitTime(t time.Time) (uint64, uint32) {
	if t.IsZero() {
		return 0, 0
	}
	return uint64(t.Unix()), uint32(t.Nanosecond())
}

func (w *writer) attr(a *Attr) {
	w.u64(a.Ino)
	w.u64(a.Size)
	w.u64(a.Blocks)
	as, ans := splitTime(a.Atime)
	ms, mns := splitTime(a.Mtime)
	cs, cns := splitTime(a.Ctime)
	w.u64(as)
	w.u64(ms)
	w.u64(cs)
	w.u32(ans)
	w.u32(mns)
	w.u32(cns)
	w.u32(a.Mode)
	w.u32(a.Nlink)
	w.u32(a.UID)
	w.u32(a.GID)
	w.u32(a.Rdev)
	w.u32(pageSize) // blksize
	w.u32(0)        // flags
}

func splitDuration(d time.Duration) (uint64, uint32) {
	return uint64(d / time.Second), uint32(d % time.Second)
}

// entry encodes fuse_entry_out. A zero node with a timeout is a negative
// entry, which the kernel caches as "no such name".
func (w *writer) entry(e *Entry, entryTimeout, attrTimeout time.Duration) {
	es, ens := splitDuration(entryTimeout)
	as, ans := splitDuration(attrTimeout)
	w.u64(e.Node)
	w.u64(0) // generation
	w.u64(es)
	w.u64(as)
	w.u32(ens)
	w.u32(ans)
	w.attr(&e.Attr)
}

func (w *writer) attrOut(a *Attr, timeout time.Duration) {
	s, ns := splitDuration(timeout)
	w.u64(s)
	w.u32(ns)
	w.u32(0)
	w.attr(a)
}

// dirent appends a fuse_dirent, padded to 8 bytes.
func (w *writer) dirent(e *DirEntry) {
	w.u64(e.Entry.Attr.Ino)
	w.u64(e.Off)
	w.u32(uint32(len(e.Name)))
	w.u32(e.Entry.Attr.Mode & ModeType >> 12)
	w.b = append(w.b, e.Name...)
	w.pad(direntPad(len(e.Name)))
}

func direntPad(nameLen int) int { return (8 - nameLen%8) % 8 }

func direntSize(nameLen int, plus bool) int {
	n := direntLen + nameLen + direntPad(nameLen)
	if plus {
		n += entryOutLen
	}
	return n
}
