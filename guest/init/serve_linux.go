package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// serveLayers is init's work in a guest booted with Spec.Serve set to
// ServeLayers, for runwhp save: it mounts each layer on its own, read-only,
// and serves their files on guestspec.ArchivePort until the host stops
// the machine.
func serveLayers(spec *guestspec.Spec, ctl *controller) error {
	serials := make([]string, spec.Layers)
	for i := range serials {
		serials[i] = guestspec.LayerSerial(i)
	}
	devices, err := waitDisks(serials)
	if err != nil {
		return err
	}
	roots := make([]string, spec.Layers)
	for i := range roots {
		if roots[i], err = mountLayer(i, devices[guestspec.LayerSerial(i)]); err != nil {
			return err
		}
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guestspec.ArchivePort}); err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	debugf("serving %d layers", len(roots))
	ctl.send(guestspec.Event{Ready: true})
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
			continue
		}
		if err != nil {
			return fmt.Errorf("vsock: accept: %w", err)
		}
		go func() {
			conn := os.NewFile(uintptr(nfd), "vsock")
			defer conn.Close()
			if err := serveFiles(conn, conn, roots); err != nil {
				debugf("serving files: %v", err)
			}
		}()
	}
}

// serveFiles answers one files request read from r, writing to w.
func serveFiles(r io.Reader, w io.Writer, roots []string) error {
	layer, names, err := guestspec.ReadFilesRequest(bufio.NewReaderSize(r, 64<<10))
	if err != nil {
		return err
	}
	if layer < 0 || layer >= len(roots) {
		return fmt.Errorf("files request for layer %d of %d", layer, len(roots))
	}
	dir, err := os.Open(roots[layer])
	if err != nil {
		return err
	}
	defer dir.Close()
	bw := bufio.NewWriterSize(w, 256<<10)
	for _, name := range names {
		if err := serveFile(bw, int(dir.Fd()), name); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// serveFile writes one file's header and contents, or an error the host
// reports. It returns an error only when the connection fails.
func serveFile(w io.Writer, dirfd int, name string) error {
	f, size, err := openBeneath(dirfd, name)
	if err != nil {
		return guestspec.WriteFileError(w, fmt.Sprintf("%q: %v", name, err))
	}
	defer f.Close()
	if err := guestspec.WriteFileHeader(w, size); err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err == nil && n < size {
		err = fmt.Errorf("%q: %w", name, io.ErrUnexpectedEOF)
	}
	// A short file cannot be told apart from the next one's header now:
	// the connection ends, and the host reports it.
	return err
}

// openBeneath opens a regular file at name beneath dirfd, cleaned as
// tar2ext4 cleans names, following no symlink on the way: tar2ext4 cannot
// store a file beneath one, so a real name never needs it.
func openBeneath(dirfd int, name string) (*os.File, int64, error) {
	clean := path.Clean("/" + name)[1:]
	if clean == "" {
		return nil, 0, errors.New("not a file")
	}
	fd, err := unix.Openat2(dirfd, clean, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(fd), clean)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, 0, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		f.Close()
		return nil, 0, errors.New("not a regular file")
	}
	return f, st.Size, nil
}

// serveRoot is init's work in a guest booted with Spec.Serve set to
// ServeRoot, for runwhp cp, export, diff and commit on a stopped
// container: it prepares the container's root, its mounts and its /etc
// files as for a run, serves them on guestspec.RootPort, and waits for the
// host to send Control.Shutdown. It runs no process, and has no network.
func serveRoot(spec *guestspec.Spec, ctl *controller) error {
	if err := mountRoot(spec); err != nil {
		return err
	}
	if err := prepareRoot(spec); err != nil {
		return err
	}
	user, err := resolveUser(spec)
	if err != nil {
		return err
	}
	if err := mountAll(spec, user); err != nil {
		return err
	}
	if err := bindEtc(spec); err != nil {
		return err
	}
	if err := enterRoot(); err != nil {
		return err
	}
	startRootServer(spec, user)
	debugf("serving the container's root")
	ctl.send(guestspec.Event{Ready: true})
	ctl.waitShutdown()
	debugf("shutting down")
	return nil
}
