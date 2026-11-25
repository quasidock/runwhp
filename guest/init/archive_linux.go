//go:build linux

package main

// The archives runwhp cp, export, diff and commit move: tars of the
// container's files written and read as Docker's daemon writes and reads
// them, with github.com/moby/go-archive (Apache-2.0), whose rules this
// follows. It is not linked into init, as its compression alone would
// grow init by half; the tests check this against it.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Whiteouts, as image layers carry them in tars.
const (
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = ".wh..wh..opq"
	opaqueXattr       = "trusted.overlay.opaque"
	paxSchilyXattr    = "SCHILY.xattr."
)

// tarOptions are the parts of go-archive's TarOptions runwhp uses.
type tarOptions struct {
	// include are the paths, relative to the root, to archive, each with
	// what is below it; none means ".".
	include []string
	// rebase renames an include's first element in the archive.
	rebase map[string]string
	// includeSourceDir archives "." itself too, as "./".
	includeSourceDir bool
	// excludeBelow leaves out what is below each of these paths, though
	// not the path itself: anonymous volumes' contents.
	excludeBelow []string
	// whiteouts writes overlayfs's whiteouts and opaque directories as
	// image layers carry them.
	whiteouts bool
}

// writeTar writes the files below root to w, as go-archive's
// TarWithOptions does.
func writeTar(w io.Writer, root *os.Root, opts tarOptions) error {
	t := &tarWriter{tw: tar.NewWriter(w), root: root, opts: opts, seenInodes: map[uint64]string{}, seen: map[string]bool{}}
	includes := opts.include
	if len(includes) == 0 {
		includes = []string{"."}
	}
	for _, include := range includes {
		if err := t.walk(include, include); err != nil {
			return err
		}
	}
	return t.tw.Close()
}

type tarWriter struct {
	tw         *tar.Writer
	root       *os.Root
	opts       tarOptions
	seenInodes map[uint64]string // hard links: the first name of each inode
	seen       map[string]bool
}

// walk archives rel and, if it is a directory, what is below it, in
// lexical order, as filepath.WalkDir visits them.
func (t *tarWriter) walk(include, rel string) error {
	fi, err := t.root.Lstat(rel)
	if err != nil {
		if rel == include {
			return err
		}
		// The container may be changing its files meanwhile; go-archive
		// passes over what it cannot read too.
		logf("archiving %s: %v", rel, err)
		return nil
	}
	if err := t.visit(include, rel, fi); err != nil {
		return err
	}
	if !fi.IsDir() || t.excluded(rel) {
		return nil
	}
	dir, err := t.root.Open(rel)
	if err != nil {
		logf("archiving %s: %v", rel, err)
		return nil
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		logf("archiving %s: %v", rel, err)
		return nil
	}
	slices.Sort(names)
	for _, name := range names {
		if err := t.walk(include, path.Join(rel, name)); err != nil {
			return err
		}
	}
	return nil
}

// excluded reports whether what is below rel is left out.
func (t *tarWriter) excluded(rel string) bool {
	return slices.Contains(t.opts.excludeBelow, path.Clean(rel))
}

func (t *tarWriter) visit(include, rel string, fi fs.FileInfo) error {
	name := rel
	if !t.opts.includeSourceDir && name == "." && fi.IsDir() {
		return nil
	}
	if t.opts.includeSourceDir && include == "." && name != "." {
		name = "./" + name
	}
	if t.seen[name] {
		return nil
	}
	t.seen[name] = true
	if rebase := t.opts.rebase[include]; rebase != "" {
		replacement := rebase
		if rebase == "/" {
			replacement = ""
		}
		name = strings.Replace(name, include, replacement, 1)
	}
	return t.add(rel, name, fi)
}

