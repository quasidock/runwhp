//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// copyTree copies the contents of directory src into directory dst, which
// exists, keeping owners, modes, times, extended attributes, symlinks, hard
// links between the files copied, device nodes and FIFOs. Sockets are
// skipped. dst itself takes src's owner and mode, as Docker gives a new
// volume those of the directory it covers.
func copyTree(src, dst string) error {
	c := copier{links: map[fileID]string{}, buf: make([]byte, 1<<20)}
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	if err := c.copyDir(src, dst); err != nil {
		return err
	}
	return c.finish(src, dst, &st)
}

type fileID struct{ dev, ino uint64 }

type copier struct {
	links map[fileID]string // copied files with more than one link, by source
	buf   []byte
}

func (c *copier) copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := c.copyEntry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) copyEntry(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	id := fileID{uint64(st.Dev), st.Ino}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 {
		if first, ok := c.links[id]; ok {
			return os.Link(first, dst)
		}
		c.links[id] = dst
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		if err := c.copyDir(src, dst); err != nil {
			return err
		}
	case unix.S_IFREG:
		if err := c.copyFile(src, dst); err != nil {
			return err
		}
	case unix.S_IFLNK:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
		if err := unix.Mknod(dst, st.Mode&(unix.S_IFMT|0o777), int(st.Rdev)); err != nil {
			return err
		}
	case unix.S_IFSOCK:
		return nil
	default:
		return fmt.Errorf("%s: unknown file type %o", src, st.Mode&unix.S_IFMT)
	}
	return c.finish(src, dst, &st)
}

func (c *copier) copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(out, in, c.buf); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// finish gives dst src's extended attributes, owner, mode and times, in
// that order: a chown clears set-user-ID bits, and the times must be set
// last.
func (c *copier) finish(src, dst string, st *unix.Stat_t) error {
	if err := copyXattrs(src, dst); err != nil {
		return err
	}
	if err := unix.Lchown(dst, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		if err := unix.Chmod(dst, st.Mode&0o7777); err != nil {
			return err
		}
	}
	times := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, dst, times, unix.AT_SYMLINK_NOFOLLOW)
}

// copyXattrs copies extended attributes, except overlayfs's own, which
// describe the source's layers and mean nothing anywhere else.
func copyXattrs(src, dst string) error {
	size, err := unix.Llistxattr(src, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return err
	}
	names := make([]byte, size)
	if size, err = unix.Llistxattr(src, names); err != nil {
		return err
	}
	for _, name := range strings.Split(strings.TrimRight(string(names[:size]), "\x00"), "\x00") {
		if name == "" || strings.HasPrefix(name, "trusted.overlay.") {
			continue
		}
		n, err := unix.Lgetxattr(src, name, nil)
		if err != nil {
			return fmt.Errorf("%s: reading %s: %w", src, name, err)
		}
		val := make([]byte, n)
		if n, err = unix.Lgetxattr(src, name, val); err != nil {
			return fmt.Errorf("%s: reading %s: %w", src, name, err)
		}
		if err := unix.Lsetxattr(dst, name, val[:n], 0); err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("%s: setting %s: %w", dst, name, err)
		}
	}
	return nil
}

// emptyDir reports whether dir has no entries.
func emptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return len(names) == 0, err
}
