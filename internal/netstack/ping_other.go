//go:build !windows

package netstack

// systemPinger is nil where runwhp has no unelevated way to ping: only the
// stack's own addresses answer.
func systemPinger() Pinger { return nil }