// add writes one entry, as go-archive's addTarFile does.
func (t *tarWriter) add(rel, name string, fi fs.FileInfo) error {
	var link string
	if fi.Mode()&fs.ModeSymlink != 0 {
		var err error
		if link, err = t.root.Readlink(rel); err != nil {
			return err
		}
	}
	hdr, err := fileInfoHeader(name, fi, link)
	if err != nil {
		return err
	}
	st := fi.Sys().(*syscall.Stat_t)

	// Regular files and directories are opened for their xattrs, and a
	// regular file for its contents; nothing else is, not to block on a
	// FIFO.
	var f *os.File
	if fi.Mode().IsRegular() || fi.IsDir() {
		if f, err = t.root.Open(rel); err != nil {
			return err
		}
		defer f.Close()
		if c, err := fgetxattr(f, "security.capability"); err == nil && c != nil {
			const xattrCapsSz2, versionOffset, vfsCapRevision2, vfsCapRevision3 = 20, 3, 2, 3
			if len(c) > versionOffset && c[versionOffset] == vfsCapRevision3 && len(c) >= xattrCapsSz2 {
				c[versionOffset] = vfsCapRevision2
				c = c[:xattrCapsSz2]
			}
			hdr.PAXRecords = map[string]string{paxSchilyXattr + "security.capability": string(c)}
		}
	}

	if !fi.IsDir() && st.Nlink > 1 {
		if first, ok := t.seenInodes[st.Ino]; ok {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
		} else {
			t.seenInodes[st.Ino] = hdr.Name
		}
	}

	if t.opts.whiteouts {
		if fi.Mode()&fs.ModeCharDevice != 0 && hdr.Devmajor == 0 && hdr.Devminor == 0 {
			dir, base := path.Split(hdr.Name)
			hdr.Name = path.Join(dir, whiteoutPrefix+base)
			hdr.Mode, hdr.Typeflag, hdr.Size = 0o600, tar.TypeReg, 0
		}
		if fi.IsDir() {
			if v, err := fgetxattr(f, opaqueXattr); err == nil && string(v) == "y" {
				if err := t.tw.WriteHeader(hdr); err != nil {
					return err
				}
				hdr = &tar.Header{
					Typeflag: tar.TypeReg, Mode: hdr.Mode & int64(fs.ModePerm), Name: path.Join(hdr.Name, whiteoutOpaqueDir),
					Uid: hdr.Uid, Gid: hdr.Gid, Format: tar.FormatPAX,
				}
			}
		}
	}

	if err := t.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
		if _, err := io.Copy(t.tw, f); err != nil {
			return err
		}
	}
	return nil
}

// fileInfoHeader is go-archive's FileInfoHeader: tar's header, without
// user and group names, owners and devices from the stat, PAX, and times
// to the second, without access and change times.
func fileInfoHeader(name string, fi fs.FileInfo, link string) (*tar.Header, error) {
	hdr, err := tar.FileInfoHeader(nosysFileInfo{fi}, link)
	if err != nil {
		return nil, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
		if st.Mode&unix.S_IFBLK != 0 || st.Mode&unix.S_IFCHR != 0 {
			hdr.Devmajor, hdr.Devminor = int64(unix.Major(st.Rdev)), int64(unix.Minor(st.Rdev))
		}
	}
	hdr.Format = tar.FormatPAX
	hdr.ModTime = hdr.ModTime.Truncate(time.Second)
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
	hdr.Name = name
	if fi.IsDir() && !strings.HasSuffix(name, "/") {
		hdr.Name += "/"
	}
	return hdr, nil
}

// nosysFileInfo keeps tar.FileInfoHeader from looking up user and group
// names, which go-archive leaves empty.
type nosysFileInfo struct{ fs.FileInfo }

func (nosysFileInfo) Uname() (string, error) { return "", nil }
func (nosysFileInfo) Gname() (string, error) { return "", nil }
func (nosysFileInfo) Sys() any               { return nil }

func fgetxattr(f *os.File, attr string) ([]byte, error) {
	buf := make([]byte, 128)
	for {
		n, err := unix.Fgetxattr(int(f.Fd()), attr, buf)
		switch {
		case errors.Is(err, unix.ERANGE):
			buf = make([]byte, 2*len(buf))
			continue
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
			return nil, nil
		case err != nil:
			return nil, err
		}
		return buf[:n], nil
	}
}

