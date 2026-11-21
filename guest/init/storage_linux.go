//go:build linux

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// storage holds what stopStorage needs, which enterRoot hides from paths:
// file descriptors on file systems whose data must reach the host before
// the guest resets.
var storage = struct {
	shares  int   // the virtio-fs mount, or -1
	volumes []int // each volume's file system
	// rcuExpedited is /sys/kernel/rcu_expedited, opened while the
	// initramfs's sysfs is still reachable and writable, or -1.
	rcuExpedited int
}{shares: -1, rcuExpedited: -1}

// FIFREEZE, which x/sys/unix lacks: _IOWR('X', 119, int).
const fifreeze = 0xc0045877

// stopStorage makes sure what the container wrote that is meant to outlast
// it has reached the host. Nothing is written back for the scratch disk,
// which is thrown away: a global sync would write all of its dirty pages to
// the host just before it is deleted.
//
// Each volume's file system is frozen: the kernel writes back its data,
// commits and empties its journal, and marks it clean, as an unmount would.
// An unmount cannot be relied on, since the container may have moved or
// copied the volume's mounts, into namespaces of its own say, which keep its
// file system mounted. A freeze acts on the file system however it is
// mounted.
//
// Writes to bind mounts go to the host as they are made, except through
// shared memory mappings, whose dirty pages syncfs writes back.
func stopStorage() {
	if len(storage.volumes) > 0 && storage.rcuExpedited >= 0 {
		// A freeze waits for an RCU grace period at each of its three
		// levels, tens of milliseconds apiece in an idle guest. Expedited
		// grace periods take microseconds, and with the container gone
		// their cost to everything else no longer matters.
		unix.Write(storage.rcuExpedited, []byte("1"))
	}
	for i, fd := range storage.volumes {
		err := unix.IoctlSetInt(fd, fifreeze, 0)
		if err != nil && !errors.Is(err, unix.EBUSY) { // EBUSY: already frozen
			logf("freezing volume disk %d: %v; syncing it instead", i, err)
			unix.Syncfs(fd)
		}
	}
	if storage.shares >= 0 {
		if err := unix.Syncfs(storage.shares); err != nil {
			debugf("syncing the host's files: %v", err)
		}
	}
}
