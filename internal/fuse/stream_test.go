package fuse

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockFS is a fakeFS that keeps whole-file write locks, with Setlk waiting
// as SETLKW asks, and extended attributes in a map.
type lockFS struct {
	fakeFS
	mu      sync.Mutex
	cond    *sync.Cond
	holder  map[uint64]uint64 // node -> owner
	aborted bool
	flushed []uint64
	xattrs  map[string][]byte
}

func newLockFS() *lockFS {
	f := &lockFS{holder: map[uint64]uint64{}, xattrs: map[string][]byte{"user.a": []byte("one")}}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *lockFS) Getlk(c Context, node, fh, owner uint64, lk Lock, flags uint32) (Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.holder[node]; ok && h != owner {
		return Lock{Start: 0, End: ^uint64(0), Type: LockWrite, PID: 0}, nil
	}
	return Lock{Type: LockUnlock}, nil
}

func (f *lockFS) Setlk(c Context, node, fh, owner uint64, lk Lock, flags uint32, wait bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lk.Type == LockUnlock {
		if f.holder[node] == owner {
			delete(f.holder, node)
			f.cond.Broadcast()
		}
		return nil
	}
	for {
		if f.aborted {
			return EIO
		}
		if h, ok := f.holder[node]; !ok || h == owner {
			f.holder[node] = owner
			return nil
		}
		if !wait {
			return EAGAIN
		}
		f.cond.Wait()
	}
}

func (f *lockFS) FlushOwner(node, fh, owner uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushed = append(f.flushed, owner)
	if f.holder[node] == owner {
		delete(f.holder, node)
		f.cond.Broadcast()
	}
}

func (f *lockFS) abort() {
	f.mu.Lock()
	f.aborted = true
	f.cond.Broadcast()
	f.mu.Unlock()
}

func (f *lockFS) Getxattr(c Context, node uint64, name string) ([]byte, error) {
	if v, ok := f.xattrs[name]; ok {
		return v, nil
	}
	return nil, ENODATA
}

func (f *lockFS) Listxattr(c Context, node uint64) ([]byte, error) {
	var b []byte
	for n := range f.xattrs {
		b = append(append(b, n...), 0)
	}
	return b, nil
}

func (f *lockFS) Removexattr(c Context, node uint64, name string) error {
	delete(f.xattrs, name)
	return nil
}

func TestInitLocks(t *testing.T) {
	all := uint32(0xffffffff)
	_, out, _ := serve(t, &Server{FS: &fakeFS{}}, request(opInit, 0, uint32(7), uint32(31), uint32(0), all))
	if f := le.Uint32(out[12:]); f&(initPosixLocks|initFlockLocks) != 0 {
		t.Errorf("flags %#x: locks offered by a file system that keeps none", f)
	}
	_, out, _ = serve(t, &Server{FS: newLockFS(), TimeGran: 1}, request(opInit, 0, uint32(7), uint32(31), uint32(0), all))
	if f := le.Uint32(out[12:]); f&(initPosixLocks|initFlockLocks) != initPosixLocks|initFlockLocks {
		t.Errorf("flags %#x: locks not offered", f)
	}
	if g := le.Uint32(out[24:]); g != 1 {
		t.Errorf("time_gran %d, want 1", g)
	}
}

