//go:build windows

package container

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"

	"github.com/quasidock/runwhp/internal/storefile"
	"golang.org/x/sys/windows"
)

// Records are read and replaced while other runwhps may be reading them,
// as package storefile allows.

// readShared reads a whole file, letting others replace it meanwhile.
func readShared(path string) ([]byte, error) { return storefile.Read(path) }

// OpenShared opens a file for reading, letting others replace or delete it
// meanwhile, as `logs -f` must not keep a container from being removed.
func OpenShared(path string) (*os.File, error) { return storefile.Open(path) }

// writeAtomic replaces the file at path with data, so readers see the old
// contents or the new, never part of either.
func writeAtomic(path string, data []byte) error { return storefile.Replace(path, data) }

// createWith makes a new file holding data, failing with fs.ErrExist if
// there is one. Readers never see it empty: it is written aside and linked
// into place, where the file system has hard links.
func createWith(path string, data []byte) error {
	var b [6]byte
	rand.Read(b[:])
	tmp := path + ".new-" + hex.EncodeToString(b[:])
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	err := os.Link(tmp, path)
	var le *os.LinkError
	if errors.As(err, &le) && (errors.Is(le.Err, windows.ERROR_ALREADY_EXISTS) || errors.Is(le.Err, windows.ERROR_FILE_EXISTS)) {
		return fs.ErrExist
	}
	if errors.As(err, &le) && (errors.Is(le.Err, windows.ERROR_NOT_SUPPORTED) || errors.Is(le.Err, windows.ERROR_INVALID_FUNCTION)) {
		return createNew(path, data)
	}
	return err
}

// createNew writes a new file, failing with fs.ErrExist if there is one.
func createNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
