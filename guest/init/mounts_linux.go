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

// mountAll makes the container's mounts: binds of the host's shares, named
// and anonymous volumes, and tmpfs mounts. Host files without an owner of
// their own belong to the container's user.
//
// Volumes are filled from the image first, before anything is mounted over
// it, so a volume under a bind mount gets what the image has there, not
// what the host has. Then every mount is made in the spec's order, parents
// first.
func mountAll(spec *guestspec.Spec, user passwd.User) error {
	binds := false
	for _, m := range spec.Mounts {
		binds = binds || m.Kind == guestspec.MountBind
	}
	if binds {
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
	}
	if err := prepareVolumes(spec); err != nil {
		return err
	}
	for i, m := range spec.Mounts {
		target, err := resolveIn(m.Target)
		if err != nil {
			return fmt.Errorf("mount point %s: %w", m.Target, err)
		}
		if err := makeMountPoint(target, m.File); err != nil {
			return fmt.Errorf("mount point %s: %w", m.Target, err)
		}
		var source, what string
		switch m.Kind {
		case guestspec.MountBind:
			source, what = filepath.Join(sharesDir, strconv.Itoa(m.Share)), fmt.Sprintf("share %d", m.Share)
		case guestspec.MountVolume:
			source, what = filepath.Join(volumeDir(m.Volume), guestspec.VolumeData), "volume "+m.Name
		case guestspec.MountAnon:
			source, what = anonDir(i), "an anonymous volume"
		case guestspec.MountTmpfs:
			flags, data := tmpfsFlags(m.Options)
			if err := syscall.Mount("tmpfs", target, "tmpfs", flags, data); err != nil {
				return fmt.Errorf("mounting a tmpfs on %s: %w", m.Target, err)
			}
			debugf("mounted a tmpfs on %s (%s)", m.Target, m.Options)
			continue
		default:
			return fmt.Errorf("mount %s: unknown kind %q", m.Target, m.Kind)
		}
		if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("mounting %s on %s: %w", what, m.Target, err)
		}
		if m.ReadOnly {
			if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("making %s read-only: %w", m.Target, err)
			}
		}
		debugf("mounted %s on %s (read-only %v)", what, m.Target, m.ReadOnly)
	}
	return nil
}

// tmpfsFlags splits a tmpfs's options into mount flags and the options
// tmpfs itself reads.
func tmpfsFlags(options string) (uintptr, string) {
	var flags uintptr
	var data []string
	for _, o := range strings.Split(options, ",") {
		switch o {
		case "":
		case "noexec":
			flags |= syscall.MS_NOEXEC
		case "nosuid":
			flags |= syscall.MS_NOSUID
		case "nodev":
			flags |= syscall.MS_NODEV
		case "ro":
			flags |= syscall.MS_RDONLY
		default:
			data = append(data, o)
		}
	}
	return flags, strings.Join(data, ",")
}

// makeMountPoint creates a missing mount point: a directory, or an empty
// file for a file.
func makeMountPoint(p string, file bool) error {
	fi, err := os.Stat(p)
	switch {
	case err == nil && fi.IsDir() == file:
		if file {
			return errors.New("is a directory, and a file is mounted there")
		}
		return errors.New("is not a directory, and a directory is mounted there")
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
