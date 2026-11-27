package volfs

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/fuse"
)

// dirStream is an open directory's listing in progress: entries read from
// the kernel but not yet sent, and the offset the next read starts at.
type dirStream struct {
	mu      sync.Mutex
	pos     uint64 // offset of pending[0], or of the next getdents
	pending []dirent
	eof     bool
	buf     []byte
}

type dirent struct {
	ino  uint64
	off  uint64 // the offset after this entry
	typ  uint8
	name string
}

// fill reads the next batch of entries from the directory.
func (d *dirStream) fill(fd int) error {
	if d.buf == nil {
		d.buf = make([]byte, 32<<10)
	}
	n, err := unix.Getdents(fd, d.buf)
	if err != nil {
		return err
	}
	if n == 0 {
		d.eof = true
		return nil
	}
	b := d.buf[:n]
	for len(b) >= 19 {
		// struct linux_dirent64: ino, off, reclen, type, name.
		ino := *(*uint64)(unsafe.Pointer(&b[0]))
		off := *(*uint64)(unsafe.Pointer(&b[8]))
		reclen := int(*(*uint16)(unsafe.Pointer(&b[16])))
		if reclen < 19 || reclen > len(b) {
			break
		}
		typ := b[18]
		name := b[19:reclen]
		for i, c := range name {
			if c == 0 {
				name = name[:i]
				break
			}
		}
		d.pending = append(d.pending, dirent{ino: ino, off: off, typ: typ, name: string(name)})
		b = b[reclen:]
	}
	return nil
}

// dtMode is a getdents type as the type bits of a mode.
func dtMode(t uint8) uint32 { return uint32(t) << 12 }

func (s *Session) Readdir(c fuse.Context, id, fh, off uint64, plus bool, add func(fuse.DirEntry) bool) error {
	h, err := s.handle(fh)
	if err != nil {
		return err
	}
	d := h.dir
	if d == nil {
		return fuse.ENOTDIR
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if off != d.pos {
		// The kernel continues from another entry, or starts again.
		if _, err := unix.Seek(h.fd, int64(off), 0); err != nil {
			return errno(err)
		}
		d.pos, d.pending, d.eof = off, nil, false
	}
	sent := 0
	for {
		if len(d.pending) == 0 {
			if d.eof {
				return nil
			}
			if err := d.fill(h.fd); err != nil {
				if sent > 0 {
					return nil
				}
				return errno(err)
			}
			continue
		}
		e := d.pending[0]
		de := fuse.DirEntry{Name: e.name, Off: e.off}
		de.Entry.Attr.Ino = e.ino
		de.Entry.Attr.Mode = dtMode(e.typ)
		counted := false
		if plus && e.name != "." && e.name != ".." {
			got, err := s.entry(h.node, e.name)
			if err == nil {
				de.Entry = got
				counted = true
			}
			// A name gone since the listing is sent without its node, as
			// "." and ".." are.
		}
		if !add(de) {
			if counted {
				s.Forget(de.Entry.Node, 1)
			}
			return nil
		}
		sent++
		d.pending = d.pending[1:]
		d.pos = e.off
	}
}
