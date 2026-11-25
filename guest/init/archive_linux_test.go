//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moby/go-archive"
	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// These tests check init's archives against go-archive's, which Docker's
// daemon uses. They need root, for owners, devices and trusted xattrs:
// run them in a privileged container.

func needRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root: run it in a privileged container")
	}
}

// tmpfsDir is a new tmpfs, which takes trusted xattrs and whiteouts, as
// the overlay a test may run on does not.
func tmpfsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "mode=755"); err != nil {
		t.Skipf("mounting tmpfs: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dir, syscall.MNT_DETACH) })
	return dir
}

// makeTree makes files of every kind below dir.
func makeTree(t *testing.T, dir string) {
	t.Helper()
	old := time.Date(2021, 5, 6, 7, 8, 9, 123456789, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "d/sub"), 0o750))
	must(os.WriteFile(filepath.Join(dir, "d/f1"), []byte("f1\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "d/sub/f2"), bytes.Repeat([]byte("x"), 70000), 0o600))
	must(os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644))
	must(os.WriteFile(filepath.Join(dir, "setuid"), []byte("#!/bin/sh\n"), 0o755))
	must(os.Chmod(filepath.Join(dir, "setuid"), 0o4755|fs.ModeSetuid))
	must(os.Symlink("/d/f1", filepath.Join(dir, "abs")))
	must(os.Symlink("sub", filepath.Join(dir, "d/rel")))
	must(os.Link(filepath.Join(dir, "d/f1"), filepath.Join(dir, "hard")))
	must(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o640))
	must(syscall.Mknod(filepath.Join(dir, "null"), syscall.S_IFCHR|0o666, int(unix.Mkdev(1, 3))))
	must(os.Lchown(filepath.Join(dir, "d/f1"), 1234, 5678))
	must(os.Lchown(filepath.Join(dir, "abs"), 99, 98))
	for _, p := range []string{"d/f1", "d/sub/f2", "d", "empty", "fifo"} {
		must(os.Chtimes(filepath.Join(dir, p), old, old))
	}
	// security.capability, as setcap writes it: version 2, cap_net_raw.
	capv2 := []byte{0, 0, 0, 2, 0, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := unix.Lsetxattr(filepath.Join(dir, "empty"), "security.capability", capv2, 0); err != nil {
		t.Logf("no security.capability: %v", err)
	}
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func ours(t *testing.T, dir string, opts tarOptions) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeTar(&buf, openRoot(t, dir), opts); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sameTar compares two tars entry by entry, and then byte for byte.
func sameTar(t *testing.T, got, want []byte) {
	t.Helper()
	g, w := listTar(t, got), listTar(t, want)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("entries differ:\n got %q\nwant %q", g, w)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("same entries, different bytes: %d and %d bytes", len(got), len(want))
	}
}

func listTar(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var out []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out = append(out, fmt.Sprintf("%s %c %o %d:%d %d %s %q %d/%d %v %x", hdr.Name, hdr.Typeflag, hdr.Mode, hdr.Uid, hdr.Gid,
			hdr.Size, hdr.ModTime.UTC().Format(time.RFC3339Nano), hdr.Linkname, hdr.Devmajor, hdr.Devminor, hdr.PAXRecords, sha256.Sum256(body)))
	}
}

