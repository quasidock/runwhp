// Package hostfs serves host directories and files to the guest as a FUSE
// file system, for virtio-fs. It is the host end of runwhp's bind mounts.
//
// Each share is a host directory or file, and they appear as the entries
// "0", "1", ... of a read-only root. The server never follows a link on the
// host, and every name it is given is a single path component, so the guest
// reaches nothing outside its shares.
//
// Windows has no Unix owners or modes. Like WSL's DrvFS with its "metadata"
// option, hostfs keeps them in the extended attributes $LXUID, $LXGID and
// $LXMOD, keeps symlinks as WSL symlink reparse points, and keeps FIFOs,
// sockets and device nodes as WSL's other reparse points. WSL and runwhp
// therefore see the same files the same way.
package hostfs

import (
	"strings"
	"unicode/utf8"

	"github.com/quasidock/runwhp/internal/fuse"
)

// maxName is the longest name, in bytes, Linux allows.
const maxName = 255

// escapeBase is where characters Windows forbids in names are moved to, in
// Unicode's private use area, as WSL does.
const escapeBase = 0xf000

// forbidden reports whether Windows refuses c in a file name.
func forbidden(c rune) bool {
	if c > 0 && c < 0x20 {
		return true
	}
	switch c {
	case '"', '*', ':', '<', '>', '?', '\\', '|':
		return true
	}
	return false
}

// hostName turns a name the guest gave into the name of the host file. It
// refuses anything but one path component.
func hostName(name string) (string, error) {
	switch {
	case name == "" || name == "." || name == "..":
		return "", fuse.EINVAL
	case len(name) > maxName:
		return "", fuse.ENAMETOOLONG
	case strings.ContainsAny(name, "/\x00"), !utf8.ValidString(name):
		return "", fuse.EINVAL
	}
	if strings.IndexFunc(name, forbidden) < 0 {
		return name, nil
	}
	var b strings.Builder
	for _, c := range name {
		if forbidden(c) {
			c += escapeBase
		}
		b.WriteRune(c)
	}
	return b.String(), nil
}

// guestName turns the name of a host file into the name the guest sees.
func guestName(name string) string {
	unescape := func(c rune) bool { return c > escapeBase && c < escapeBase+0x80 && forbidden(c-escapeBase) }
	if strings.IndexFunc(name, unescape) < 0 {
		return name
	}
	var b strings.Builder
	for _, c := range name {
		if unescape(c) {
			c -= escapeBase
		}
		b.WriteRune(c)
	}
	return b.String()
}
