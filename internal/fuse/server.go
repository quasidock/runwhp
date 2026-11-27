package fuse

import (
	"cmp"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileSystem is what a Server serves. Node ids are the file system's own,
// except RootID, which names its root. Lookup, Symlink, Mknod, Mkdir, Link and
// Create, and every READDIRPLUS entry with a non-zero node, each count one
// lookup of the node they return; Forget takes lookups away again, and a node
// may be forgotten once its count reaches zero.
//
// Errors are reported to the guest as the Errno they wrap, or as EIO. Methods
// are called from several goroutines at once.
type FileSystem interface {
	Lookup(c Context, parent uint64, name string) (Entry, error)
	Forget(node, nlookup uint64)
	Getattr(c Context, node uint64) (Attr, error)
	Setattr(c Context, node uint64, in SetattrIn) (Attr, error)
	Readlink(c Context, node uint64) (string, error)
	Symlink(c Context, parent uint64, name, target string) (Entry, error)
	Mknod(c Context, parent uint64, name string, mode, rdev uint32) (Entry, error)
	Mkdir(c Context, parent uint64, name string, mode uint32) (Entry, error)
	Unlink(c Context, parent uint64, name string) error
	Rmdir(c Context, parent uint64, name string) error
	Rename(c Context, parent uint64, name string, newParent uint64, newName string, flags uint32) error
	Link(c Context, node, newParent uint64, newName string) (Entry, error)
	// Open opens a file for flags, as open(2) takes them; with O_TRUNC it
	// also truncates it.
	Open(c Context, node uint64, flags uint32) (fh uint64, err error)
	Create(c Context, parent uint64, name string, flags, mode uint32) (e Entry, fh uint64, err error)
	// Read reads at off into buf. Fewer bytes than len(buf) mean end of
	// file.
	Read(c Context, node, fh uint64, off int64, buf []byte) (int, error)
	Write(c Context, node, fh uint64, off int64, data []byte) (int, error)
	Flush(c Context, node, fh uint64) error
	Fsync(c Context, node, fh uint64, datasync bool) error
	Release(node, fh uint64)
	Opendir(c Context, node uint64) (fh uint64, err error)
	// Readdir lists entries from offset off, calling add for each until add
	// reports that the reply is full. off is 0 or an entry's Off.
	Readdir(c Context, node, fh, off uint64, plus bool, add func(DirEntry) bool) error
	Releasedir(node, fh uint64)
	Statfs(c Context, node uint64) (Statfs, error)
	Setxattr(c Context, node uint64, name string, value []byte, flags uint32) error
}

// Xattrs is implemented by file systems that keep extended attributes.
// Getxattr and Listxattr return the whole value, or the names, each ended by
// a NUL; the server answers the kernel's size probes and ERANGE from it.
// Without Xattrs, the kernel learns from ENOSYS not to ask again.
type Xattrs interface {
	Getxattr(c Context, node uint64, name string) ([]byte, error)
	Listxattr(c Context, node uint64) ([]byte, error)
	Removexattr(c Context, node uint64, name string) error
}

// Locker is implemented by file systems that keep POSIX and flock(2) locks
// themselves, so that every guest using them sees the same locks. Only then
// does the server offer the kernel to send it lock requests: once offered,
// the kernel keeps no locks of its own, and fails a lock request the file
// system cannot serve.
//
// owner is the kernel's lock owner: a process's files table for POSIX
// locks, or the open file for flock's, which come with LockFlock in flags.
// Setlk with wait blocks until the lock is granted; the kernel cannot
// interrupt it.
type Locker interface {
	Getlk(c Context, node, fh, owner uint64, lk Lock, flags uint32) (Lock, error)
	Setlk(c Context, node, fh, owner uint64, lk Lock, flags uint32, wait bool) error
	// FlushOwner drops owner's POSIX locks on node, as close(2) of any of
	// its descriptors for the file does. It is called on FLUSH.
	FlushOwner(node, fh, owner uint64)
}

// Allocator is implemented by file systems that take fallocate(2).
type Allocator interface {
	Fallocate(c Context, node, fh uint64, off, length uint64, mode uint32) error
}

// Seeker is implemented by file systems that answer lseek(2)'s SEEK_DATA
// and SEEK_HOLE.
type Seeker interface {
	Lseek(c Context, node, fh uint64, off uint64, whence uint32) (uint64, error)
}

// RangeCopier is implemented by file systems that copy between files
// themselves, as copy_file_range(2) asks.
type RangeCopier interface {
	CopyFileRange(c Context, nodeIn, fhIn, offIn, nodeOut, fhOut, offOut, length uint64, flags uint64) (uint64, error)
}

// Syncer is implemented by file systems that can write everything back at
// once, as syncfs(2) asks.
type Syncer interface {
	Syncfs(c Context) error
}

// Slower is implemented by file systems some of whose files may take long
// to reach, such as cloud files that Windows fetches from the network before
// they can be opened or read.
type Slower interface {
	// Slow reports whether requests about node may take long.
	Slow(node uint64) bool
}

// DefaultTimeout is how long the guest may cache names, attributes and the
// absence of names, unless Server.Timeout says otherwise. Changes made on the
// host show in the guest within this time.
const DefaultTimeout = time.Second

// Server decodes FUSE requests, calls its FileSystem, and encodes the
// replies. It implements virtio.FsHandler.
type Server struct {
	FS      FileSystem
	Timeout time.Duration
	// Logf, when set, reports protocol problems and errors the file system
	// returned that are not an Errno.
	Logf func(format string, a ...any)
	// Writeback takes the kernel's writeback cache, when it offers it: the
	// guest gathers writes in its page cache and sends them when files are
	// closed or synced, or as it writes back dirty pages. It also trusts its
	// own idea of each cached file's size and times, so host changes to
	// those files may go unseen. The file system must let files opened only
	// for writing be read: the kernel fills the pages it writes in part.
	Writeback bool
	// NoKeepCache drops a file's cached pages each time it is opened, so a
	// guest reads what other guests wrote before the open, rather than
	// trusting its cache until the file's size or mtime is seen to change.
	NoKeepCache bool
	// TimeGran is the granularity of the file system's times, in
	// nanoseconds. Zero means 100, as NTFS and ReFS keep them.
	TimeGran uint32

	counts  [maxOpcode]atomic.Uint64
	nanos   [maxOpcode]atomic.Int64 // time spent serving each opcode
	failed  atomic.Uint64
	readBuf sync.Pool
}

func (s *Server) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// keepCache returns the open flags that keep a file's cached pages.
func (s *Server) keepCache() uint32 {
	if s.NoKeepCache {
		return 0
	}
	return fopenKeepCache
}

func (s *Server) timeout() time.Duration {
	if s.Timeout == 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

// Serve handles one request and writes its reply into reply, returning the
// reply's length. Requests that get no reply return 0.
func (s *Server) Serve(req []byte, reply [][]byte) uint32 {
	if len(req) < inHeaderLen {
		s.logf("fuse: request of %d bytes is shorter than its header", len(req))
		return 0
	}
	h := &reader{b: req}
	total := h.u32()
	op := h.u32()
	unique := h.u64()
	node := h.u64()
	c := Context{UID: h.u32(), GID: h.u32(), PID: h.u32()}
	if int(total) >= inHeaderLen && int(total) < len(req) {
		req = req[:total]
	}
	r := &reader{b: req[inHeaderLen:]}
	if op < maxOpcode {
		s.counts[op].Add(1)
		start := time.Now()
		defer func() { s.nanos[op].Add(int64(time.Since(start))) }()
	}

	switch op {
	case opForget:
		n := r.u64()
		if !r.bad {
			s.FS.Forget(node, n)
		}
		return 0
	case opBatchForget:
		count := r.u32()
		r.skip(4)
		for i := uint32(0); i < count; i++ {
			id, n := r.u64(), r.u64()
			if r.bad {
				break
			}
			s.FS.Forget(id, n)
		}
		return 0
	case opInterrupt:
		return 0 // requests run to completion; there is nothing to interrupt
	case opRead:
		return s.read(c, node, unique, r, reply)
	}
	out, err := s.handle(c, op, node, r, capacity(reply)-outHeaderLen)
	if err == nil && r.bad {
		err = EINVAL
		s.logf("fuse: %s request too short", opName(op))
	}
	return s.reply(reply, unique, out, err, op)
}

// Inline reports whether req may be served on the vCPU that sent it, as
// virtio.InlineHandler asks: unless its node, the file or the directory it
// names, may be slow to reach, which the file system says if it implements
// Slower.
func (s *Server) Inline(req []byte) bool {
	sl, ok := s.FS.(Slower)
	if !ok || len(req) < inHeaderLen {
		return true
	}
	return !sl.Slow(le.Uint64(req[16:])) // fuse_in_header's nodeid
}

func (s *Server) handle(c Context, op uint32, node uint64, r *reader, room int) ([]byte, error) {
	w := &writer{}
	t := s.timeout()
	entry := func(e Entry, err error) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		w.entry(&e, t, t)
		return w.b, nil
	}
	switch op {
	case opInit:
		return s.init(r)
	case opDestroy, opFsyncdir:
		return nil, nil
	case opLookup:
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		e, err := s.FS.Lookup(c, node, name)
		if errors.Is(err, ENOENT) {
			// Let the guest remember that the name does not exist.
			w.entry(&Entry{}, t, 0)
			return w.b, nil
		}
		return entry(e, err)
	case opGetattr:
		a, err := s.FS.Getattr(c, node)
		if err != nil {
			return nil, err
		}
		w.attrOut(&a, t)
		return w.b, nil
	case opSetattr:
		in := SetattrIn{Valid: r.u32()}
		r.skip(4)
		in.Handle = r.u64()
		in.Size = r.u64()
		r.skip(8) // lock_owner
		atime, mtime := r.u64(), r.u64()
		r.skip(8) // ctime
		atimeNs, mtimeNs := r.u32(), r.u32()
		r.skip(4)
		in.Mode = r.u32()
		r.skip(4)
		in.UID, in.GID = r.u32(), r.u32()
		in.Atime = time.Unix(int64(atime), int64(atimeNs))
		in.Mtime = time.Unix(int64(mtime), int64(mtimeNs))
		if r.bad {
			return nil, EINVAL
		}
		a, err := s.FS.Setattr(c, node, in)
		if err != nil {
			return nil, err
		}
		w.attrOut(&a, t)
		return w.b, nil
	case opReadlink:
		target, err := s.FS.Readlink(c, node)
		return []byte(target), err
	case opSymlink:
		name := r.name()
		target := r.name()
		if r.bad {
			return nil, EINVAL
		}
		return entry(s.FS.Symlink(c, node, name, target))
	case opMknod:
		mode, rdev := r.u32(), r.u32()
		r.skip(8) // umask, padding
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		return entry(s.FS.Mknod(c, node, name, mode, rdev))
	case opMkdir:
		mode := r.u32()
		r.skip(4) // umask
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		return entry(s.FS.Mkdir(c, node, name, mode))
	case opUnlink, opRmdir:
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		if op == opUnlink {
			return nil, s.FS.Unlink(c, node, name)
		}
		return nil, s.FS.Rmdir(c, node, name)
	case opRename, opRename2:
		newDir := r.u64()
		var flags uint32
		if op == opRename2 {
			flags = r.u32()
			r.skip(4)
		}
		name, newName := r.name(), r.name()
		if r.bad {
			return nil, EINVAL
		}
		return nil, s.FS.Rename(c, node, name, newDir, newName, flags)
	case opLink:
		old := r.u64()
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		return entry(s.FS.Link(c, old, node, name))
	case opOpen, opOpendir:
		flags := r.u32()
		if r.bad {
			return nil, EINVAL
		}
		var fh uint64
		var err error
		var openFlags uint32
		if op == opOpen {
			fh, err = s.FS.Open(c, node, flags)
			// The page cache survives across opens; with AUTO_INVAL_DATA
			// the kernel drops it when the file's size or mtime changes.
			openFlags = s.keepCache()
		} else {
			fh, err = s.FS.Opendir(c, node)
		}
		if err != nil {
			return nil, err
		}
		w.u64(fh)
		w.u32(openFlags)
		w.u32(0)
		return w.b, nil
	case opCreate:
		flags, mode := r.u32(), r.u32()
		r.skip(8) // umask, open_flags
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		e, fh, err := s.FS.Create(c, node, name, flags, mode)
		if err != nil {
			return nil, err
		}
		w.entry(&e, t, t)
		w.u64(fh)
		w.u32(s.keepCache())
		w.u32(0)
		return w.b, nil
	case opWrite:
		fh, off, size := r.u64(), r.u64(), r.u32()
		r.skip(4 + 8 + 4 + 4) // write_flags, lock_owner, flags, padding
		if r.bad || int(size) > len(r.b) {
			return nil, EINVAL
		}
		n, err := s.FS.Write(c, node, fh, int64(off), r.b[:size])
		if err != nil {
			return nil, err
		}
		w.u32(uint32(n))
		w.u32(0)
		return w.b, nil
	case opStatfs:
		st, err := s.FS.Statfs(c, node)
		if err != nil {
			return nil, err
		}
		w.u64(st.Blocks)
		w.u64(st.Bfree)
		w.u64(st.Bavail)
		w.u64(st.Files)
		w.u64(st.Ffree)
		w.u32(st.Bsize)
		w.u32(st.Namelen)
		w.u32(st.Frsize)
		w.pad(4 + 24)
		return w.b, nil
	case opRelease, opReleasedir:
		// The open file's flock(2) locks, which RELEASE's flags mention,
		// go with its handle.
		fh := r.u64()
		if r.bad {
			return nil, EINVAL
		}
		if op == opRelease {
			s.FS.Release(node, fh)
		} else {
			s.FS.Releasedir(node, fh)
		}
		return nil, nil
	case opFlush:
		fh := r.u64()
		r.skip(8) // unused, padding
		owner := r.u64()
		if r.bad {
			return nil, EINVAL
		}
		if l, ok := s.FS.(Locker); ok {
			l.FlushOwner(node, fh, owner)
		}
		return nil, s.FS.Flush(c, node, fh)
	case opGetxattr, opListxattr:
		x, ok := s.FS.(Xattrs)
		if !ok {
			return nil, ENOSYS
		}
		size := r.u32()
		r.skip(4)
		var v []byte
		var err error
		if op == opGetxattr {
			name := r.name()
			if r.bad {
				return nil, EINVAL
			}
			v, err = x.Getxattr(c, node, name)
		} else {
			if r.bad {
				return nil, EINVAL
			}
			v, err = x.Listxattr(c, node)
		}
		if err != nil {
			return nil, err
		}
		if size == 0 {
			// A probe for the size: fuse_getxattr_out.
			w.u32(uint32(len(v)))
			w.u32(0)
			return w.b, nil
		}
		if len(v) > int(size) {
			return nil, ERANGE
		}
		return v, nil
	case opRemovexattr:
		x, ok := s.FS.(Xattrs)
		if !ok {
			return nil, ENOSYS
		}
		name := r.name()
		if r.bad {
			return nil, EINVAL
		}
		return nil, x.Removexattr(c, node, name)
	case opGetlk, opSetlk, opSetlkw:
		l, ok := s.FS.(Locker)
		if !ok {
			return nil, ENOSYS
		}
		fh, owner := r.u64(), r.u64()
		lk := Lock{Start: r.u64(), End: r.u64(), Type: r.u32(), PID: r.u32()}
		flags := r.u32()
		if r.bad {
			return nil, EINVAL
		}
		if op != opGetlk {
			return nil, l.Setlk(c, node, fh, owner, lk, flags, op == opSetlkw)
		}
		got, err := l.Getlk(c, node, fh, owner, lk, flags)
		if err != nil {
			return nil, err
		}
		w.u64(got.Start)
		w.u64(got.End)
		w.u32(got.Type)
		w.u32(got.PID)
		return w.b, nil
	case opFallocate:
		a, ok := s.FS.(Allocator)
		if !ok {
			return nil, ENOSYS
		}
		fh, off, length, mode := r.u64(), r.u64(), r.u64(), r.u32()
		if r.bad {
			return nil, EINVAL
		}
		return nil, a.Fallocate(c, node, fh, off, length, mode)
	case opLseek:
		sk, ok := s.FS.(Seeker)
		if !ok {
			return nil, ENOSYS
		}
		fh, off, whence := r.u64(), r.u64(), r.u32()
		if r.bad {
			return nil, EINVAL
		}
		got, err := sk.Lseek(c, node, fh, off, whence)
		if err != nil {
			return nil, err
		}
		w.u64(got)
		return w.b, nil
	case opCopyRange:
		rc, ok := s.FS.(RangeCopier)
		if !ok {
			return nil, ENOSYS
		}
		fhIn, offIn := r.u64(), r.u64()
		nodeOut, fhOut, offOut := r.u64(), r.u64(), r.u64()
		length, flags := r.u64(), r.u64()
		if r.bad {
			return nil, EINVAL
		}
		n, err := rc.CopyFileRange(c, node, fhIn, offIn, nodeOut, fhOut, offOut, length, flags)
		if err != nil {
			return nil, err
		}
		w.u32(uint32(n))
		w.u32(0)
		return w.b, nil
	case opSyncfs:
		sy, ok := s.FS.(Syncer)
		if !ok {
			return nil, ENOSYS
		}
		return nil, sy.Syncfs(c)
	case opFsync:
		fh, flags := r.u64(), r.u32()
		if r.bad {
			return nil, EINVAL
		}
		return nil, s.FS.Fsync(c, node, fh, flags&1 != 0)
	case opSetxattr:
		size := r.u32()
		flags := r.u32()
		name := r.name()
		if r.bad || int(size) > len(r.b) {
			return nil, EINVAL
		}
		return nil, s.FS.Setxattr(c, node, name, r.b[:size], flags)
	case opReaddir, opReaddirplus:
		fh, off, size := r.u64(), r.u64(), r.u32()
		if r.bad {
			return nil, EINVAL
		}
		limit := min(int(size), room)
		plus := op == opReaddirplus
		err := s.FS.Readdir(c, node, fh, off, plus, func(e DirEntry) bool {
			if len(w.b)+direntSize(len(e.Name), plus) > limit {
				return false
			}
			if plus {
				w.entry(&e.Entry, t, t)
			}
			w.dirent(&e)
			return true
		})
		if err != nil && len(w.b) == 0 {
			return nil, err
		}
		return w.b, nil
	}
	return nil, ENOSYS
}

func (s *Server) init(r *reader) ([]byte, error) {
	major, minor, readahead, flags := r.u32(), r.u32(), r.u32(), r.u32()
	if r.bad || major < kernelVersion {
		return nil, EPROTO
	}
	w := &writer{}
	w.u32(kernelVersion)
	w.u32(min(minor, minorVersion))
	w.u32(readahead)
	want := uint32(wantInitFlags)
	if s.Writeback {
		want |= initWritebackCache
	}
	if _, ok := s.FS.(Locker); ok {
		want |= initPosixLocks | initFlockLocks
	}
	w.u32(flags & want)
	w.u16(64)                  // max_background
	w.u16(48)                  // congestion_threshold
	w.u32(MaxPages * pageSize) // max_write
	w.u32(cmp.Or(s.TimeGran, 100))
	w.u16(MaxPages)
	w.u16(0) // map_alignment
	w.pad(64 - len(w.b))
	return w.b, nil
}

func (s *Server) read(c Context, node, unique uint64, r *reader, reply [][]byte) uint32 {
	fh, off, size := r.u64(), r.u64(), r.u32()
	if r.bad {
		return s.reply(reply, unique, nil, EINVAL, opRead)
	}
	n := min(int(size), MaxPages*pageSize, capacity(reply)-outHeaderLen)
	if n < 0 {
		n = 0
	}
	bp, _ := s.readBuf.Get().(*[]byte)
	if bp == nil || cap(*bp) < n {
		b := make([]byte, MaxPages*pageSize)
		bp = &b
	}
	defer s.readBuf.Put(bp)
	buf := (*bp)[:n]
	got, err := s.FS.Read(c, node, fh, int64(off), buf)
	return s.reply(reply, unique, buf[:max(got, 0)], err, opRead)
}

func (s *Server) reply(dst [][]byte, unique uint64, out []byte, err error, op uint32) uint32 {
	var errno Errno
	if err != nil {
		if !errors.As(err, &errno) {
			s.logf("fuse: %s: %v", opName(op), err)
			errno = EIO
		}
		s.failed.Add(1)
		out = nil
	}
	if outHeaderLen+len(out) > capacity(dst) {
		s.logf("fuse: %s reply of %d bytes does not fit in %d", opName(op), outHeaderLen+len(out), capacity(dst))
		errno, out = ERANGE, nil
		if capacity(dst) < outHeaderLen {
			return 0
		}
	}
	var h [outHeaderLen]byte
	le.PutUint32(h[0:], uint32(outHeaderLen+len(out)))
	le.PutUint32(h[4:], uint32(-int32(errno)))
	le.PutUint64(h[8:], unique)
	return scatter(dst, h[:], out)
}

func capacity(bufs [][]byte) int {
	n := 0
	for _, b := range bufs {
		n += len(b)
	}
	return n
}

// scatter copies parts, in order, across dst and returns how many bytes it
// copied.
func scatter(dst [][]byte, parts ...[]byte) uint32 {
	var n uint32
	i, at := 0, 0
	for _, p := range parts {
		for len(p) > 0 && i < len(dst) {
			c := copy(dst[i][at:], p)
			p = p[c:]
			at += c
			n += uint32(c)
			if at == len(dst[i]) {
				i, at = i+1, 0
			}
		}
	}
	return n
}

// Stats summarizes the requests served, those that took longest in all
// first: how many of each, and how long each took on average.
func (s *Server) Stats() string {
	type count struct {
		op    string
		n     uint64
		total time.Duration
	}
	var counts []count
	for op := range s.counts {
		if n := s.counts[op].Load(); n > 0 {
			counts = append(counts, count{opName(uint32(op)), n, time.Duration(s.nanos[op].Load())})
		}
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].total > counts[j].total })
	parts := make([]string, len(counts))
	for i, c := range counts {
		parts[i] = fmt.Sprintf("%s=%d/%dus", c.op, c.n, c.total.Microseconds()/int64(c.n))
	}
	return fmt.Sprintf("fuse requests (count/average): %s; %d failed", strings.Join(parts, " "), s.failed.Load())
}
