//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestCopyTree needs root, for owners and device nodes: run it in a
// container.
func TestCopyTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src, dst := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Chown(src, 999, 998))
	must(os.Chmod(src, 0o750))
	must(os.MkdirAll(filepath.Join(src, "dir/sub"), 0o755))
	must(os.WriteFile(filepath.Join(src, "dir/file"), []byte("data"), 0o640))
	must(os.Chown(filepath.Join(src, "dir/file"), 1000, 1001))
	must(os.WriteFile(filepath.Join(src, "suid"), []byte("x"), 0o644))
	must(os.Chown(filepath.Join(src, "suid"), 1000, 1000))
	must(os.Chmod(filepath.Join(src, "suid"), 0o4755|os.ModeSetuid))
	must(unix.Chmod(filepath.Join(src, "suid"), 0o4755))
	must(os.Symlink("dir/file", filepath.Join(src, "link")))
	must(os.Lchown(filepath.Join(src, "link"), 1000, 1000))
	must(os.Link(filepath.Join(src, "dir/file"), filepath.Join(src, "hard")))
	must(unix.Mkfifo(filepath.Join(src, "fifo"), 0o600))
	must(unix.Mknod(filepath.Join(src, "null"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))))
	must(unix.Chmod(filepath.Join(src, "null"), 0o666)) // past the umask
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	must(os.Chtimes(filepath.Join(src, "dir/file"), old, old))
	must(os.Chtimes(filepath.Join(src, "dir"), old, old))
	xattrs := true
	if err := unix.Setxattr(filepath.Join(src, "dir/file"), "user.note", []byte("kept"), 0); err != nil {
		t.Logf("no user xattrs here: %v", err)
		xattrs = false
	}
	overlayXattr := unix.Setxattr(filepath.Join(src, "dir"), "trusted.overlay.origin", []byte("x"), 0) == nil
	sock, err := net.Listen("unix", filepath.Join(src, "sock"))
	must(err)
	defer sock.Close()

	must(copyTree(src, dst))

	stat := func(p string) unix.Stat_t {
		t.Helper()
		var st unix.Stat_t
		must(unix.Lstat(filepath.Join(dst, p), &st))
		return st
	}
	check := func(p string, mode uint32, uid, gid uint32) {
		t.Helper()
		st := stat(p)
		if st.Mode != mode || st.Uid != uid || st.Gid != gid {
			t.Errorf("%s: mode %o, owner %d:%d; want %o, %d:%d", p, st.Mode, st.Uid, st.Gid, mode, uid, gid)
		}
	}
	check(".", unix.S_IFDIR|0o750, 999, 998)
	check("dir", unix.S_IFDIR|0o755, 0, 0)
	check("dir/sub", unix.S_IFDIR|0o755, 0, 0)
	check("dir/file", unix.S_IFREG|0o640, 1000, 1001)
	check("suid", unix.S_IFREG|0o4755, 1000, 1000)
	check("fifo", unix.S_IFIFO|0o600, 0, 0)
	check("null", unix.S_IFCHR|0o666, 0, 0)
	if st := stat("link"); st.Mode&unix.S_IFMT != unix.S_IFLNK || st.Uid != 1000 {
		t.Errorf("link: mode %o, uid %d", st.Mode, st.Uid)
	}
	if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "dir/file" {
		t.Errorf("link points to %q", target)
	}
	if stat("hard").Ino != stat("dir/file").Ino {
		t.Error("the hard link was copied as a separate file")
	}
	if st := stat("null"); st.Rdev != unix.Mkdev(1, 3) {
		t.Errorf("null: device %#x", st.Rdev)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "dir/file")); string(data) != "data" {
		t.Errorf("file holds %q", data)
	}
	for _, p := range []string{"dir/file", "dir"} {
		if st := stat(p); time.Unix(st.Mtim.Unix()).UTC() != old {
			t.Errorf("%s: mtime %v, want %v", p, time.Unix(st.Mtim.Unix()).UTC(), old)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket was copied: %v", err)
	}
	if xattrs {
		buf := make([]byte, 16)
		if n, err := unix.Getxattr(filepath.Join(dst, "dir/file"), "user.note", buf); err != nil || string(buf[:n]) != "kept" {
			t.Errorf("user.note = %q, %v", buf[:n], err)
		}
	}
	if overlayXattr {
		if _, err := unix.Getxattr(filepath.Join(dst, "dir"), "trusted.overlay.origin", nil); err == nil {
			t.Error("overlayfs's own attribute was copied")
		}
	}
}

func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if empty, err := emptyDir(dir); err != nil || !empty {
		t.Errorf("emptyDir(new) = %v, %v", empty, err)
	}
	os.WriteFile(filepath.Join(dir, "f"), nil, 0o644)
	if empty, err := emptyDir(dir); err != nil || empty {
		t.Errorf("emptyDir(one file) = %v, %v", empty, err)
	}
}
