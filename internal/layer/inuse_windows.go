package layer

import (
	"errors"

	"golang.org/x/sys/windows"
)

// inUse reports whether err is Windows refusing to remove a file someone has
// open without sharing delete.
func inUse(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }
