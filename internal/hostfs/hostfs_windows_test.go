//go:build windows

package hostfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
)

var ctx = fuse.Context{UID: 1000, GID: 1001}

// testDir is a new empty directory, under RUNWHP_TEST_DIR when it is set, so
// the tests can run on another volume, such as one formatted with ReFS.
func testDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(base, "hostfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFS(t *testing.T, shares ...Share) *FS {
	t.Helper()
	fs, err := New(shares, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}

// walk looks up a slash-separated path from the root of all shares.
func walk(t *testing.T, fs *FS, p string) fuse.Entry {
	t.Helper()
	e := fuse.Entry{Node: fuse.RootID}
	for _, name := range strings.Split(p, "/") {
		var err error
		if e, err = fs.Lookup(ctx, e.Node, name); err != nil {
			t.Fatalf("lookup %s (%s): %v", p, name, err)
		}
	}
	return e
}

func wantErr(t *testing.T, what string, err error, want fuse.Errno) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

func readAll(t *testing.T, fs *FS, node uint64) string {
	t.Helper()
	fh, err := fs.Open(ctx, node, fuse.ORdonly)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer fs.Release(node, fh)
	buf := make([]byte, 1<<16)
	n, err := fs.Read(ctx, node, fh, 0, buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

func listDir(t *testing.T, fs *FS, node uint64, plus bool) []fuse.DirEntry {
	t.Helper()
	fh, err := fs.Opendir(ctx, node)
	if err != nil {
		t.Fatalf("opendir: %v", err)
	}
	defer fs.Releasedir(node, fh)
	var out []fuse.DirEntry
	err = fs.Readdir(ctx, node, fh, 0, plus, func(e fuse.DirEntry) bool { out = append(out, e); return true })
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	return out
}

func TestLookupAndAttributes(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "a.txt"), "hello")
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	fs := newFS(t, Share{Path: dir, Target: "/src"})

	root, err := fs.Getattr(ctx, fuse.RootID)
	if err != nil || root.Mode != fuse.ModeDir|0o555 {
		t.Errorf("root: %o, %v", root.Mode, err)
	}
	share := walk(t, fs, "0")
	if share.Attr.Mode != fuse.ModeDir|defaultDirMode {
		t.Errorf("share root mode %o", share.Attr.Mode)
	}
	a := walk(t, fs, "0/a.txt")
	if a.Attr.Size != 5 || a.Attr.Mode != fuse.ModeFile|defaultFileMode || a.Attr.UID != 0 || a.Attr.Mtime.IsZero() {
		t.Errorf("a.txt: %+v", a.Attr)
	}
	if got := readAll(t, fs, a.Node); got != "hello" {
		t.Errorf("read %q", got)
	}

	// The owner init sets applies to files without one of their own.
	if err := fs.Setxattr(ctx, fuse.RootID, guestspec.OwnerXattr, []byte("1000:1001"), 0); err != nil {
		t.Fatal(err)
	}
	if attr, _ := fs.Getattr(ctx, a.Node); attr.UID != 1000 || attr.GID != 1001 {
		t.Errorf("owner %d:%d after the handshake", attr.UID, attr.GID)
	}
	wantErr(t, "owner on a share", fs.Setxattr(ctx, share.Node, guestspec.OwnerXattr, []byte("1:1"), 0), fuse.EOPNOTSUPP)
	wantErr(t, "another xattr", fs.Setxattr(ctx, fuse.RootID, "user.x", []byte("1"), 0), fuse.EOPNOTSUPP)
	wantErr(t, "bad owner", fs.Setxattr(ctx, fuse.RootID, guestspec.OwnerXattr, []byte("me"), 0), fuse.EINVAL)

	// Another spelling of the name is the same node, as on the host.
	if A := walk(t, fs, "0/A.TXT"); A.Node != a.Node || A.Attr.Ino != a.Attr.Ino {
		t.Errorf("A.TXT is node %d, a.txt is %d", A.Node, a.Node)
	}
	for _, name := range []string{"1", "00", "-1", "x"} {
		_, err := fs.Lookup(ctx, fuse.RootID, name)
		wantErr(t, "share "+name, err, fuse.ENOENT)
	}
	_, err = fs.Lookup(ctx, share.Node, "missing")
	wantErr(t, "missing", err, fuse.ENOENT)
	for _, name := range []string{"..", ".", "a/b", ""} {
		_, err := fs.Lookup(ctx, share.Node, name)
		wantErr(t, "name "+name, err, fuse.EINVAL)
	}
	_, err = fs.Lookup(ctx, a.Node, "x")
	wantErr(t, "lookup in a file", err, fuse.ENOTDIR)
	_, err = fs.Getattr(ctx, 9999)
	wantErr(t, "unknown node", err, fuse.ESTALE)

	st, err := fs.Statfs(ctx, share.Node)
	if err != nil || st.Blocks == 0 || st.Bavail == 0 || st.Namelen != 255 {
		t.Errorf("statfs %+v, %v", st, err)
	}
}

func TestReaddir(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "f"), "x")
	write(t, filepath.Join(dir, "a"+esc(':')+"b"), "")
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	fs := newFS(t, Share{Path: dir}, Share{Path: dir, ReadOnly: true})

	roots := listDir(t, fs, fuse.RootID, true)
	if len(roots) != 4 || roots[2].Name != "0" || roots[3].Name != "1" || roots[3].Entry.Node == 0 {
		t.Fatalf("root listing %+v", roots)
	}
	share := walk(t, fs, "0")
	ents := listDir(t, fs, share.Node, true)
	var names []string
	for i, e := range ents {
		names = append(names, e.Name)
		if e.Off != uint64(i+1) {
			t.Errorf("%s: offset %d", e.Name, e.Off)
		}
		if e.Name == "d" && e.Entry.Attr.Mode&fuse.ModeType != fuse.ModeDir {
			t.Errorf("d has mode %o", e.Entry.Attr.Mode)
		}
		if (e.Name == "." || e.Name == "..") != (e.Entry.Node == 0) {
			t.Errorf("%s has node %d", e.Name, e.Entry.Node)
		}
	}
	sort.Strings(names)
	if strings.Join(names, " ") != ". .. a:b d f" {
		t.Errorf("names %q", names)
	}
	// Entries carry the nodes a lookup would return.
	f := walk(t, fs, "0/f")
	for _, e := range ents {
		if e.Name == "f" && e.Entry.Node != f.Node {
			t.Errorf("readdirplus gave f node %d, lookup %d", e.Entry.Node, f.Node)
		}
	}

	// An entry that does not fit is not counted as looked up, and the next
	// read starts from it.
	fh, _ := fs.Opendir(ctx, share.Node)
	defer fs.Releasedir(share.Node, fh)
	var got []fuse.DirEntry
	fs.Readdir(ctx, share.Node, fh, 0, true, func(e fuse.DirEntry) bool {
		if len(got) == 3 {
			return false
		}
		got = append(got, e)
		return true
	})
	var rest []fuse.DirEntry
	fs.Readdir(ctx, share.Node, fh, got[2].Off, false, func(e fuse.DirEntry) bool { rest = append(rest, e); return true })
	if len(rest) != 2 || rest[0].Off != 4 {
		t.Errorf("continued listing %+v", rest)
	}
	fs.mu.Lock()
	nodes := len(fs.nodes)
	fs.mu.Unlock()
	// Root, two shares, f (from lookup and listing), a:b and d.
	if nodes != 6 {
		t.Errorf("%d nodes", nodes)
	}
}

