//go:build windows

package container

import (
	"os"

	"github.com/quasidock/runwhp/internal/storefile"
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
// there is one, as storefile.Create does.
func createWith(path string, data []byte) error { return storefile.Create(path, data) }
