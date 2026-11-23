package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeFS answers ENOSYS to everything its function fields leave out.
type fakeFS struct {
	lookup  func(parent uint64, name string) (Entry, error)
	forget  func(node, n uint64)
	read    func(off int64, buf []byte) (int, error)
	write   func(off int64, data []byte) (int, error)
	readdir func(off uint64, plus bool, add func(DirEntry) bool) error
	setattr func(in SetattrIn) (Attr, error)
}

func (f *fakeFS) Lookup(c Context, parent uint64, name string) (Entry, error) {
	if f.lookup != nil {
		return f.lookup(parent, name)
	}
	return Entry{}, ENOSYS
}
func (f *fakeFS) Forget(node, n uint64) {
	if f.forget != nil {
		f.forget(node, n)
	}
}
func (f *fakeFS) Getattr(c Context, node uint64) (Attr, error) {
	return Attr{}, errors.New("host on fire")
}
func (f *fakeFS) Setattr(c Context, node uint64, in SetattrIn) (Attr, error) {
	if f.setattr != nil {
		return f.setattr(in)
	}
	return Attr{}, ENOSYS
}
func (f *fakeFS) Readlink(c Context, node uint64) (string, error) { return "../target", nil }
func (f *fakeFS) Symlink(c Context, parent uint64, name, target string) (Entry, error) {
	return Entry{}, ENOSYS
}
func (f *fakeFS) Mknod(c Context, parent uint64, name string, mode, rdev uint32) (Entry, error) {
	return Entry{}, ENOSYS
}
func (f *fakeFS) Mkdir(c Context, parent uint64, name string, mode uint32) (Entry, error) {
	return Entry{}, ENOSYS
}
func (f *fakeFS) Unlink(c Context, parent uint64, name string) error { return ENOSYS }
func (f *fakeFS) Rmdir(c Context, parent uint64, name string) error  { return ENOSYS }
func (f *fakeFS) Rename(c Context, parent uint64, name string, newParent uint64, newName string, flags uint32) error {
	return fmt.Errorf("rename %d/%s -> %d/%s flags %d: %w", parent, name, newParent, newName, flags, EXDEV)
}
func (f *fakeFS) Link(c Context, node, newParent uint64, newName string) (Entry, error) {
	return Entry{}, ENOSYS
}
func (f *fakeFS) Open(c Context, node uint64, flags uint32) (uint64, error) { return 7, nil }
func (f *fakeFS) Create(c Context, parent uint64, name string, flags, mode uint32) (Entry, uint64, error) {
	return Entry{}, 0, ENOSYS
}
func (f *fakeFS) Read(c Context, node, fh uint64, off int64, buf []byte) (int, error) {
	return f.read(off, buf)
}
func (f *fakeFS) Write(c Context, node, fh uint64, off int64, data []byte) (int, error) {
	return f.write(off, data)
}
func (f *fakeFS) Flush(c Context, node, fh uint64) error                { return nil }
func (f *fakeFS) Fsync(c Context, node, fh uint64, datasync bool) error { return nil }
func (f *fakeFS) Release(node, fh uint64)                               {}
func (f *fakeFS) Opendir(c Context, node uint64) (uint64, error)        { return 9, nil }
func (f *fakeFS) Readdir(c Context, node, fh, off uint64, plus bool, add func(DirEntry) bool) error {
	return f.readdir(off, plus, add)
}
func (f *fakeFS) Releasedir(node, fh uint64) {}
func (f *fakeFS) Statfs(c Context, node uint64) (Statfs, error) {
	return Statfs{Blocks: 100, Bfree: 50, Bavail: 40, Bsize: 4096, Frsize: 4096, Namelen: 255}, nil
}
func (f *fakeFS) Setxattr(c Context, node uint64, name string, value []byte, flags uint32) error {
	return EOPNOTSUPP
}

// request builds a FUSE request.
func request(op uint32, node uint64, args ...any) []byte {
	w := &writer{}
	w.u32(0) // length, filled in below
	w.u32(op)
	w.u64(42) // unique
	w.u64(node)
	w.u32(1000)
	w.u32(1001)
	w.u32(77)
	w.u32(0)
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			w.u32(v)
		case uint64:
			w.u64(v)
		case string: // a NUL-terminated name
			w.b = append(append(w.b, v...), 0)
		case []byte:
			w.b = append(w.b, v...)
		default:
			panic(fmt.Sprintf("bad argument %T", a))
		}
	}
	le.PutUint32(w.b, uint32(len(w.b)))
	return w.b
}

