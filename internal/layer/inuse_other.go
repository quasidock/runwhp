//go:build !windows

package layer

// inUse reports whether err means a file is open elsewhere, which only
// Windows refuses a removal for.
func inUse(error) bool { return false }
