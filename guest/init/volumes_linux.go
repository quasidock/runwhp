//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// Where init mounts volumes' file systems, and binds anonymous volumes, in
// the initramfs. Once enterRoot has moved the container's root over it,
// nothing can reach them by path.
func volumeDir(i int) string { return fmt.Sprintf("/volumes/%d", i) }
func anonDir(i int) string   { return fmt.Sprintf("/anon/%d", i) }

// volumeMountOpts: discard gives the host back the space of what the
// container deletes.
const volumeMountOpts = "discard"

// prepareVolumes mounts each volume's file system, makes its _data
// directory, and fills it from the image if it is empty; and makes the
// directory of each anonymous volume. It runs before any mount is made in
// the container's root, so what it copies is the image's.
func prepareVolumes(spec *guestspec.Spec) error {
	if spec.Volumes > 0 {
		serials := make([]string, spec.Volumes)
		for i := range serials {
			serials[i] = guestspec.VolumeSerial(i)
		}
		devices, err := waitDisks(serials)
		if err != nil {
			return err
		}
		openRCUExpedited()
		for i := range spec.Volumes {
			dev, dir := devices[guestspec.VolumeSerial(i)], volumeDir(i)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			replay := needsRecovery(dev)
			if err := syscall.Mount(dev, dir, "ext4", 0, volumeMountOpts); err != nil {
				return fmt.Errorf("mounting volume disk %d (%s): %w", i, dev, err)
			}
			if replay {
				debugf("volume disk %d (%s): replayed its journal", i, dev)
			}
			fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			storage.volumes = append(storage.volumes, fd)
			if err := os.Mkdir(filepath.Join(dir, guestspec.VolumeData), 0o755); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
	for i, m := range spec.Mounts {
		switch m.Kind {
		case guestspec.MountVolume:
			if m.NoCopy {
				continue
			}
			data := filepath.Join(volumeDir(m.Volume), guestspec.VolumeData)
			if empty, err := emptyDir(data); err != nil || !empty {
				continue
			}
			src, err := imageDir(m.Target)
			if err != nil {
				return fmt.Errorf("volume %s on %s: %w", m.Name, m.Target, err)
			}
			if src == "" {
				continue
			}
			if err := copyTree(src, data); err != nil {
				return fmt.Errorf("filling volume %s from the image's %s: %w", m.Name, m.Target, err)
			}
			debugf("filled volume %s from the image's %s", m.Name, m.Target)
		case guestspec.MountAnon:
			var src string
			if m.NoCopy {
				src = filepath.Join("/rw/anon", fmt.Sprint(i))
			} else {
				// The image's directory itself, in the writable layer, bound
				// aside now in case a bind mount covers it later.
				var err error
				if src, err = imageDir(m.Target); err != nil {
					return fmt.Errorf("volume on %s: %w", m.Target, err)
				}
				if src == "" {
					if src, err = resolveIn(m.Target); err != nil {
						return err
					}
				}
			}
			if err := os.MkdirAll(src, 0o755); err != nil {
				return err
			}
			dir := anonDir(i)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := syscall.Mount(src, dir, "", syscall.MS_BIND, ""); err != nil {
				return fmt.Errorf("volume on %s: %w", m.Target, err)
			}
		}
	}
	return nil
}

// imageDir returns where the image's directory at the container path p is,
// or "" if the image has nothing there. Something other than a directory
// there is an error: a volume can only be mounted on a directory.
func imageDir(p string) (string, error) {
	src, err := resolveIn(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(src)
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err != nil:
		return "", err
	case !fi.IsDir():
		return "", fmt.Errorf("the image has a file there, and a volume must be a directory")
	}
	return src, nil
}

// needsRecovery reports whether the ext4 file system on dev was left with
// work in its journal, which mounting it replays.
func needsRecovery(dev string) bool {
	f, err := os.Open(dev)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [4]byte
	if _, err := f.ReadAt(b[:], 1024+0x60); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(b[:])&0x4 != 0 // INCOMPAT_RECOVER
}