// serve runs one request with a reply area split into pieces of the given
// sizes, and returns the reply joined, its error and its payload.
func serve(t *testing.T, s *Server, req []byte, pieces ...int) (errno Errno, payload []byte, n uint32) {
	t.Helper()
	if len(pieces) == 0 {
		pieces = []int{outHeaderLen, 1 << 20}
	}
	var bufs [][]byte
	for _, p := range pieces {
		bufs = append(bufs, make([]byte, p))
	}
	n = s.Serve(req, bufs)
	if n == 0 {
		return 0, nil, 0
	}
	all := bytes.Join(bufs, nil)[:n]
	if got := le.Uint32(all[0:]); got != n {
		t.Fatalf("reply header says %d bytes, Serve returned %d", got, n)
	}
	if u := le.Uint64(all[8:]); u != 42 {
		t.Fatalf("reply unique %d, want 42", u)
	}
	return Errno(-int32(le.Uint32(all[4:]))), all[outHeaderLen:], n
}

func TestInit(t *testing.T) {
	s := &Server{FS: &fakeFS{}}
	for _, tc := range []struct{ kernelMinor, want uint32 }{{45, minorVersion}, {27, 27}} {
		errno, out, _ := serve(t, s, request(opInit, 0, uint32(7), tc.kernelMinor, uint32(128<<10), uint32(0xffffffff)))
		if errno != 0 || len(out) != 64 {
			t.Fatalf("init: errno %v, %d bytes", errno, len(out))
		}
		if le.Uint32(out[0:]) != 7 || le.Uint32(out[4:]) != tc.want {
			t.Errorf("kernel 7.%d: replied %d.%d, want 7.%d", tc.kernelMinor, le.Uint32(out[0:]), le.Uint32(out[4:]), tc.want)
		}
		if le.Uint32(out[8:]) != 128<<10 {
			t.Errorf("max_readahead %d", le.Uint32(out[8:]))
		}
		if f := le.Uint32(out[12:]); f != wantInitFlags {
			t.Errorf("flags %#x, want %#x", f, wantInitFlags)
		}
		if le.Uint32(out[20:]) != MaxPages*pageSize || le.Uint32(out[24:]) != 100 || le.Uint16(out[28:]) != MaxPages {
			t.Errorf("max_write %d, time_gran %d, max_pages %d", le.Uint32(out[20:]), le.Uint32(out[24:]), le.Uint16(out[28:]))
		}
	}
	// Only flags the kernel offers are accepted.
	_, out, _ := serve(t, s, request(opInit, 0, uint32(7), uint32(45), uint32(0), uint32(initAsyncRead|1<<1)))
	if f := le.Uint32(out[12:]); f != initAsyncRead {
		t.Errorf("flags %#x, want only ASYNC_READ", f)
	}
	if errno, _, _ := serve(t, s, request(opInit, 0, uint32(6), uint32(0), uint32(0), uint32(0))); errno != EPROTO {
		t.Errorf("protocol 6: %v, want EPROTO", errno)
	}
}

func TestLookup(t *testing.T) {
	mtime := time.Unix(1700000000, 123456789)
	fs := &fakeFS{lookup: func(parent uint64, name string) (Entry, error) {
		if parent != RootID {
			t.Errorf("parent %d", parent)
		}
		if name == "there" {
			return Entry{Node: 5, Attr: Attr{Ino: 99, Size: 1234, Mode: ModeFile | 0o644, Nlink: 1, UID: 1000, GID: 1000, Mtime: mtime}}, nil
		}
		return Entry{}, ENOENT
	}}
	s := &Server{FS: fs, Timeout: 1500 * time.Millisecond}
	errno, out, _ := serve(t, s, request(opLookup, RootID, "there"))
	if errno != 0 || len(out) != entryOutLen {
		t.Fatalf("errno %v, %d bytes", errno, len(out))
	}
	if le.Uint64(out[0:]) != 5 || le.Uint64(out[16:]) != 1 || le.Uint64(out[24:]) != 1 || le.Uint32(out[32:]) != 500e6 {
		t.Errorf("entry_out head % x", out[:40])
	}
	a := out[40:]
	if le.Uint64(a[0:]) != 99 || le.Uint64(a[8:]) != 1234 || le.Uint64(a[32:]) != 1700000000 || le.Uint32(a[52:]) != 123456789 {
		t.Errorf("attr ino/size/mtime wrong: % x", a)
	}
	if le.Uint32(a[60:]) != ModeFile|0o644 || le.Uint32(a[64:]) != 1 || le.Uint32(a[68:]) != 1000 || le.Uint32(a[80:]) != pageSize {
		t.Errorf("attr mode/nlink/uid/blksize wrong: % x", a)
	}

	// A missing name is a negative entry the kernel can cache.
	errno, out, _ = serve(t, s, request(opLookup, RootID, "missing"))
	if errno != 0 || le.Uint64(out[0:]) != 0 || le.Uint64(out[16:]) != 1 {
		t.Errorf("negative lookup: errno %v, nodeid %d, entry_valid %d", errno, le.Uint64(out[0:]), le.Uint64(out[16:]))
	}
}

