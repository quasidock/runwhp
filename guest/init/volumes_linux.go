//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

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
	// made says which volumes' _data init made now: new volumes.
	made := map[int]bool{}
	if spec.Volumes > 0 {
		shared := map[int]bool{}
		for _, i := range spec.SharedVolumes {
			shared[i] = true
		}
		var serials []string
		for i := range spec.Volumes {
			if !shared[i] {
				serials = append(serials, guestspec.VolumeSerial(i))
			}
		}
		devices, err := waitDisks(serials)
		if err != nil {
			return err
		}
		openRCUExpedited()
		for i := range spec.Volumes {
			dir := volumeDir(i)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if shared[i] {
				fd, err := mountSharedVolume(i, dir)
				if err != nil {
					return err
				}
				// Until the volume is ready, which another container
				// starting at once may be making it too: the lock is the
				// volume's, so every container sees it.
				if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
					return fmt.Errorf("locking shared volume %d: %w", i, err)
				}
				defer syscall.Flock(fd, syscall.LOCK_UN)
			} else {
				dev := devices[guestspec.VolumeSerial(i)]
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
			}
			err = os.Mkdir(filepath.Join(dir, guestspec.VolumeData), 0o755)
			if err != nil && !os.IsExist(err) {
				return err
			}
			made[i] = err == nil
		}
	}
	for i, m := range spec.Mounts {
		switch m.Kind {
		case guestspec.MountBind:
			// A containerd volume: a host directory, filled from the image
			// while it is empty, as a volume of runwhp's own is.
			if !m.CopyUp || m.File || m.ReadOnly {
				continue
			}
			data := shareSource(m)
			if empty, err := emptyDir(data); err != nil || !empty {
				continue
			}
			src, err := imageDir(m.Target)
			if err != nil {
				return fmt.Errorf("volume on %s: %w", m.Target, err)
			}
			if src == "" {
				continue
			}
			if err := copyTree(src, data); err != nil {
				return fmt.Errorf("filling the volume on %s from the image's: %w", m.Target, err)
			}
			debugf("filled the volume on %s from the image's", m.Target)
		case guestspec.MountVolume:
			data := filepath.Join(volumeDir(m.Volume), guestspec.VolumeData)
			if in := m.Init; in != nil && made[m.Volume] {
				if err := os.Lchown(data, in.UID, in.GID); err != nil {
					return err
				}
				if err := os.Chmod(data, os.FileMode(in.Mode&0o777)|unixMode(in.Mode)); err != nil {
					return err
				}
			}
			if m.NoCopy {
				continue
			}
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

// mountSharedVolume mounts volume i, served by the volume's own guest, at
// dir, and returns a descriptor of it, which stopStorage syncs.
func mountSharedVolume(i int, dir string) (int, error) {
	tag := guestspec.VolumeFSTag(i)
	// The device is probed as the kernel boots; give it a moment all the
	// same.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Mount(tag, dir, "virtiofs", 0, "")
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ENOENT) || time.Now().After(deadline) {
			return -1, fmt.Errorf("mounting shared volume %d: %w", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	storage.sharedVolumes = append(storage.sharedVolumes, fd)
	return fd, nil
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

// unixMode is the setuid, setgid and sticky bits of a Unix mode, as Go's
// os.FileMode has them.
func unixMode(m uint32) os.FileMode {
	var out os.FileMode
	if m&0o4000 != 0 {
		out |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		out |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		out |= os.ModeSticky
	}
	return out
}
