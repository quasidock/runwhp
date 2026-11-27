//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/volfs"
)

// sharedVolumeDir is where a guest serving a volume mounts its file system.
const sharedVolumeDir = "/volume"

// serveVolume is init's work in a guest booted with Spec.Serve set to
// ServeVolume: it mounts its one volume disk and serves the file system to
// the containers using the volume, each connection to
// guestspec.VolumePort a session of its own, until the host sends
// Control.Shutdown. shutdown then freezes the file system, as a
// container's guest freezes its volumes.
func serveVolume(spec *guestspec.Spec, ctl *controller) error {
	// The guests asking apply their processes' umasks themselves.
	syscall.Umask(0)
	devices, err := waitDisks([]string{guestspec.VolumeSerial(0)})
	if err != nil {
		return err
	}
	dev := devices[guestspec.VolumeSerial(0)]
	if err := os.MkdirAll(sharedVolumeDir, 0o755); err != nil {
		return err
	}
	openRCUExpedited()
	replay := needsRecovery(dev)
	if err := syscall.Mount(dev, sharedVolumeDir, "ext4", 0, volumeMountOpts); err != nil {
		return fmt.Errorf("mounting the volume disk (%s): %w", dev, err)
	}
	if replay {
		logf("the volume's journal was replayed: it was last used by a guest that did not stop cleanly")
	}
	fd, err := syscall.Open(sharedVolumeDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	storage.volumes = append(storage.volumes, fd)
	vol, err := volfs.Open(sharedVolumeDir)
	if err != nil {
		return err
	}
	mem := openMemFiles()
	ctl.mem.Store(mem)
	go mem.reclaimLoop(spec.Reclaim, nil)

	lfd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Bind(lfd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guestspec.VolumePort}); err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Listen(lfd, 64); err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	go func() {
		for {
			nfd, _, err := unix.Accept4(lfd, unix.SOCK_CLOEXEC)
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			if err != nil {
				logf("vsock: accept: %v", err)
				return
			}
			// A buffer larger than vsock's 256 KiB keeps large reads
			// and writes flowing while each side handles what came
			// before.
			unix.SetsockoptUint64(nfd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, vsockBuffer)
			unix.SetsockoptUint64(nfd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_SIZE, vsockBuffer)
			// Nonblocking, so that Go's poller waits for it rather than
			// a thread.
			if err := unix.SetNonblock(nfd, true); err != nil {
				unix.Close(nfd)
				continue
			}
			go serveSession(vol, os.NewFile(uintptr(nfd), "vsock"))
		}
	}()
	debugf("serving the volume")
	ctl.send(guestspec.Event{Ready: true})
	ctl.waitShutdown()
	debugf("shutting down")
	return nil
}

// serveSession serves one container's requests until its connection ends.
func serveSession(vol *volfs.Volume, conn *os.File) {
	defer conn.Close()
	start := time.Now()
	s := vol.NewSession()
	srv := &fuse.Server{FS: s, TimeGran: 1, NoKeepCache: true, Logf: debugf}
	err := fuse.ServeStream(conn, srv, s.Close)
	s.Close()
	if err != nil {
		debugf("session of %v ended: %v", time.Since(start).Round(time.Millisecond), err)
	}
	if debug {
		debugf("%s", srv.Stats())
	}
}