func TestCreateWriteAndSetattr(t *testing.T) {
	dir := testDir(t)
	fs := newFS(t, Share{Path: dir})
	share := walk(t, fs, "0")

	e, fh, err := fs.Create(ctx, share.Node, "new", fuse.ORdwr|fuse.OCreat|fuse.OExcl, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if e.Attr.Mode != fuse.ModeFile|0o640 || e.Attr.UID != 1000 || e.Attr.GID != 1001 {
		t.Errorf("new file %o %d:%d", e.Attr.Mode, e.Attr.UID, e.Attr.GID)
	}
	if n, err := fs.Write(ctx, e.Node, fh, 0, []byte("0123456789")); n != 10 || err != nil {
		t.Fatalf("write %d, %v", n, err)
	}
	if n, err := fs.Write(ctx, e.Node, fh, 1<<20, []byte("end")); n != 3 || err != nil {
		t.Fatalf("write far out %d, %v", n, err)
	}
	buf := make([]byte, 4)
	if n, _ := fs.Read(ctx, e.Node, fh, 6, buf); string(buf[:n]) != "6789" {
		t.Errorf("read %q", buf[:n])
	}
	if n, _ := fs.Read(ctx, e.Node, fh, 1<<20+1, buf); string(buf[:n]) != "nd" {
		t.Errorf("read at the end %q", buf[:n])
	}
	if err := fs.Fsync(ctx, e.Node, fh, false); err != nil {
		t.Error(err)
	}
	fs.Release(e.Node, fh)
	_, _, err = fs.Create(ctx, share.Node, "new", fuse.OWronly|fuse.OCreat|fuse.OExcl, 0o644)
	wantErr(t, "O_EXCL on an existing file", err, fuse.EEXIST)

	// Truncate and set times.
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 600, time.UTC)
	a, err := fs.Setattr(ctx, e.Node, fuse.SetattrIn{Valid: fuse.SetSize | fuse.SetMtime, Size: 4, Mtime: mtime})
	if err != nil || a.Size != 4 || !a.Mtime.Equal(mtime) {
		t.Errorf("setattr: size %d, mtime %v, %v", a.Size, a.Mtime, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "new")); string(data) != "0123" {
		t.Errorf("host sees %q", data)
	}

	// O_TRUNC through open and through create.
	fh, err = fs.Open(ctx, e.Node, fuse.OWronly|fuse.OTrunc)
	if err != nil {
		t.Fatal(err)
	}
	fs.Release(e.Node, fh)
	if st, _ := os.Stat(filepath.Join(dir, "new")); st.Size() != 0 {
		t.Errorf("open with O_TRUNC left %d bytes", st.Size())
	}
	write(t, filepath.Join(dir, "new"), "again")
	_, fh, err = fs.Create(ctx, share.Node, "new", fuse.OWronly|fuse.OCreat|fuse.OTrunc, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fs.Release(e.Node, fh)
	if st, _ := os.Stat(filepath.Join(dir, "new")); st.Size() != 0 {
		t.Errorf("create with O_TRUNC left %d bytes", st.Size())
	}
}

func TestMetadataPersists(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "script.sh"), "#!/bin/sh\n")
	fs := newFS(t, Share{Path: dir})
	f := walk(t, fs, "0/script.sh")
	if !fs.shares[0].eas {
		t.Skip("the volume has no extended attributes")
	}

	a, err := fs.Setattr(ctx, f.Node, fuse.SetattrIn{Valid: fuse.SetMode, Mode: 0o700})
	if err != nil || a.Mode != fuse.ModeFile|0o700 {
		t.Fatalf("chmod: %o, %v", a.Mode, err)
	}
	a, err = fs.Setattr(ctx, f.Node, fuse.SetattrIn{Valid: fuse.SetUID | fuse.SetGID, UID: 999, GID: 998})
	if err != nil || a.UID != 999 || a.GID != 998 || a.Mode != fuse.ModeFile|0o700 {
		t.Fatalf("chown: %d:%d %o, %v", a.UID, a.GID, a.Mode, err)
	}

	// A new server, as in the next run, sees the same.
	fs2 := newFS(t, Share{Path: dir})
	if a := walk(t, fs2, "0/script.sh").Attr; a.Mode != fuse.ModeFile|0o700 || a.UID != 999 || a.GID != 998 {
		t.Errorf("next run sees %o %d:%d", a.Mode, a.UID, a.GID)
	}
	for _, e := range listDir(t, fs2, walk(t, fs2, "0").Node, true) {
		if e.Name == "script.sh" && (e.Entry.Attr.Mode != fuse.ModeFile|0o700 || e.Entry.Attr.UID != 999) {
			t.Errorf("readdirplus sees %o %d", e.Entry.Attr.Mode, e.Entry.Attr.UID)
		}
	}

	// Taking away the owner's write bit makes the file read-only for
	// Windows too, and giving it back undoes that.
	path := filepath.Join(dir, "script.sh")
	if _, err := fs.Setattr(ctx, f.Node, fuse.SetattrIn{Valid: fuse.SetMode, Mode: 0o444}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o200 != 0 {
		t.Error("chmod 444 did not set the read-only attribute")
	}
	if _, err := fs.Setattr(ctx, f.Node, fuse.SetattrIn{Valid: fuse.SetMode, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o200 == 0 {
		t.Error("chmod 644 did not clear the read-only attribute")
	}

	// A host file marked read-only shows without write bits.
	write(t, filepath.Join(dir, "ro"), "")
	os.Chmod(filepath.Join(dir, "ro"), 0o444)
	if a := walk(t, fs, "0/ro").Attr; a.Mode != fuse.ModeFile|0o555 {
		t.Errorf("read-only host file shows as %o", a.Mode)
	}
}

func TestDelete(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "f"), "data")
	write(t, filepath.Join(dir, "full", "x"), "")
	os.Mkdir(filepath.Join(dir, "empty"), 0o755)
	fs := newFS(t, Share{Path: dir})
	share := walk(t, fs, "0")

	// A file can be removed while open, and stays readable through the
	// open handle, as on Linux.
	f := walk(t, fs, "0/f")
	fh, err := fs.Open(ctx, f.Node, fuse.ORdonly)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Unlink(ctx, share.Node, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
		t.Errorf("f still there: %v", err)
	}
	buf := make([]byte, 8)
	if n, err := fs.Read(ctx, f.Node, fh, 0, buf); string(buf[:n]) != "data" {
		t.Errorf("read after unlink %q, %v", buf[:n], err)
	}
	fs.Release(f.Node, fh)
	write(t, filepath.Join(dir, "f"), "new")
	if f2 := walk(t, fs, "0/f"); f2.Node == f.Node {
		t.Error("a new file with the old name got the old node")
	}

	wantErr(t, "unlink a directory", fs.Unlink(ctx, share.Node, "empty"), fuse.EISDIR)
	wantErr(t, "rmdir a file", fs.Rmdir(ctx, share.Node, "f"), fuse.ENOTDIR)
	wantErr(t, "rmdir non-empty", fs.Rmdir(ctx, share.Node, "full"), fuse.ENOTEMPTY)
	wantErr(t, "unlink missing", fs.Unlink(ctx, share.Node, "missing"), fuse.ENOENT)
	if err := fs.Rmdir(ctx, share.Node, "empty"); err != nil {
		t.Error(err)
	}

	// Read-only host files can be removed, as on Linux, where only the
	// directory's permissions matter.
	write(t, filepath.Join(dir, "ro"), "")
	os.Chmod(filepath.Join(dir, "ro"), 0o444)
	if err := fs.Unlink(ctx, share.Node, "ro"); err != nil {
		t.Errorf("unlink a read-only file: %v", err)
	}
}

