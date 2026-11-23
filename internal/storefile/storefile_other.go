//go:build !windows

package storefile

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

// TempSuffix starts the name Replace gives a file it writes aside, after
// the name of the file it replaces.
const TempSuffix = ".tmp-"

// Read reads a whole file. Elsewhere than on Windows, a file open for
// reading can be replaced anyway.
func Read(path string) ([]byte, error) { return os.ReadFile(path) }

// Open opens a file for reading.
func Open(path string) (*os.File, error) { return os.Open(path) }

// Replace replaces the file at path with data, so readers see the old
// contents or the new, never part of either.
func Replace(path string, data []byte) error {
	var b [6]byte
	rand.Read(b[:])
	tmp := path + TempSuffix + hex.EncodeToString(b[:])
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
