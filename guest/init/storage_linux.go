//go:build linux

package main

import (
	"golang.org/x/sys/unix"
)

// storage holds what stopStorage needs, which enterRoot hides from paths:
// file descriptors on file systems whose data must reach the host before
// the guest resets.
var storage = struct {
	shares int // the virtio-fs mount, or -1
}{shares: -1}

// stopStorage makes sure what the container wrote that is meant to outlast
// it has reached the host. Nothing is written back for the scratch disk,
// which is thrown away: a global sync would write all of its dirty pages to
// the host just before it is deleted.
//
// Writes to bind mounts go to the host as they are made, except through
// shared memory mappings, whose dirty pages syncfs writes back.
func stopStorage() {
	if storage.shares >= 0 {
		if err := unix.Syncfs(storage.shares); err != nil {
			debugf("syncing the host's files: %v", err)
		}
	}
}
