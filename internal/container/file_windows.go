//go:build windows

package container

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Records are read and replaced while other runwhps may be reading them.
// os.Open shares a file for reading and writing but not deleting, and
// Windows will not replace a file open without FILE_SHARE_DELETE, so
// readers open with it, and writers replace with a POSIX-semantics rename,
// which replaces a file others have open as Linux would.

// readShared reads a whole file, letting others replace it meanwhile.
func readShared(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			err = fs.ErrNotExist
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}

// OpenShared opens a file for reading, letting others replace or delete it
// meanwhile, as `logs -f` must not keep a container from being removed.
func OpenShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			err = fs.ErrNotExist
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

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

// writeAtomic replaces the file at path with data, so readers see the old
// contents or the new, never part of either.
func writeAtomic(path string, data []byte) error {
	var b [6]byte
	rand.Read(b[:])
	tmp := path + ".tmp-" + hex.EncodeToString(b[:])
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	var err error
	for try := 0; try < 5; try++ {
		if err = replace(tmp, path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond) // an antivirus scanner, say
	}
	os.Remove(tmp)
	return &fs.PathError{Op: "replace", Path: path, Err: err}
}

const (
	fileRenameInfoEx      = 22 // FileRenameInfoEx
	renameReplaceIfExists = 0x1
	renamePosixSemantics  = 0x2
)

// replace renames from over to with POSIX semantics, falling back to an
// ordinary rename on file systems without them, such as FAT.
func replace(from, to string) error {
	p, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer func() {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}()
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	// FILE_RENAME_INFORMATION: ULONG Flags; HANDLE RootDirectory;
	// ULONG FileNameLength; WCHAR FileName[].
	const nameOffset = 20
	buf := make([]uint64, (nameOffset+2*len(name)+2+7)/8) // 8-byte aligned, as NT wants
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(buf)*8)
	*(*uint32)(unsafe.Pointer(&b[0])) = renameReplaceIfExists | renamePosixSemantics
	*(*uint32)(unsafe.Pointer(&b[16])) = uint32(2 * len(name))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&b[nameOffset])), len(name)), name)
	err = windows.SetFileInformationByHandle(h, fileRenameInfoEx, &b[0], uint32(len(b)))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		windows.CloseHandle(h)
		h = 0
		return os.Rename(from, to)
	}
	return err
}