func TestRenameAndLink(t *testing.T) {
	dir := testDir(t)
	other := testDir(t)
	write(t, filepath.Join(dir, "a"), "A")
	write(t, filepath.Join(dir, "b"), "B")
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Mkdir(filepath.Join(dir, "d1"), 0o755)
	os.Mkdir(filepath.Join(dir, "d2"), 0o755)
	fs := newFS(t, Share{Path: dir}, Share{Path: other})
	share := walk(t, fs, "0")
	sub := walk(t, fs, "0/sub")
	a := walk(t, fs, "0/a")

	wantErr(t, "rename without replacing", fs.Rename(ctx, share.Node, "a", share.Node, "b", fuse.RenameNoreplace), fuse.EEXIST)
	if err := fs.Rename(ctx, share.Node, "a", share.Node, "b", 0); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "b")); string(data) != "A" {
		t.Errorf("b holds %q", data)
	}
	// The node follows the file.
	if err := fs.Rename(ctx, share.Node, "b", sub.Node, "moved", 0); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fs, a.Node); got != "A" {
		t.Errorf("renamed node reads %q", got)
	}
	wantErr(t, "rename across shares", fs.Rename(ctx, sub.Node, "moved", walk(t, fs, "1").Node, "x", 0), fuse.EXDEV)
	wantErr(t, "exchange", fs.Rename(ctx, share.Node, "d1", share.Node, "d2", fuse.RenameExchange), fuse.EINVAL)
	// Linux replaces an empty directory.
	if err := fs.Rename(ctx, share.Node, "d1", share.Node, "d2", 0); err != nil {
		t.Errorf("rename over an empty directory: %v", err)
	}
	// A change of case only.
	if err := fs.Rename(ctx, share.Node, "sub", share.Node, "SUB", 0); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); !strings.Contains(names(ents), "SUB") {
		t.Errorf("after the case change: %s", names(ents))
	}

	l, err := fs.Link(ctx, a.Node, share.Node, "hard")
	if err != nil {
		t.Fatal(err)
	}
	if l.Node != a.Node || l.Attr.Nlink != 2 {
		t.Errorf("link: node %d (want %d), nlink %d", l.Node, a.Node, l.Attr.Nlink)
	}
}