// splitPathDirEntry is go-archive's SplitPathDirEntry: path's directory
// and its last element, a trailing "." kept.
func splitPathDirEntry(p string) (dir, base string) {
	cleaned := filepath.Clean(p)
	if filepath.Base(p) == "." {
		cleaned += "/."
	}
	return filepath.Dir(cleaned), filepath.Base(cleaned)
}

// untarOptions are the parts of go-archive's TarOptions extraction uses.
type untarOptions struct {
	noOverwriteDirNonDir bool
	// chown, if set, owns every file extracted instead of the archive's
	// owners.
	chown *[2]int
	// parentOwner, if set, owns the directories made for entries whose
	// parents the archive lacks; else root does.
	parentOwner *[2]int
	// merge is BuildKit's copy: a directory that exists keeps its mode
	// and owner and takes only the entry's times, and an entry "."
	// describes root itself, whose mode and owner it sets only if
	// madeRoot.
	merge, madeRoot bool
}

// untar extracts the archive r into root, as go-archive's Unpack does.
func untar(r io.Reader, root *os.Root, opts untarOptions) error {
	tr := tar.NewReader(r)
	type dirTimes struct {
		name string
		hdr  *tar.Header
	}
	var dirs []dirTimes
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := path.Clean(strings.TrimLeft(hdr.Name, "/"))
		if name == "." {
			if opts.merge && hdr.Typeflag == tar.TypeDir {
				if opts.madeRoot {
					if err := chownChmod(root, name, hdr, opts); err != nil {
						return err
					}
				}
				dirs = append(dirs, dirTimes{name, hdr})
			}
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("invalid entry name %q", hdr.Name)
		}
		hdr.Name = name
		if fi, err := root.Lstat(name); err == nil {
			if opts.merge && fi.IsDir() && hdr.Typeflag == tar.TypeDir {
				dirs = append(dirs, dirTimes{name, hdr})
				continue
			}
			if opts.noOverwriteDirNonDir && fi.IsDir() && hdr.Typeflag != tar.TypeDir {
				return fmt.Errorf("cannot overwrite directory %q with non-directory %q", "/"+name, hdr.Name)
			}
			if opts.noOverwriteDirNonDir && !fi.IsDir() && hdr.Typeflag == tar.TypeDir {
				return fmt.Errorf("cannot overwrite non-directory %q with directory %q", "/"+name, hdr.Name)
			}
			if !fi.IsDir() || hdr.Typeflag != tar.TypeDir {
				if err := root.RemoveAll(name); err != nil {
					return err
				}
			}
		}
		if err := makeParents(root, name, opts.parentOwner); err != nil {
			return err
		}
		if err := extractEntry(root, name, hdr, tr, opts); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, dirTimes{name, hdr})
		}
	}
	for _, d := range dirs {
		if err := root.Chtimes(d.name, boundTime(latestTime(d.hdr.AccessTime, d.hdr.ModTime)), boundTime(d.hdr.ModTime)); err != nil {
			return err
		}
	}
	return nil
}

// chownChmod gives name the entry's owner, or opts.chown's, and mode.
func chownChmod(root *os.Root, name string, hdr *tar.Header, opts untarOptions) error {
	uid, gid := hdr.Uid, hdr.Gid
	if opts.chown != nil {
		uid, gid = opts.chown[0], opts.chown[1]
	}
	if err := root.Lchown(name, uid, gid); err != nil {
		return err
	}
	return root.Chmod(name, hdr.FileInfo().Mode())
}

