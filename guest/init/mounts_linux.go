//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/passwd"
)

// sharesDir is where init mounts the host's shares, in the initramfs. Once
// enterRoot has moved the container's root over it, nothing can reach it.
const sharesDir = "/shares"

// mountShares mounts the virtio-fs device and binds each share onto its
// target in the container's root. Host files without an owner of their own
// belong to the container's user.
func mountShares(mounts []guestspec.Mount, user passwd.User) error {
	if len(mounts) == 0 {
		return nil
	}
	if err := os.MkdirAll(sharesDir, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(guestspec.FSTag, sharesDir, "virtiofs", 0, ""); err != nil {
		return fmt.Errorf("mounting the host's files: %w", err)
	}
	if fd, err := syscall.Open(sharesDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); err == nil {
		storage.shares = fd
	}
	owner := fmt.Sprintf("%d:%d", user.UID, user.GID)
	if err := syscall.Setxattr(sharesDir, guestspec.OwnerXattr, []byte(owner), 0); err != nil {
		return fmt.Errorf("setting the owner of the host's files: %w", err)
	}
	for _, m := range mounts {
		target, err := resolveIn(m.Target)
		if err != nil {
			return fmt.Errorf("mount point %s: %w", m.Target, err)
		}
		if err := makeMountPoint(target, m.File); err != nil {
			return fmt.Errorf("mount point %s: %w", m.Target, err)
		}
		source := filepath.Join(sharesDir, strconv.Itoa(m.Share))
		if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding the host's files onto %s: %w", m.Target, err)
		}
		if m.ReadOnly {
			if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("making %s read-only: %w", m.Target, err)
			}
		}
		debugf("bound share %d onto %s (read-only %v)", m.Share, m.Target, m.ReadOnly)
	}
	return nil
}

// makeMountPoint creates a missing mount point: a directory, or an empty
// file for a file.
func makeMountPoint(p string, file bool) error {
	fi, err := os.Stat(p)
	switch {
	case err == nil && fi.IsDir() == file:
		if file {
			return errors.New("is a directory, and the host path is a file")
		}
		return errors.New("is not a directory, and the host path is one")
	case err == nil:
		return nil
	case !os.IsNotExist(err):
		return err
	}
	if !file {
		return os.MkdirAll(p, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// resolveIn returns where the container path p lies below root, following
// symlinks the way the container would see them: an absolute link starts
// from the container's root, and ".." never climbs above it. The first
// missing component ends the walk; the rest is kept as given.
func resolveIn(p string) (string, error) {
	resolved := "/"
	parts := strings.Split(p, "/")
	for links := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if os.IsNotExist(err) {
			resolved = path.Join(append([]string{next}, parts...)...)
			break
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > 40 {
			return "", syscall.ELOOP
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			resolved = "/"
		}
		parts = append(strings.Split(target, "/"), parts...)
	}
	return filepath.Join(root, resolved), nil
}
