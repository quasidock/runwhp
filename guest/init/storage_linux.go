//go:build linux

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// storage holds what stopStorage needs, which enterRoot hides from paths:
// file descriptors on file systems whose data must reach the host before
// the guest resets.
var storage = struct {
	shares    int   // the virtio-fs mount, or -1
	delegated int   // the delegated shares' virtio-fs mount, or -1
	volumes   []int // each volume's file system
	// sharedVolumes are the virtio-fs mounts of volumes served by their own
	// guests.
	sharedVolumes []int
	rw            int // a kept writable layer's file system, or -1
	// rcuExpedited is /sys/kernel/rcu_expedited, opened while the
	// initramfs's sysfs is still reachable and writable, or -1.
	rcuExpedited int
}{shares: -1, delegated: -1, rw: -1, rcuExpedited: -1}

// FIFREEZE, which x/sys/unix lacks: _IOWR('X', 119, int).
const fifreeze = 0xc0045877

// openRCUExpedited opens /sys/kernel/rcu_expedited, once, while the
// initramfs's sysfs is reachable: for stopStorage, and for init's own
// steps that wait for RCU grace periods.
func openRCUExpedited() {
	if storage.rcuExpedited >= 0 {
		return
	}
	if fd, err := syscall.Open("/sys/kernel/rcu_expedited", syscall.O_WRONLY|syscall.O_CLOEXEC, 0); err == nil {
		storage.rcuExpedited = fd
	}
}

// stopStorage makes sure what the container wrote that is meant to outlast
// it has reached the host. Nothing is written back for a scratch disk that
// is thrown away: a global sync would write all of its dirty pages to the
// host just before it is deleted.
//
// Each volume's file system, and a kept writable layer's, is frozen: the
// kernel writes back its data, commits and empties its journal, and marks
// it clean, as an unmount would. An unmount cannot be relied on, since the
// container may have moved or copied its mounts, into namespaces of its own
// say, which keep the file system mounted. A freeze acts on the file system
// however it is mounted.
//
// Writes to bind mounts go to the host as they are made, except through
// shared memory mappings, whose dirty pages syncfs writes back, and on
// delegated bind mounts, whose cached writes it writes back too.
func stopStorage() {
	if storage.rcuExpedited >= 0 {
		// A freeze waits for an RCU grace period at each of its three
		// levels, and the balloon's driver for one as the kernel shuts it
		// down, at ten milliseconds or more apiece in an idle guest.
		// Expedited grace periods take microseconds, and with the
		// container gone their cost to everything else no longer matters.
		unix.Write(storage.rcuExpedited, []byte("1"))
	}
	for i, fd := range storage.volumes {
		freeze(fd, "volume disk", i)
	}
	if storage.rw >= 0 {
		freeze(storage.rw, "the writable layer", -1)
	}
	for _, fd := range []int{storage.shares, storage.delegated} {
		if fd >= 0 {
			if err := unix.Syncfs(fd); err != nil {
				logf("syncing the host's files: %v", err)
			}
		}
	}
	// A shared volume's writes reach its guest as they are made, but for
	// shared memory mappings' dirty pages; syncfs writes those back, and
	// has the volume's guest write the volume to its disk.
	for i, fd := range storage.sharedVolumes {
		if err := unix.Syncfs(fd); err != nil {
			logf("syncing shared volume %d: %v", i, err)
		}
	}
}

func freeze(fd int, what string, i int) {
	err := unix.IoctlSetInt(fd, fifreeze, 0)
	if err != nil && !errors.Is(err, unix.EBUSY) { // EBUSY: already frozen
		if i >= 0 {
			logf("freezing %s %d: %v; syncing it instead", what, i, err)
		} else {
			logf("freezing %s: %v; syncing it instead", what, err)
		}
		unix.Syncfs(fd)
	}
}
