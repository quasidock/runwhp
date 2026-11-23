package gc

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockFile is the file whose byte range runwhps lock, in the store's root.
// It is made when first needed and never removed.
const lockFile = "store.lock"

// Hold takes the store's lock shared, for a runwhp that is about to add
// what a prune would remove: a pull, until its image is recorded, and a
// run or create, from resolving its image until its container is recorded.
// Any number of runwhps can hold it at once. A prune waits for them all.
func Hold(root string) (release func(), err error) {
	return lock(root, 0, nil)
}

// Exclusive takes the store's lock for a prune, waiting for every runwhp
// that holds it shared. waiting, if not nil, is called once if it has to
// wait.
func Exclusive(root string, waiting func()) (release func(), err error) {
	return lock(root, windows.LOCKFILE_EXCLUSIVE_LOCK, waiting)
}

func lock(root string, flags uint32, waiting func()) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(filepath.Join(root, lockFile))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(root, lockFile), Err: err}
	}
	// The handle is synchronous, so LockFileEx returns once the lock is
	// taken; the OVERLAPPED only gives the range's offset, 0. Windows lets
	// go of the lock when the process ends, however it ends.
	err = windows.LockFileEx(h, flags|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		if waiting != nil {
			waiting()
		}
		err = windows.LockFileEx(h, flags, 0, 1, 0, new(windows.Overlapped))
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "lock", Path: filepath.Join(root, lockFile), Err: err}
	}
	return func() {
		windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		windows.CloseHandle(h)
	}, nil
}
