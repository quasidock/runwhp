//go:build windows

package hostfs

import (
	"errors"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
)

// dirHandle is an open directory. Its listing is taken when the guest reads
// from offset 0, and later reads continue through it, so offsets stay valid
// while the directory changes.
type dirHandle struct {
	mu   sync.Mutex
	node *node
	self fileStat
	ents []dirEntry
	read bool
}

type dirEntry struct {
	host string // the name on the host
	st   fileStat
	// full is set when the listing lacks something a stat reports, such as
	// WSL metadata, and READDIRPLUS should stat the file.
	full  bool
	share *share // set for the entries of the root of all shares
}

func (fs *FS) Opendir(c fuse.Context, id uint64) (uint64, error) {
	n, p, err := fs.getPath(id)
	if err != nil {
		return 0, err
	}
	d := &dirHandle{node: n}
	if n != fs.root {
		st, err := statPath(p)
		if err != nil {
			return 0, errno(err)
		}
		if !st.isDir() || st.isLink() {
			return 0, fuse.ENOTDIR
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fh := fs.nextFH
	fs.nextFH++
	fs.handles[fh] = &handle{dir: d}
	return fh, nil
}

func (fs *FS) Releasedir(id, fh uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.handles, fh)
}

func (fs *FS) dir(fh uint64) (*dirHandle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	h := fs.handles[fh]
	if h == nil || h.dir == nil {
		return nil, fuse.EBADF
	}
	return h.dir, nil
}

func (fs *FS) Readdir(c fuse.Context, id, fh, off uint64, plus bool, add func(fuse.DirEntry) bool) error {
	d, err := fs.dir(fh)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var dirPath path
	if d.node != fs.root {
		fs.mu.Lock()
		dirPath = fs.pathLocked(d.node)
		fs.mu.Unlock()
	}
	if off == 0 || !d.read {
		if err := fs.list(d, dirPath); err != nil {
			return err
		}
	}
	dotIno := d.self.key.ino()
	if d.node == fs.root {
		dotIno = fuse.RootID
	}
	for i := off; i < uint64(2+len(d.ents)); i++ {
		de := fuse.DirEntry{Off: i + 1}
		if i < 2 {
			de.Name = ".."[:i+1]
			de.Entry.Attr = fuse.Attr{Ino: dotIno, Mode: fuse.ModeDir}
			if !add(de) {
				return nil
			}
			continue
		}
		e := &d.ents[i-2]
		st := e.st
		if plus && e.full && e.share == nil {
			if full, err := statPath(dirPath.child(e.host)); err == nil {
				st = full
			}
		}
		de.Name = guestName(e.host)
		de.Entry.Attr = fs.attr(&st)
		if !plus {
			if !add(de) {
				return nil
			}
			continue
		}
		// Count the lookup only if the entry made it into the reply.
		fs.mu.Lock()
		var n *node
		if e.share != nil {
			n = e.share.node
		} else {
			n = fs.attachLocked(d.node, e.host, &st)
		}
		de.Entry.Node = n.id
		ok := add(de)
		if ok {
			n.lookups++
		} else {
			fs.reapLocked(n)
		}
		fs.mu.Unlock()
		if !ok {
			return nil
		}
	}
	return nil
}

// list takes a directory's listing.
func (fs *FS) list(d *dirHandle, p path) error {
	d.read = true
	d.ents = d.ents[:0]
	if d.node == fs.root {
		for i, sh := range fs.shares {
			st, err := statPath(sh.root)
			if err != nil {
				fs.logf("share %d (%s): %v", i, sh.Path, err)
				continue
			}
			d.ents = append(d.ents, dirEntry{host: strconv.Itoa(i), st: st, share: sh})
		}
		return nil
	}
	h, _, err := open(p, windows.FILE_LIST_DIRECTORY|statAccess, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, 0, nil)
	if err != nil {
		return errno(err)
	}
	defer windows.CloseHandle(h)
	if d.self, err = statHandle(h); err != nil {
		return errno(err)
	}
	buf := buffer(64 << 10)
	for {
		// FILE_ID_BOTH_DIR_INFO, whose ids are the ones stats report.
		err := windows.GetFileInformationByHandleEx(h, windows.FileIdBothDirectoryInfo, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil
		}
		if err != nil {
			return errno(err)
		}
		for off := 0; ; {
			e := buf[off:]
			next := le.Uint32(e[0:])
			nameLen := le.Uint32(e[60:]) / 2
			name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&e[104])), nameLen))
			if name != "." && name != ".." {
				ent := dirEntry{host: name}
				st := &ent.st
				st.created = int64(le.Uint64(e[8:]))
				st.accessed = int64(le.Uint64(e[16:]))
				st.written = int64(le.Uint64(e[24:]))
				st.changed = int64(le.Uint64(e[32:]))
				st.size = int64(le.Uint64(e[40:]))
				st.alloc = int64(le.Uint64(e[48:]))
				st.attrs = le.Uint32(e[56:])
				st.nlink = 1
				id := le.Uint64(e[96:])
				le.PutUint64(st.key[:], id)
				// EaSize holds a reparse point's tag instead, so a link or
				// special file, which may have metadata too, is stat'ed in
				// full, as is a file whose id the listing cannot give.
				if st.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
					st.tag = le.Uint32(e[64:])
					ent.full = true
				} else {
					ent.full = le.Uint32(e[64:]) > 0
				}
				keep := true
				if !validID(id) {
					full, err := statPath(p.child(name))
					ent.st, ent.full = full, false
					keep = err == nil // else gone since the listing
				}
				if keep {
					d.ents = append(d.ents, ent)
				}
			}
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
}
