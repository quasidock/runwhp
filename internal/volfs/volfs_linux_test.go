package volfs

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/fuse"
)

// These tests make files as other users and take locks, as the server does
// for its guests: run them as root, in a container, with
// `go test -c` built for linux.

var root = fuse.Context{UID: 0, GID: 0, PID: 1}

func volume(t *testing.T) (*Volume, string) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root: run it in a container")
	}
	dir := t.TempDir()
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, dir
}

func session(t *testing.T, v *Volume) *Session {
	s := v.NewSession()
	t.Cleanup(s.Close)
	return s
}

func create(t *testing.T, s *Session, c fuse.Context, parent uint64, name string, mode uint32) (fuse.Entry, uint64) {
	t.Helper()
	e, fh, err := s.Create(c, parent, name, uint32(os.O_RDWR), mode)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return e, fh
}

func TestFiles(t *testing.T) {
	v, dir := volume(t)
	s := session(t, v)
	e, fh := create(t, s, root, fuse.RootID, "f", 0o640)
	if e.Node == fuse.RootID || e.Attr.Mode != fuse.ModeFile|0o640 {
		t.Fatalf("entry %+v", e)
	}
	if n, err := s.Write(root, e.Node, fh, 0, []byte("hello")); n != 5 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	buf := make([]byte, 16)
	if n, err := s.Read(root, e.Node, fh, 1, buf); n != 4 || err != nil || string(buf[:4]) != "ello" {
		t.Fatalf("read: %d %v %q", n, err, buf[:n])
	}
	got, err := os.ReadFile(dir + "/f")
	if err != nil || string(got) != "hello" {
		t.Fatalf("on disk: %q %v", got, err)
	}
	// The same file looked up again is the same node.
	again, err := s.Lookup(root, fuse.RootID, "f")
	if err != nil || again.Node != e.Node {
		t.Fatalf("lookup: %+v %v", again, err)
	}
	// Removed while open, it stays readable.
	if err := s.Unlink(root, fuse.RootID, "f"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Read(root, e.Node, fh, 0, buf); string(buf[:n]) != "hello" {
		t.Fatalf("read after unlink: %q", buf[:n])
	}
	if a, err := s.Getattr(root, e.Node); err != nil || a.Nlink != 0 {
		t.Fatalf("getattr after unlink: %+v %v", a, err)
	}
	if _, err := s.Lookup(root, fuse.RootID, "f"); !errors.Is(err, fuse.ENOENT) {
		t.Fatalf("lookup after unlink: %v", err)
	}
	s.Release(e.Node, fh)
	s.Forget(e.Node, 2)
	if _, err := s.Getattr(root, e.Node); !errors.Is(err, fuse.ESTALE) {
		t.Fatalf("getattr of a forgotten node: %v", err)
	}
}

// No name leads out of the volume.
func TestNames(t *testing.T) {
	v, _ := volume(t)
	s := session(t, v)
	for _, name := range []string{"..", ".", "a/b", "", "../etc"} {
		if _, err := s.Lookup(root, fuse.RootID, name); !errors.Is(err, fuse.EINVAL) {
			t.Errorf("lookup %q: %v, want EINVAL", name, err)
		}
		if _, _, err := s.Create(root, fuse.RootID, name, 0, 0o644); !errors.Is(err, fuse.EINVAL) {
			t.Errorf("create %q: %v, want EINVAL", name, err)
		}
	}
	// A symlink out of the volume is a symlink: the server never follows
	// it.
	e, err := s.Symlink(root, fuse.RootID, "out", "/etc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(root, e.Node, "passwd"); err == nil {
		t.Fatal("looked up a name through a symlink")
	}
	if _, err := s.Open(root, e.Node, uint32(os.O_RDONLY)); err == nil {
		t.Fatal("opened a symlink")
	}
	if target, err := s.Readlink(root, e.Node); err != nil || target != "/etc" {
		t.Fatalf("readlink: %q %v", target, err)
	}
	if _, err := s.Getxattr(root, e.Node, "user.x"); !errors.Is(err, fuse.EOPNOTSUPP) {
		t.Fatalf("getxattr of a symlink: %v", err)
	}
}

// What a caller makes is the caller's, and a setgid directory's group's.
func TestOwners(t *testing.T) {
	v, dir := volume(t)
	s := session(t, v)
	user := fuse.Context{UID: 1000, GID: 1000, PID: 2}
	d, err := s.Mkdir(root, fuse.RootID, "shared", 0o2777)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setattr(root, d.Node, fuse.SetattrIn{Valid: fuse.SetGID | fuse.SetMode, GID: 50, Mode: 0o2777}); err != nil {
		t.Fatal(err)
	}
	e, fh := create(t, s, user, d.Node, "mine", 0o600)
	s.Release(e.Node, fh)
	if e.Attr.UID != 1000 || e.Attr.GID != 50 {
		t.Fatalf("file made in a setgid directory: %d:%d, want 1000:50", e.Attr.UID, e.Attr.GID)
	}
	sub, err := s.Mkdir(user, d.Node, "sub", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Attr.UID != 1000 || sub.Attr.GID != 50 || sub.Attr.Mode&0o2000 == 0 {
		t.Fatalf("directory made in a setgid directory: %d:%d %o", sub.Attr.UID, sub.Attr.GID, sub.Attr.Mode)
	}
	// A caller may make files where only root could, as its kernel has
	// checked it may: here, root's own 0700 directory.
	priv, err := s.Mkdir(root, fuse.RootID, "priv", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mknod(user, priv.Node, "fifo", syscall.S_IFIFO|0o644, 0); err != nil {
		t.Fatalf("mknod as another user in root's directory: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dir+"/priv/fifo", &st); err != nil || st.Uid != 1000 {
		t.Fatalf("fifo: %+v %v", st, err)
	}
	// The thread that served the caller is root's again.
	if fsuid, _ := unix.SetfsuidRetUid(-1); fsuid != 0 {
		t.Fatalf("fsuid %d after serving another user", fsuid)
	}
	if _, err := s.Setattr(root, e.Node, fuse.SetattrIn{Valid: fuse.SetUID | fuse.SetGID, UID: 7, GID: 8}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Getattr(root, e.Node); a.UID != 7 || a.GID != 8 {
		t.Fatalf("chown: %d:%d", a.UID, a.GID)
	}
	when := time.Unix(1700000000, 5)
	if a, err := s.Setattr(root, e.Node, fuse.SetattrIn{Valid: fuse.SetMtime | fuse.SetAtime, Mtime: when, Atime: when}); err != nil || !a.Mtime.Equal(when) {
		t.Fatalf("utimes: %v %v", a.Mtime, err)
	}
}

// POSIX locks conflict between sessions, and go when their owner closes
// the file; flock's go with their open file; a waiter is woken when the
// lock it waits for goes.
func TestLocks(t *testing.T) {
	v, _ := volume(t)
	a, b := session(t, v), session(t, v)
	e, fha := create(t, a, root, fuse.RootID, "db", 0o644)
	eb, err := b.Lookup(root, fuse.RootID, "db")
	if err != nil {
		t.Fatal(err)
	}
	fhb, err := b.Open(root, eb.Node, uint32(os.O_RDWR))
	if err != nil {
		t.Fatal(err)
	}
	whole := fuse.Lock{Start: 0, End: ^uint64(0), Type: fuse.LockWrite}
	if err := a.Setlk(root, e.Node, fha, 1, whole, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Setlk(root, eb.Node, fhb, 1, whole, 0, false); !errors.Is(err, fuse.EAGAIN) {
		t.Fatalf("second session's lock: %v, want EAGAIN", err)
	}
	got, err := b.Getlk(root, eb.Node, fhb, 1, whole, 0)
	if err != nil || got.Type != fuse.LockWrite {
		t.Fatalf("getlk: %+v %v", got, err)
	}
	// A range that does not overlap is free.
	if err := b.Setlk(root, eb.Node, fhb, 1, fuse.Lock{Start: 0, End: 9, Type: fuse.LockRead}, 0, false); !errors.Is(err, fuse.EAGAIN) {
		t.Fatalf("overlapping read lock: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- b.Setlk(root, eb.Node, fhb, 1, whole, 0, true) }()
	select {
	case err := <-waited:
		t.Fatalf("waiter returned while the lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	a.FlushOwner(e.Node, fha, 1)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken when the owner's file was closed")
	}
	b.FlushOwner(eb.Node, fhb, 1)

	// flock: held by the open file, until it is released.
	if err := a.Setlk(root, e.Node, fha, 9, whole, fuse.LockFlock, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Setlk(root, eb.Node, fhb, 9, whole, fuse.LockFlock, false); !errors.Is(err, fuse.EAGAIN) {
		t.Fatalf("second flock: %v", err)
	}
	a.Release(e.Node, fha)
	if err := b.Setlk(root, eb.Node, fhb, 9, whole, fuse.LockFlock, false); err != nil {
		t.Fatalf("flock after release: %v", err)
	}
	// A session that ends lets go of its locks, and fails its waiters.
	fha, _ = a.Open(root, e.Node, uint32(os.O_RDWR))
	w := make(chan error, 1)
	go func() { w <- a.Setlk(root, e.Node, fha, 9, whole, fuse.LockFlock, true) }()
	time.Sleep(50 * time.Millisecond)
	a.Close()
	if err := <-w; !errors.Is(err, fuse.EIO) {
		t.Fatalf("waiter of a closed session: %v", err)
	}
}

func TestXattrsAndRanges(t *testing.T) {
	v, _ := volume(t)
	s := session(t, v)
	e, fh := create(t, s, root, fuse.RootID, "x", 0o644)
	if err := s.Setxattr(root, e.Node, "user.k", []byte("v"), 0); err != nil {
		t.Skipf("setxattr: %v (the test's file system keeps no user xattrs)", err)
	}
	if got, err := s.Getxattr(root, e.Node, "user.k"); err != nil || string(got) != "v" {
		t.Fatalf("getxattr: %q %v", got, err)
	}
	if got, err := s.Listxattr(root, e.Node); err != nil || !slices.Contains(strings.Split(string(got), "\x00"), "user.k") {
		t.Fatalf("listxattr: %q %v", got, err)
	}
	if err := s.Removexattr(root, e.Node, "user.k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Getxattr(root, e.Node, "user.k"); !errors.Is(err, fuse.ENODATA) {
		t.Fatalf("getxattr after remove: %v", err)
	}
	s.Write(root, e.Node, fh, 0, []byte("0123456789"))
	o, ofh := create(t, s, root, fuse.RootID, "y", 0o644)
	if n, err := s.CopyFileRange(root, e.Node, fh, 2, o.Node, ofh, 0, 5, 0); err != nil || n != 5 {
		t.Fatalf("copy_file_range: %d %v", n, err)
	}
	buf := make([]byte, 8)
	if n, _ := s.Read(root, o.Node, ofh, 0, buf); string(buf[:n]) != "23456" {
		t.Fatalf("copied %q", buf[:n])
	}
	if err := s.Rename(root, fuse.RootID, "x", fuse.RootID, "y", fuse.RenameNoreplace); !errors.Is(err, fuse.EEXIST) {
		t.Fatalf("rename noreplace: %v", err)
	}
	if err := s.Rename(root, fuse.RootID, "x", fuse.RootID, "y", fuse.RenameExchange); err != nil {
		t.Fatalf("rename exchange: %v", err)
	}
	if err := s.Fallocate(root, e.Node, fh, 0, 1<<20, 0); err != nil {
		t.Fatalf("fallocate: %v", err)
	}
	if err := s.Syncfs(root); err != nil {
		t.Fatalf("syncfs: %v", err)
	}
}

func TestReaddir(t *testing.T) {
	v, _ := volume(t)
	s := session(t, v)
	want := map[string]bool{}
	for i := range 300 {
		name := "file-with-a-long-name-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		e, fh := create(t, s, root, fuse.RootID, name, 0o644)
		s.Release(e.Node, fh)
		s.Forget(e.Node, 1)
		want[name] = true
	}
	fh, err := s.Opendir(root, fuse.RootID)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Releasedir(fuse.RootID, fh)
	seen := map[string]bool{}
	var off uint64
	for calls := 0; ; calls++ {
		n := 0
		err := s.Readdir(root, fuse.RootID, fh, off, true, func(e fuse.DirEntry) bool {
			if n == 7 { // a small reply, so the listing continues
				return false
			}
			n++
			if seen[e.Name] {
				t.Errorf("%s listed twice", e.Name)
			}
			seen[e.Name] = true
			if e.Name != "." && e.Name != ".." && e.Entry.Node == 0 {
				t.Errorf("%s has no node", e.Name)
			}
			off = e.Off
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if calls > 1000 {
			t.Fatal("listing does not end")
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s not listed", name)
		}
	}
	// Every node listed counts one lookup, and only those: forgetting
	// them leaves the root alone.
	s.mu.Lock()
	nodes := len(s.nodes)
	s.mu.Unlock()
	if nodes != 301 {
		t.Errorf("%d nodes after listing 300 files, want 301", nodes)
	}
}

func TestKillSuidgid(t *testing.T) {
	v, _ := volume(t)
	s := session(t, v)
	for _, tc := range []struct{ mode, want uint32 }{
		{0o6755, 0o755},  // setgid with group execute goes too
		{0o6644, 0o2644}, // setgid without it marks mandatory locking, and stays
		{0o755, 0o755},
	} {
		e, fh := create(t, s, root, fuse.RootID, fmt.Sprintf("f%o", tc.mode), 0o600)
		if _, err := s.Setattr(root, e.Node, fuse.SetattrIn{Valid: fuse.SetMode, Mode: tc.mode}); err != nil {
			t.Fatal(err)
		}
		if err := s.KillSuidgid(root, e.Node, fh); err != nil {
			t.Fatal(err)
		}
		if a, _ := s.Getattr(root, e.Node); a.Mode&0o7777 != tc.want {
			t.Errorf("mode %o after killing privileges: %o, want %o", tc.mode, a.Mode&0o7777, tc.want)
		}
	}
}