func TestErrors(t *testing.T) {
	s := &Server{FS: &fakeFS{}}
	if errno, out, _ := serve(t, s, request(46, RootID)); errno != ENOSYS || len(out) != 0 {
		t.Errorf("lseek: %v", errno)
	}
	if errno, _, _ := serve(t, s, request(opGetattr, RootID, uint32(0), uint32(0), uint64(0))); errno != EIO {
		t.Errorf("an error that is not an Errno: %v, want EIO", errno)
	}
	if errno, _, _ := serve(t, s, request(opRename2, 3, uint64(4), uint32(RenameNoreplace), uint32(0), "a", "b")); errno != EXDEV {
		t.Errorf("a wrapped Errno: %v, want EXDEV", errno)
	}
	if errno, _, _ := serve(t, s, request(opLookup, RootID)); errno != EINVAL {
		t.Errorf("lookup without a name: %v, want EINVAL", errno)
	}
	// A reply that does not fit.
	if errno, _, _ := serve(t, s, request(opReadlink, RootID), outHeaderLen, 3); errno != ERANGE {
		t.Errorf("oversized reply: %v, want ERANGE", errno)
	}
	if n := s.Serve([]byte{1, 2, 3}, [][]byte{make([]byte, 64)}); n != 0 {
		t.Errorf("a truncated header got a %d-byte reply", n)
	}
}

func TestForget(t *testing.T) {
	got := map[uint64]uint64{}
	s := &Server{FS: &fakeFS{forget: func(node, n uint64) { got[node] += n }}}
	if _, _, n := serve(t, s, request(opForget, 5, uint64(3))); n != 0 {
		t.Errorf("forget got a reply")
	}
	if _, _, n := serve(t, s, request(opBatchForget, 0, uint32(2), uint32(0), uint64(6), uint64(1), uint64(5), uint64(2))); n != 0 {
		t.Errorf("batch_forget got a reply")
	}
	if got[5] != 5 || got[6] != 1 {
		t.Errorf("forgotten: %v", got)
	}
}

func TestReadWrite(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 1000)
	fs := &fakeFS{
		read: func(off int64, buf []byte) (int, error) {
			if off >= int64(len(data)) {
				return 0, nil
			}
			return copy(buf, data[off:]), nil
		},
		write: func(off int64, p []byte) (int, error) {
			if off != 100 || string(p) != "hello" {
				t.Errorf("write %q at %d", p, off)
			}
			return len(p), nil
		},
	}
	s := &Server{FS: fs}
	// A read whose reply spans a header buffer and several pages, as the
	// guest lays it out.
	errno, out, _ := serve(t, s, request(opRead, 5, uint64(7), uint64(10), uint32(8192), uint32(0), uint64(0), uint32(0), uint32(0)),
		outHeaderLen, 4096, 4096)
	if errno != 0 || !bytes.Equal(out, data[10:10+8192]) {
		t.Errorf("read: errno %v, %d bytes", errno, len(out))
	}
	// A read past the end.
	errno, out, _ = serve(t, s, request(opRead, 5, uint64(7), uint64(9990), uint32(4096), uint32(0), uint64(0), uint32(0), uint32(0)))
	if errno != 0 || string(out) != "0123456789" {
		t.Errorf("short read: errno %v, %q", errno, out)
	}
	errno, out, _ = serve(t, s, request(opWrite, 5, uint64(7), uint64(100), uint32(5), uint32(0), uint64(0), uint32(0), uint32(0), []byte("hello")))
	if errno != 0 || le.Uint32(out) != 5 {
		t.Errorf("write: errno %v, % x", errno, out)
	}
	errno, _, _ = serve(t, s, request(opWrite, 5, uint64(7), uint64(100), uint32(50), uint32(0), uint64(0), uint32(0), uint32(0), []byte("hello")))
	if errno != EINVAL {
		t.Errorf("write with less data than its size: %v", errno)
	}
}

