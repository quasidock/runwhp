//go:build !windows

package container

import "os"

// OpenShared opens a file for reading.
func OpenShared(path string) (*os.File, error) { return os.Open(path) }