func names(ents []os.DirEntry) string {
	var s []string
	for _, e := range ents {
		s = append(s, e.Name())
	}
	return strings.Join(s, " ")
}

func TestSymlinksAndSpecialFiles(t *testing.T) {
	dir := testDir(t)
	fs := newFS(t, Share{Path: dir, Target: "/src"})
	share := walk(t, fs, "0")

	target := "../some where/ünïcode"
	l, err := fs.Symlink(ctx, share.Node, "link", target)
	if err != nil {
		t.Fatal(err)
	}
	if l.Attr.Mode != fuse.ModeLink|0o777 || l.Attr.UID != 1000 {
		t.Errorf("symlink attrs %o %d", l.Attr.Mode, l.Attr.UID)
	}
	if got, err := fs.Readlink(ctx, l.Node); got != target || err != nil {
		t.Errorf("readlink %q, %v", got, err)
	}
	// On the host it is a WSL symlink, which Windows does not follow.
	if st, err := statPath(ntPath(filepath.Join(dir, "link"))); err != nil || st.tag != tagLxSymlink {
		t.Errorf("host sees tag %#x, %v", st.tag, err)
	}
	_, err = fs.Lookup(ctx, l.Node, "x")
	wantErr(t, "lookup through a symlink", err, fuse.ENOTDIR)

	if !fs.shares[0].eas {
		t.Skip("the volume has no extended attributes")
	}
	fifo, err := fs.Mknod(ctx, share.Node, "fifo", fuse.ModeFIFO|0o600, 0)
	if err != nil || fifo.Attr.Mode != fuse.ModeFIFO|0o600 {
		t.Errorf("fifo: %o, %v", fifo.Attr.Mode, err)
	}
	sock, err := fs.Mknod(ctx, share.Node, "sock", fuse.ModeSocket|0o755, 0)
	if err != nil || sock.Attr.Mode != fuse.ModeSocket|0o755 {
		t.Errorf("socket: %o, %v", sock.Attr.Mode, err)
	}
	dev := encodeDev(4, 300)
	chr, err := fs.Mknod(ctx, share.Node, "tty", fuse.ModeChar|0o620, dev)
	if err != nil || chr.Attr.Mode != fuse.ModeChar|0o620 || chr.Attr.Rdev != dev {
		t.Errorf("char device: %o %#x (want %#x), %v", chr.Attr.Mode, chr.Attr.Rdev, dev, err)
	}
	reg, err := fs.Mknod(ctx, share.Node, "plain", fuse.ModeFile|0o600, 0)
	if err != nil || reg.Attr.Mode != fuse.ModeFile|0o600 {
		t.Errorf("mknod of a file: %o, %v", reg.Attr.Mode, err)
	}
	_, err = fs.Open(ctx, fifo.Node, fuse.ORdonly)
	wantErr(t, "open a fifo on the host", err, fuse.EINVAL)
}