func TestReaddirplus(t *testing.T) {
	names := []string{".", "..", "a", "bb", "a-much-longer-name"}
	accepted := 0
	fs := &fakeFS{readdir: func(off uint64, plus bool, add func(DirEntry) bool) error {
		for i := off; i < uint64(len(names)); i++ {
			e := DirEntry{Name: names[i], Off: i + 1, Entry: Entry{Node: 10 + i, Attr: Attr{Ino: 100 + i, Mode: ModeFile}}}
			if i < 2 {
				e.Entry.Node = 0
			}
			if !add(e) {
				return nil
			}
			accepted++
		}
		return nil
	}}
	s := &Server{FS: fs}
	// Room for three entries with short names.
	size := uint32(3 * direntSize(2, true))
	errno, out, _ := serve(t, s, request(opReaddirplus, RootID, uint64(9), uint64(0), size, uint32(0), uint64(0), uint32(0), uint32(0)))
	if errno != 0 || accepted != 3 || len(out) != int(size) {
		t.Fatalf("errno %v, accepted %d, %d bytes", errno, accepted, len(out))
	}
	var got []string
	for len(out) > 0 {
		d := out[entryOutLen:]
		n := le.Uint32(d[16:])
		got = append(got, string(d[direntLen:direntLen+n]))
		if le.Uint32(d[20:]) != ModeFile>>12 {
			t.Errorf("type %d", le.Uint32(d[20:]))
		}
		out = out[direntSize(int(n), true):]
	}
	if fmt.Sprint(got) != "[. .. a]" {
		t.Errorf("entries %v", got)
	}

	// READDIR from an offset, without the entries' attributes.
	accepted = 0
	errno, out, _ = serve(t, s, request(opReaddir, RootID, uint64(9), uint64(3), uint32(4096), uint32(0), uint64(0), uint32(0), uint32(0)))
	if errno != 0 || accepted != 2 || len(out) != direntSize(2, false)+direntSize(len(names[4]), false) {
		t.Fatalf("readdir: errno %v, accepted %d, %d bytes", errno, accepted, len(out))
	}
	if le.Uint64(out[0:]) != 103 || le.Uint64(out[8:]) != 4 || string(out[24:26]) != "bb" {
		t.Errorf("first dirent % x", out[:32])
	}
}

func TestSetattr(t *testing.T) {
	var got SetattrIn
	s := &Server{FS: &fakeFS{setattr: func(in SetattrIn) (Attr, error) {
		got = in
		return Attr{Mode: ModeFile | 0o600}, nil
	}}}
	req := request(opSetattr, 5,
		uint32(SetMode|SetSize|SetMtime), uint32(0), uint64(7), uint64(4096), uint64(0),
		uint64(0), uint64(1700000000), uint64(0), uint32(0), uint32(5), uint32(0),
		uint32(0o600), uint32(0), uint32(1000), uint32(1000), uint32(0))
	errno, out, _ := serve(t, s, req)
	if errno != 0 || len(out) != attrOutLen {
		t.Fatalf("errno %v, %d bytes", errno, len(out))
	}
	if got.Valid != SetMode|SetSize|SetMtime || got.Size != 4096 || got.Mode != 0o600 || got.UID != 1000 ||
		!got.Mtime.Equal(time.Unix(1700000000, 5)) || got.Handle != 7 {
		t.Errorf("decoded %+v", got)
	}
}

func TestScatter(t *testing.T) {
	dst := [][]byte{make([]byte, 3), make([]byte, 2), make([]byte, 10)}
	if n := scatter(dst, []byte("abcd"), []byte("efg")); n != 7 {
		t.Fatalf("copied %d", n)
	}
	if string(dst[0])+string(dst[1])+string(dst[2][:2]) != "abcdefg" {
		t.Errorf("%q", dst)
	}
}

// slowFS is a fakeFS some of whose nodes are slow.
type slowFS struct {
	fakeFS
	slow map[uint64]bool
}

func (f *slowFS) Slow(node uint64) bool { return f.slow[node] }

func TestInline(t *testing.T) {
	s := &Server{FS: &fakeFS{}}
	if !s.Inline(request(opRead, 5, uint64(7), uint64(0), uint32(4096))) {
		t.Error("a file system that never says a node is slow had a request refused")
	}
	s = &Server{FS: &slowFS{slow: map[uint64]bool{5: true}}}
	if s.Inline(request(opRead, 5, uint64(7), uint64(0), uint32(4096))) {
		t.Error("a read of a slow node may be served on the vCPU")
	}
	if s.Inline(request(opLookup, 5, "child")) {
		t.Error("a lookup in a slow directory may be served on the vCPU")
	}
	if !s.Inline(request(opGetattr, 6)) {
		t.Error("a request about a node that is not slow was refused")
	}
	if !s.Inline([]byte{1, 2, 3}) {
		t.Error("a request too short to name a node was refused; Serve answers those")
	}
}

// The writeback cache is taken only when the server is told to, and only
// if the kernel offers it.
func TestInitWriteback(t *testing.T) {
	for _, tc := range []struct {
		writeback bool
		offered   uint32
		want      uint32
	}{
		{false, 0xffffffff, wantInitFlags},
		{true, 0xffffffff, wantInitFlags | initWritebackCache},
		{true, wantInitFlags, wantInitFlags},
	} {
		s := &Server{FS: &fakeFS{}, Writeback: tc.writeback}
		_, out, _ := serve(t, s, request(opInit, 0, uint32(7), uint32(45), uint32(0), tc.offered))
		if f := le.Uint32(out[12:]); f != tc.want {
			t.Errorf("writeback %v, kernel offering %#x: flags %#x, want %#x", tc.writeback, tc.offered, f, tc.want)
		}
	}
}