func TestLocks(t *testing.T) {
	fs := newLockFS()
	s := &Server{FS: fs}
	lk := func(op uint32, owner uint64, typ uint32) []byte {
		return request(op, 5, uint64(7), owner, uint64(0), ^uint64(0), typ, uint32(77), uint32(0), uint32(0))
	}
	if errno, _, _ := serve(t, s, lk(opSetlk, 1, LockWrite)); errno != 0 {
		t.Fatalf("setlk: %v", errno)
	}
	if errno, _, _ := serve(t, s, lk(opSetlk, 2, LockWrite)); errno != EAGAIN {
		t.Fatalf("setlk of a held lock: %v, want EAGAIN", errno)
	}
	errno, out, _ := serve(t, s, lk(opGetlk, 2, LockWrite))
	if errno != 0 || len(out) != 24 || le.Uint32(out[16:]) != LockWrite || le.Uint64(out[8:]) != ^uint64(0) {
		t.Fatalf("getlk: %v %x", errno, out)
	}
	// FLUSH names its owner, whose locks go.
	if errno, _, _ := serve(t, s, request(opFlush, 5, uint64(7), uint32(0), uint32(0), uint64(1))); errno != 0 {
		t.Fatalf("flush: %v", errno)
	}
	if len(fs.flushed) != 1 || fs.flushed[0] != 1 {
		t.Fatalf("flushed owners %v, want [1]", fs.flushed)
	}
	if errno, _, _ := serve(t, s, lk(opSetlk, 2, LockWrite)); errno != 0 {
		t.Fatalf("setlk after the holder's flush: %v", errno)
	}
	// Without a Locker, lock requests get ENOSYS.
	if errno, _, _ := serve(t, &Server{FS: &fakeFS{}}, lk(opSetlk, 1, LockWrite)); errno != ENOSYS {
		t.Errorf("setlk without locks: %v", errno)
	}
}

func TestXattrs(t *testing.T) {
	s := &Server{FS: newLockFS()}
	errno, out, _ := serve(t, s, request(opGetxattr, 5, uint32(0), uint32(0), "user.a"))
	if errno != 0 || len(out) != 8 || le.Uint32(out) != 3 {
		t.Fatalf("size probe: %v %x", errno, out)
	}
	if errno, out, _ := serve(t, s, request(opGetxattr, 5, uint32(64), uint32(0), "user.a")); errno != 0 || string(out) != "one" {
		t.Fatalf("getxattr: %v %q", errno, out)
	}
	if errno, _, _ := serve(t, s, request(opGetxattr, 5, uint32(2), uint32(0), "user.a")); errno != ERANGE {
		t.Fatalf("getxattr into 2 bytes: %v, want ERANGE", errno)
	}
	if errno, _, _ := serve(t, s, request(opGetxattr, 5, uint32(64), uint32(0), "user.b")); errno != ENODATA {
		t.Fatalf("getxattr of a missing name: %v", errno)
	}
	if errno, out, _ := serve(t, s, request(opListxattr, 5, uint32(64), uint32(0))); errno != 0 || string(out) != "user.a\x00" {
		t.Fatalf("listxattr: %v %q", errno, out)
	}
	if errno, _, _ := serve(t, s, request(opRemovexattr, 5, "user.a")); errno != 0 {
		t.Fatalf("removexattr: %v", errno)
	}
	if errno, _, _ := serve(t, &Server{FS: &fakeFS{}}, request(opGetxattr, 5, uint32(0), uint32(0), "user.a")); errno != ENOSYS {
		t.Errorf("getxattr without Xattrs: %v", errno)
	}
}

func TestNoKeepCache(t *testing.T) {
	for _, no := range []bool{false, true} {
		_, out, _ := serve(t, &Server{FS: &fakeFS{}, NoKeepCache: no}, request(opOpen, 5, uint32(0), uint32(0)))
		if keep := le.Uint32(out[8:])&fopenKeepCache != 0; keep == no {
			t.Errorf("NoKeepCache %v: open flags %#x", no, le.Uint32(out[8:]))
		}
	}
}

// forward runs a Forwarder against ServeStream over an in-memory pipe.
func forward(t *testing.T, fs FileSystem, abort func()) (*Forwarder, net.Conn, chan error) {
	t.Helper()
	client, server := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, &Server{FS: fs}, abort) }()
	f := NewForwarder(client)
	t.Cleanup(func() { f.Close(); server.Close() })
	return f, server, served
}