// junction makes a Windows junction at link pointing to target, which no
// privilege is needed for.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := utf16.Encode([]rune(`\??\` + target))
	print := utf16.Encode([]rune(target))
	var data []byte
	data = le.AppendUint16(data, 0)
	data = le.AppendUint16(data, uint16(2*len(sub)))
	data = le.AppendUint16(data, uint16(2*len(sub)+2))
	data = le.AppendUint16(data, uint16(2*len(print)))
	for _, c := range sub {
		data = le.AppendUint16(data, c)
	}
	data = append(data, 0, 0)
	for _, c := range print {
		data = le.AppendUint16(data, c)
	}
	data = append(data, 0, 0)
	h, _, err := open(ntPath(link), windows.FILE_WRITE_DATA, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := setReparse(h, tagMountPoint, data); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLinksAreNotFollowed(t *testing.T) {
	dir := testDir(t)
	outside := testDir(t)
	write(t, filepath.Join(outside, "secret"), "do not read")
	os.Mkdir(filepath.Join(dir, "inner"), 0o755)
	junction(t, filepath.Join(dir, "out"), outside)
	junction(t, filepath.Join(dir, "in"), filepath.Join(dir, "inner"))
	fs := newFS(t, Share{Path: dir, Target: "/src"})

	out := walk(t, fs, "0/out")
	if out.Attr.Mode&fuse.ModeType != fuse.ModeLink {
		t.Fatalf("junction shows as %o", out.Attr.Mode)
	}
	if got, _ := fs.Readlink(ctx, out.Node); got != strings.ReplaceAll(outside, `\`, "/") {
		t.Errorf("junction out of the share reads %q", got)
	}
	_, err := fs.Lookup(ctx, out.Node, "secret")
	wantErr(t, "lookup through a junction", err, fuse.ENOTDIR)
	_, err = fs.Opendir(ctx, out.Node)
	wantErr(t, "opendir of a junction", err, fuse.ENOTDIR)
	in := walk(t, fs, "0/in")
	if got, _ := fs.Readlink(ctx, in.Node); got != "/src/inner" {
		t.Errorf("junction within the share reads %q", got)
	}

	// Windows symlinks, where this machine lets us make them.
	if err := os.Symlink(`..\up`, filepath.Join(dir, "rel")); err == nil {
		if got, _ := fs.Readlink(ctx, walk(t, fs, "0/rel").Node); got != "../up" {
			t.Errorf("relative Windows symlink reads %q", got)
		}
	} else {
		t.Logf("no Windows symlinks here: %v", err)
	}
}

func TestReadOnlyShare(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "f"), "data")
	fs := newFS(t, Share{Path: dir, ReadOnly: true})
	share := walk(t, fs, "0")
	f := walk(t, fs, "0/f")

	_, _, err := fs.Create(ctx, share.Node, "new", fuse.OWronly|fuse.OCreat, 0o644)
	wantErr(t, "create", err, fuse.EROFS)
	_, err = fs.Mkdir(ctx, share.Node, "d", 0o755)
	wantErr(t, "mkdir", err, fuse.EROFS)
	wantErr(t, "unlink", fs.Unlink(ctx, share.Node, "f"), fuse.EROFS)
	wantErr(t, "rename", fs.Rename(ctx, share.Node, "f", share.Node, "g", 0), fuse.EROFS)
	_, err = fs.Setattr(ctx, f.Node, fuse.SetattrIn{Valid: fuse.SetSize})
	wantErr(t, "truncate", err, fuse.EROFS)
	_, err = fs.Open(ctx, f.Node, fuse.ORdwr)
	wantErr(t, "open for writing", err, fuse.EROFS)
	_, err = fs.Symlink(ctx, share.Node, "l", "f")
	wantErr(t, "symlink", err, fuse.EROFS)
	if got := readAll(t, fs, f.Node); got != "data" {
		t.Errorf("read %q", got)
	}
	_, err = fs.Mkdir(ctx, fuse.RootID, "x", 0o755)
	wantErr(t, "mkdir in the root of shares", err, fuse.EROFS)
}

func TestFileShare(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "app.toml")
	write(t, path, "key = 1\n")
	fs := newFS(t, Share{Path: path, File: true})
	f := walk(t, fs, "0")
	if f.Attr.Mode&fuse.ModeType != fuse.ModeFile || f.Attr.Size != 8 {
		t.Errorf("file share %o, %d bytes", f.Attr.Mode, f.Attr.Size)
	}
	if got := readAll(t, fs, f.Node); got != "key = 1\n" {
		t.Errorf("read %q", got)
	}
	_, err := fs.Lookup(ctx, f.Node, "x")
	wantErr(t, "lookup in a file share", err, fuse.ENOTDIR)
	fh, err := fs.Open(ctx, f.Node, fuse.OWronly|fuse.OTrunc)
	if err != nil {
		t.Fatal(err)
	}
	fs.Write(ctx, f.Node, fh, 0, []byte("key = 2\n"))
	fs.Release(f.Node, fh)
	if data, _ := os.ReadFile(path); !bytes.Equal(data, []byte("key = 2\n")) {
		t.Errorf("host sees %q", data)
	}

	if _, err := New([]Share{{Path: dir, File: true}}, nil); err == nil {
		t.Error("a directory was accepted as a file share")
	}
	if _, err := New([]Share{{Path: path}}, nil); err == nil {
		t.Error("a file was accepted as a directory share")
	}
}

func TestForget(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "d", "f"), "")
	fs := newFS(t, Share{Path: dir})
	f := walk(t, fs, "0/d/f")
	d := walk(t, fs, "0/d")
	count := func() int {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.nodes)
	}
	if count() != 4 {
		t.Fatalf("%d nodes", count())
	}
	// d is looked up twice, and is f's parent: it stays until both are
	// forgotten and f is gone.
	fs.Forget(d.Node, 2)
	if count() != 4 {
		t.Errorf("d dropped while f still refers to it")
	}
	fs.Forget(f.Node, 1)
	if count() != 2 {
		t.Errorf("%d nodes after forgetting everything", count())
	}
	fs.Forget(12345, 1) // unknown nodes are ignored
}

// A file whose data Windows may have to fetch from elsewhere before it can
// be opened or read is slow, from the stat that found it. Only the cloud
// filter sets the recall attributes, but anyone may mark a file offline.
func TestSlowFiles(t *testing.T) {
	dir := testDir(t)
	write(t, filepath.Join(dir, "cloud"), "data")
	write(t, filepath.Join(dir, "local"), "data")
	fs := newFS(t, Share{Path: dir, Target: "/s"})
	local := walk(t, fs, "0/local")
	if fs.Slow(local.Node) {
		t.Error("an ordinary file is slow")
	}
	cloud := walk(t, fs, "0/cloud")
	if fs.Slow(cloud.Node) {
		t.Fatal("the file is slow before it is marked offline")
	}
	p, err := windows.UTF16PtrFromString(filepath.Join(dir, "cloud"))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_OFFLINE); err != nil {
		t.Fatal(err)
	}
	defer windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_NORMAL)
	if _, err := fs.Getattr(ctx, cloud.Node); err != nil {
		t.Fatal(err)
	}
	if !fs.Slow(cloud.Node) {
		t.Error("a file marked offline is not slow after a getattr")
	}
	if !fs.Slow(walk(t, fs, "0/cloud").Node) {
		t.Error("a file marked offline is not slow after a lookup")
	}
	if fs.Slow(12345) {
		t.Error("an unknown node is slow")
	}
}