// makeParents makes the missing directories above name, 0755, owned by
// owner if set, else root.
func makeParents(root *os.Root, name string, owner *[2]int) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	if _, err := root.Lstat(parent); err == nil {
		return nil
	}
	var cur string
	for _, c := range strings.Split(parent, "/") {
		cur = path.Join(cur, c)
		if err := root.Mkdir(cur, 0o755); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			if fi, err := root.Stat(cur); err != nil {
				return err
			} else if !fi.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: cur, Err: syscall.ENOTDIR}
			}
			continue
		}
		if err := root.Chmod(cur, 0o755); err != nil {
			return err
		}
		if owner != nil {
			if err := root.Lchown(cur, owner[0], owner[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractEntry is go-archive's createTarFile.
func extractEntry(root *os.Root, name string, hdr *tar.Header, r io.Reader, opts untarOptions) error {
	info := hdr.FileInfo()
	var hardlink string
	if hdr.Typeflag == tar.TypeLink {
		target := path.Clean(strings.TrimLeft(hdr.Linkname, "/"))
		if target == "." || !filepath.IsLocal(target) {
			return fmt.Errorf("invalid hardlink target %q", hdr.Linkname)
		}
		hardlink = target
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if fi, err := root.Lstat(name); err != nil || !fi.IsDir() {
			if err := root.Mkdir(name, info.Mode()&0o777); err != nil {
				return err
			}
		}
	case tar.TypeReg, tar.TypeRegA:
		f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode()&0o777)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case tar.TypeBlock, tar.TypeChar, tar.TypeFifo:
		mode := uint32(hdr.Mode & 0o7777)
		switch hdr.Typeflag {
		case tar.TypeBlock:
			mode |= unix.S_IFBLK
		case tar.TypeChar:
			mode |= unix.S_IFCHR
		case tar.TypeFifo:
			mode |= unix.S_IFIFO
		}
		if err := atParent(root, name, func(dirfd int, base string) error {
			return unix.Mknodat(dirfd, base, mode, int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))))
		}); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := root.Link(hardlink, name); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := root.Symlink(hdr.Linkname, name); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unhandled tar header type %d", hdr.Typeflag)
	}

	uid, gid := hdr.Uid, hdr.Gid
	if opts.chown != nil {
		uid, gid = opts.chown[0], opts.chown[1]
	}
	if err := root.Lchown(name, uid, gid); err != nil {
		return fmt.Errorf("failed to Lchown %q for UID %d, GID %d: %w", name, uid, gid, err)
	}

	// Xattrs on regular files and directories, which are opened for them;
	// devices and FIFOs are not, not to open what they lead to.
	if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA || hdr.Typeflag == tar.TypeDir {
		var f *os.File
		for key, value := range hdr.PAXRecords {
			attr, ok := strings.CutPrefix(key, paxSchilyXattr)
			if !ok {
				continue
			}
			if f == nil {
				var err error
				if f, err = root.Open(name); err != nil {
					return err
				}
				defer f.Close()
			}
			if err := unix.Fsetxattr(int(f.Fd()), attr, []byte(value), 0); err != nil && !errors.Is(err, syscall.EPERM) {
				return err
			}
		}
	}

	// After the owner, which can clear setuid bits; never on a symlink.
	switch {
	case hdr.Typeflag == tar.TypeSymlink:
	case hdr.Typeflag == tar.TypeLink:
		if fi, err := root.Lstat(hardlink); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			if err := root.Chmod(name, info.Mode()); err != nil {
				return err
			}
		}
	default:
		if err := root.Chmod(name, info.Mode()); err != nil {
			return err
		}
	}

	atime, mtime := boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime)
	switch hdr.Typeflag {
	case tar.TypeSymlink:
		return atParent(root, name, func(dirfd int, base string) error {
			ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
			return unix.UtimesNanoAt(dirfd, base, ts, unix.AT_SYMLINK_NOFOLLOW)
		})
	case tar.TypeLink:
		if fi, err := root.Lstat(hardlink); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			return root.Chtimes(name, atime, mtime)
		}
		return nil
	}
	return root.Chtimes(name, atime, mtime)
}

// atParent calls f with a descriptor of name's directory, opened through
// root, and name's last element.
func atParent(root *os.Root, name string, f func(dirfd int, base string) error) error {
	dir, err := root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	return f(int(dir.Fd()), path.Base(name))
}

func latestTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// boundTime keeps a time within what the kernel takes, as go-archive does.
func boundTime(t time.Time) time.Time {
	if t.Before(unixEpoch) || t.After(unixMax) {
		return unixEpoch
	}
	return t
}

var (
	unixEpoch = time.Unix(0, 0)
	unixMax   = time.Unix(0, 1<<63-1)
)