// call sends req through f and waits for its reply.
func call(t *testing.T, f *Forwarder, req []byte) (Errno, []byte) {
	t.Helper()
	reply := [][]byte{make([]byte, outHeaderLen), make([]byte, 1<<20)}
	got := make(chan uint32, 1)
	f.ServeAsync(req, reply, func(n uint32) { got <- n })
	select {
	case n := <-got:
		all := bytes.Join(reply, nil)[:n]
		if n < outHeaderLen {
			t.Fatalf("reply of %d bytes", n)
		}
		return Errno(-int32(le.Uint32(all[4:]))), all[outHeaderLen:]
	case <-time.After(10 * time.Second):
		t.Fatal("no reply")
		return 0, nil
	}
}

func withUnique(req []byte, unique uint64) []byte {
	le.PutUint64(req[8:], unique)
	return req
}

func TestForwarder(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), MaxPages*pageSize/16)
	var forgot atomic.Uint64
	fs := &fakeFS{
		read: func(off int64, buf []byte) (int, error) { return copy(buf, data[off:]), nil },
		write: func(off int64, d []byte) (int, error) {
			if !bytes.Equal(d, data[:len(d)]) {
				return 0, EIO
			}
			return len(d), nil
		},
		forget: func(node, n uint64) { forgot.Add(n) },
	}
	f, _, _ := forward(t, fs, nil)
	if errno, out := call(t, f, request(opRead, 5, uint64(7), uint64(16), uint32(MaxPages*pageSize), uint32(0), uint64(0), uint32(0), uint32(0))); errno != 0 || !bytes.Equal(out, data[16:]) {
		t.Fatalf("read: %v, %d bytes", errno, len(out))
	}
	if errno, out := call(t, f, request(opWrite, 5, uint64(7), uint64(0), uint32(len(data)), uint32(0), uint64(0), uint32(0), uint32(0), data)); errno != 0 || le.Uint32(out) != uint32(len(data)) {
		t.Fatalf("write: %v %x", errno, out)
	}
	// FORGET gets no reply, and completes at once.
	done := make(chan uint32, 1)
	f.ServeAsync(request(opForget, 5, uint64(3)), [][]byte{make([]byte, 64)}, func(n uint32) { done <- n })
	if n := <-done; n != 0 {
		t.Fatalf("forget completed with %d bytes", n)
	}
	// The next request's reply comes after the forget was served.
	call(t, f, request(opStatfs, 1))
	if forgot.Load() != 3 {
		t.Fatalf("forgot %d lookups, want 3", forgot.Load())
	}
}

// Lock requests that wait hold up no other request, however many wait.
func TestForwarderWaiters(t *testing.T) {
	fs := newLockFS()
	f, _, _ := forward(t, fs, fs.abort)
	lk := func(op uint32, unique, owner uint64, typ uint32) []byte {
		return withUnique(request(op, 5, uint64(7), owner, uint64(0), ^uint64(0), typ, uint32(77), uint32(0), uint32(0)), unique)
	}
	if errno, _ := call(t, f, lk(opSetlk, 1, 1, LockWrite)); errno != 0 {
		t.Fatalf("setlk: %v", errno)
	}
	const waiters = maxServing + 50
	var got sync.WaitGroup
	var granted atomic.Int32
	for i := range waiters {
		got.Add(1)
		reply := [][]byte{make([]byte, 64)}
		f.ServeAsync(lk(opSetlkw, uint64(100+i), uint64(100+i), LockWrite), reply, func(n uint32) {
			if n >= outHeaderLen && le.Uint32(reply[0][4:]) == 0 {
				granted.Add(1)
				// Give the lock up again, for the next waiter.
				go func() {
					fs.Setlk(Context{}, 5, 7, uint64(100+i), Lock{Type: LockUnlock}, 0, false)
				}()
			}
			got.Done()
		})
	}
	// Plenty of waiters, and still a request is served.
	if errno, _ := call(t, f, withUnique(request(opStatfs, 1), 2)); errno != 0 {
		t.Fatalf("statfs while %d wait: %v", waiters, errno)
	}
	if errno, _ := call(t, f, lk(opSetlk, 3, 1, LockUnlock)); errno != 0 {
		t.Fatalf("unlock: %v", errno)
	}
	got.Wait()
	if granted.Load() != waiters {
		t.Fatalf("%d of %d waiters granted", granted.Load(), waiters)
	}
}

