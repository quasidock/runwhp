package guestspec

import "fmt"

// Shared volumes.
//
// A volume several containers use at once is served by a guest of its own:
// one booted with Spec.Serve set to ServeVolume and one volume disk, whose
// init mounts the disk's file system and serves it over FUSE on
// VolumePort, each connection a session of its own carrying frames as
// package fuse's ServeStream reads them, until the host sends
// Control.Shutdown. A container using the volume gets, instead of the
// disk, a virtio-fs device with the tag VolumeFSTag(i), whose requests its
// runwhp passes on to the serving guest; Spec.SharedVolumes lists those
// volumes.

// ServeVolume is Spec.Serve for a guest that serves a shared volume.
const ServeVolume = "volume"

// VolumePort is the vsock port on which a guest booted with ServeVolume
// serves its volume.
const VolumePort = 1029

// VolumeFSTag is the mount tag of the virtio-fs device carrying volume i,
// when it is shared.
func VolumeFSTag(i int) string { return fmt.Sprintf("runwhp-vol%d", i) }