func TestWriteTarAsGoArchive(t *testing.T) {
	needRoot(t)
	dir := tmpfsDir(t)
	makeTree(t, dir)

	want := readAll(t, must(archive.TarWithOptions(dir, &archive.TarOptions{})))
	sameTar(t, ours(t, dir, tarOptions{}), want)

	// As docker cp gets a directory, a file, a symlink and dir/.
	for _, src := range []string{"d", "d/f1", "abs", "d/.", "d/sub/"} {
		t.Run(src, func(t *testing.T) {
			full := filepath.Join(dir, src)
			if strings.HasSuffix(src, "/") {
				full += "/"
			}
			rebase := filepath.Base(src)
			want := readAll(t, must(archive.TarResourceRebase(full, rebase)))
			sdir, base := splitPathDirEntry(full)
			got := ours(t, sdir, tarOptions{include: []string{base}, includeSourceDir: true, rebase: map[string]string{base: rebase}})
			sameTar(t, got, want)
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// makeUpper makes an overlayfs upper directory: an added file, a changed
// one, a whiteout, an opaque directory and an anonymous volume's files.
func makeUpper(t *testing.T, upper string) {
	t.Helper()
	for _, p := range []string{"etc", "home", "anon", "new/deep"} {
		if err := os.MkdirAll(filepath.Join(upper, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(upper, "etc/motd"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(upper, "home/h"), []byte("h\n"), 0o644)
	os.WriteFile(filepath.Join(upper, "anon/f"), []byte("volume\n"), 0o644)
	os.WriteFile(filepath.Join(upper, "new/deep/x"), []byte("x\n"), 0o644)
	if err := syscall.Mknod(filepath.Join(upper, "etc/issue"), syscall.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mknod(filepath.Join(upper, "never"), syscall.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lsetxattr(filepath.Join(upper, "home"), opaqueXattr, []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
}

func TestDiffTarAsGoArchive(t *testing.T) {
	needRoot(t)
	upper := tmpfsDir(t)
	makeUpper(t, upper)
	want := readAll(t, must(archive.TarWithOptions(upper, &archive.TarOptions{WhiteoutFormat: archive.OverlayWhiteoutFormat})))
	sameTar(t, ours(t, upper, tarOptions{whiteouts: true}), want)

	// An anonymous volume's directory is kept, its files are not.
	got := listTar(t, ours(t, upper, tarOptions{whiteouts: true, excludeBelow: []string{"anon"}}))
	var names []string
	for _, e := range got {
		names = append(names, strings.Fields(e)[0])
	}
	if !slices.Contains(names, "anon/") || slices.Contains(names, "anon/f") {
		t.Errorf("anon excluded wrongly: %q", names)
	}
	if !slices.Contains(names, "etc/.wh.issue") || !slices.Contains(names, "home/.wh..wh..opq") {
		t.Errorf("whiteouts missing: %q", names)
	}
}

func TestChanges(t *testing.T) {
	needRoot(t)
	base := tmpfsDir(t)
	lower, upper := filepath.Join(base, "lower"), filepath.Join(base, "upper")
	for _, p := range []string{"etc", "home/old", "root", "bin"} {
		os.MkdirAll(filepath.Join(lower, p), 0o755)
	}
	os.WriteFile(filepath.Join(lower, "etc/motd"), []byte("motd\n"), 0o644)
	os.WriteFile(filepath.Join(lower, "etc/issue"), []byte("issue\n"), 0o644)
	os.WriteFile(filepath.Join(lower, "home/g"), []byte("g\n"), 0o644)
	os.WriteFile(filepath.Join(lower, "bin/sh"), []byte("sh\n"), 0o755)
	makeUpper(t, upper)
	// A directory whose times alone changed: not a change.
	os.MkdirAll(filepath.Join(upper, "root"), 0o755)
	// One whose mode changed: a change.
	os.MkdirAll(filepath.Join(upper, "bin"), 0o700)
	os.Chmod(filepath.Join(upper, "bin"), 0o700)

	got, err := changes(openRoot(t, upper), []*os.Root{openRoot(t, lower)}, []string{"anon"})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range got {
		lines = append(lines, fmt.Sprintf("%c %s", "CAD"[c.Kind], c.Path))
	}
	want := []string{
		"A /anon",
		"C /bin",
		"C /etc",
		"D /etc/issue",
		"C /etc/motd",
		"C /home",
		"D /home/g",
		"A /home/h",
		"D /home/old",
		"A /new",
		"A /new/deep",
		"A /new/deep/x",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("changes:\n got %q\nwant %q", lines, want)
	}
	_ = guestspec.ChangeAdd
}

// treeOf describes the files below dir, for comparing extractions.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		fi, _ := os.Lstat(p)
		st := fi.Sys().(*syscall.Stat_t)
		line := fmt.Sprintf("%s %v %d:%d %d", rel, fi.Mode(), st.Uid, st.Gid, fi.ModTime().UnixNano())
		switch {
		case fi.Mode().IsRegular():
			data, _ := os.ReadFile(p)
			line += fmt.Sprintf(" %x nlink=%d", sha256.Sum256(data), st.Nlink)
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		case fi.Mode()&fs.ModeDevice != 0:
			line += fmt.Sprintf(" %d", st.Rdev)
		}
		if c, _ := unix.Lgetxattr(p, "security.capability", make([]byte, 64)); c > 0 {
			line += " cap"
		}
		out = append(out, line)
		return nil
	})
	return out
}

func TestUntarAsGoArchive(t *testing.T) {
	needRoot(t)
	src := tmpfsDir(t)
	makeTree(t, src)
	data := readAll(t, must(archive.TarWithOptions(src, &archive.TarOptions{})))

	want, got := tmpfsDir(t), tmpfsDir(t)
	if err := archive.Untar(bytes.NewReader(data), want, &archive.TarOptions{NoOverwriteDirNonDir: true}); err != nil {
		t.Fatal(err)
	}
	if err := untar(bytes.NewReader(data), openRoot(t, got), untarOptions{noOverwriteDirNonDir: true}); err != nil {
		t.Fatal(err)
	}
	if w, g := treeOf(t, want), treeOf(t, got); !reflect.DeepEqual(w, g) {
		t.Errorf("trees differ:\n got %q\nwant %q", g, w)
	}

	// Again over what is there: files replaced, directories merged.
	if err := untar(bytes.NewReader(data), openRoot(t, got), untarOptions{noOverwriteDirNonDir: true}); err != nil {
		t.Fatal(err)
	}
	// The root itself changed when its files were made again: compare
	// what is below it.
	if w, g := treeOf(t, want)[1:], treeOf(t, got)[1:]; !reflect.DeepEqual(w, g) {
		t.Errorf("trees differ after a second extraction:\n got %q\nwant %q", g, w)
	}

	// A file over a directory, and a directory over a file.
	clash := func(name string, dir bool) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}
		if dir {
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		}
		tw.WriteHeader(hdr)
		tw.Close()
		return buf.Bytes()
	}
	for _, c := range []struct {
		name string
		dir  bool
		want string
	}{{"d", false, "cannot overwrite directory"}, {"empty", true, "cannot overwrite non-directory"}} {
		err := untar(bytes.NewReader(clash(c.name, c.dir)), openRoot(t, got), untarOptions{noOverwriteDirNonDir: true})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}

	// Owned by someone else, as cp -a does.
	chowned := tmpfsDir(t)
	if err := untar(bytes.NewReader(data), openRoot(t, chowned), untarOptions{chown: &[2]int{7, 8}}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(filepath.Join(chowned, "d/f1"))
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 7 || st.Gid != 8 {
		t.Errorf("chown: %d:%d", st.Uid, st.Gid)
	}

	// An entry that climbs out is refused.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../out", Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Close()
	if err := untar(&buf, openRoot(t, tmpfsDir(t)), untarOptions{}); err == nil {
		t.Error("an entry climbing out was extracted")
	}
}

func TestPreserveTrailing(t *testing.T) {
	for _, c := range [][3]string{
		{"/d/f1", "/d/f1", "d/f1"},
		{"/d/f1/", "/d/f1", "/d/f1/"},
		{"/d/.", "/d", "d/."},
		{"/", "/", "/"},
		{"/.", "/", "/."},
	} {
		if got := preserveTrailingDotOrSeparator(c[1], c[2]); got != c[0] {
			t.Errorf("preserve(%q, %q) = %q, want %q", c[1], c[2], got, c[0])
		}
	}
}

// TestUntarMerge extracts as build's COPY does: directories that exist
// keep their mode and owner, and take the tar's times; "." describes the
// root, which takes its mode and owner only when the put made it; and
// parents the tar lacks are made for chown's user.
func TestUntarMerge(t *testing.T) {
	needRoot(t)
	t0 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o750, ModTime: t0})
	tw.WriteHeader(&tar.Header{Name: "e/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: t1})
	tw.WriteHeader(&tar.Header{Name: "e/f", Typeflag: tar.TypeReg, Mode: 0o640, Size: 1, ModTime: t0})
	tw.Write([]byte("x"))
	tw.WriteHeader(&tar.Header{Name: "n/x", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: t0})
	tw.Close()

	owner := &[2]int{5, 6}
	for _, made := range []bool{false, true} {
		dir := tmpfsDir(t)
		if err := os.Mkdir(filepath.Join(dir, "e"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(dir, "e"), 0o700)
		os.Lchown(filepath.Join(dir, "e"), 1, 1)
		opts := untarOptions{merge: true, madeRoot: made, chown: owner, parentOwner: owner}
		if err := untar(bytes.NewReader(buf.Bytes()), openRoot(t, dir), opts); err != nil {
			t.Fatal(err)
		}
		stat := func(name string) (fs.FileMode, uint32, uint32, time.Time) {
			fi, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			st := fi.Sys().(*syscall.Stat_t)
			return fi.Mode(), st.Uid, st.Gid, fi.ModTime()
		}
		mode, uid, gid, mtime := stat(".")
		wantMode, wantUID := fs.ModeDir|0o755, uint32(0)
		if made {
			wantMode, wantUID = fs.ModeDir|0o750, 5
		}
		if mode != wantMode || uid != wantUID || !mtime.Equal(t0) {
			t.Errorf("made %v: root %v %d:%d %v", made, mode, uid, gid, mtime)
		}
		if mode, uid, gid, mtime := stat("e"); mode != fs.ModeDir|0o700 || uid != 1 || gid != 1 || !mtime.Equal(t1) {
			t.Errorf("made %v: existing dir %v %d:%d %v", made, mode, uid, gid, mtime)
		}
		if mode, uid, gid, _ := stat("e/f"); mode != 0o640 || uid != 5 || gid != 6 {
			t.Errorf("made %v: file %v %d:%d", made, mode, uid, gid)
		}
		if mode, uid, gid, _ := stat("n"); mode != fs.ModeDir|0o755 || uid != 5 || gid != 6 {
			t.Errorf("made %v: parent %v %d:%d", made, mode, uid, gid)
		}
	}
}