// Once the stream ends, every request waiting, and every later one, is
// answered EIO, and the server's waiters are let go.
func TestForwarderFails(t *testing.T) {
	fs := newLockFS()
	f, server, served := forward(t, fs, fs.abort)
	lk := func(op uint32, unique, owner uint64) []byte {
		return withUnique(request(op, 5, uint64(7), owner, uint64(0), ^uint64(0), uint32(LockWrite), uint32(77), uint32(0), uint32(0)), unique)
	}
	call(t, f, lk(opSetlk, 1, 1))
	reply := [][]byte{make([]byte, 64)}
	waited := make(chan uint32, 1)
	f.ServeAsync(lk(opSetlkw, 2, 2), reply, func(n uint32) { waited <- n })
	time.Sleep(50 * time.Millisecond)
	server.Close()
	select {
	case n := <-waited:
		if n != outHeaderLen || Errno(-int32(le.Uint32(reply[0][4:]))) != EIO {
			t.Fatalf("waiter answered %d bytes, errno %d", n, -int32(le.Uint32(reply[0][4:])))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter not answered")
	}
	if errno, _ := call(t, f, withUnique(request(opStatfs, 1), 3)); errno != EIO {
		t.Fatalf("request after the end: %v, want EIO", errno)
	}
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("ServeStream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStream did not return once its waiter was aborted")
	}
	if f.Failed.Load() != 2 {
		t.Errorf("%d failed, want 2", f.Failed.Load())
	}
}

func TestFrameTooLong(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, &Server{FS: &fakeFS{}}, nil) }()
	var h [frameHeaderLen]byte
	le.PutUint32(h[0:], MaxFrame+1)
	client.Write(h[:])
	if err := <-served; !errors.Is(err, errFrameTooLong) {
		t.Fatalf("ServeStream: %v, want errFrameTooLong", err)
	}
}

// killFS clears privileges when asked, counting the times.
type killFS struct {
	fakeFS
	kills int
}

func (f *killFS) KillSuidgid(c Context, node, fh uint64) error { f.kills++; return nil }

func TestKillSuidgid(t *testing.T) {
	fs := &killFS{fakeFS: fakeFS{write: func(off int64, d []byte) (int, error) { return len(d), nil }}}
	s := &Server{FS: fs}
	_, out, _ := serve(t, s, request(opInit, 0, uint32(7), uint32(31), uint32(0), uint32(0xffffffff)))
	if f := le.Uint32(out[12:]); f&initKillprivV2 == 0 {
		t.Fatalf("flags %#x: HANDLE_KILLPRIV_V2 not offered", f)
	}
	if _, out, _ = serve(t, &Server{FS: &fakeFS{}}, request(opInit, 0, uint32(7), uint32(31), uint32(0), uint32(0xffffffff))); le.Uint32(out[12:])&initKillprivV2 != 0 {
		t.Fatal("HANDLE_KILLPRIV_V2 offered by a file system that kills nothing")
	}
	write := func(flags uint32) {
		serve(t, s, request(opWrite, 5, uint64(7), uint64(0), uint32(1), flags, uint64(0), uint32(0), uint32(0), []byte("x")))
	}
	write(0)
	if fs.kills != 0 {
		t.Fatal("a write the kernel did not flag killed privileges")
	}
	write(writeKillSuidgid)
	if fs.kills != 1 {
		t.Fatalf("%d kills after a flagged write, want 1", fs.kills)
	}
	serve(t, s, request(opOpen, 5, uint32(OTrunc), uint32(openKillSuidgid)))
	if fs.kills != 2 {
		t.Fatalf("%d kills after a flagged open, want 2", fs.kills)
	}
}
