//go:build windows

package hostfs

import (
	"errors"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
)

// dataAccess is the access a file needs for open(2) flags.
func dataAccess(flags uint32) uint32 {
	access := uint32(statAccess)
	switch flags & fuse.OAccMode {
	case fuse.ORdonly:
		access |= windows.FILE_GENERIC_READ
	case fuse.OWronly:
		access |= windows.FILE_GENERIC_WRITE
	default:
		access |= windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	}
	return access
}

// openAccess is the access to open a file with for open(2) flags. With
// Writeback, a write-only open asks to read as well, since the guest reads
// the pages it writes in part; fallback is then the access to try should
// Windows refuse that, and 0 otherwise.
func (fs *FS) openAccess(flags uint32) (access, fallback uint32) {
	access = dataAccess(flags)
	if fs.Writeback && flags&fuse.OAccMode == fuse.OWronly {
		return dataAccess(flags&^fuse.OAccMode | fuse.ORdwr), access
	}
	return access, 0
}

func writes(flags uint32) bool {
	return flags&fuse.OAccMode != fuse.ORdonly || flags&fuse.OTrunc != 0
}

// openData opens a file for its data. Reparse points that are not links,
// such as cloud files, are opened through their file system filter, so their
// data is there; nothing else is ever followed.
func openData(p path, access uint32) (windows.Handle, error) {
	h, _, err := open(p, access, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, 0, nil)
	if err != nil {
		return 0, err
	}
	st, err := statHandle(h)
	if err != nil || st.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return h, nil
	}
	if st.isLink() || st.tag == tagAfUnix || st.tag == tagLxFIFO || st.tag == tagLxChr || st.tag == tagLxBlk {
		windows.CloseHandle(h)
		return 0, fuse.EINVAL
	}
	windows.CloseHandle(h)
	h, _, err = createFile(p, access, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, 0, nil)
	return h, err
}

func truncate(h windows.Handle, size uint64) error {
	b := buffer(8)
	le.PutUint64(b, size)
	return setInfo(h, classEndOfFile, b)
}

func (fs *FS) addHandle(h windows.Handle) uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fh := fs.nextFH
	fs.nextFH++
	fs.handles[fh] = &handle{h: h}
	return fh
}

func (fs *FS) file(fh uint64) (windows.Handle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	h := fs.handles[fh]
	if h == nil || h.h == 0 {
		return 0, fuse.EBADF
	}
	return h.h, nil
}

func (fs *FS) Open(c fuse.Context, id uint64, flags uint32) (uint64, error) {
	n, p, err := fs.getPath(id)
	if err != nil {
		return 0, err
	}
	if n == fs.root {
		return 0, fuse.EISDIR
	}
	if writes(flags) && n.share.ReadOnly {
		return 0, fuse.EROFS
	}
	access, fallback := fs.openAccess(flags)
	h, err := openData(p, access)
	if err != nil && fallback != 0 && errors.Is(errno(err), fuse.EACCES) {
		h, err = openData(p, fallback)
	}
	if err != nil {
		return 0, errno(err)
	}
	if flags&fuse.OTrunc != 0 {
		if err := truncate(h, 0); err != nil {
			windows.CloseHandle(h)
			return 0, errno(err)
		}
	}
	return fs.addHandle(h), nil
}

func (fs *FS) Create(c fuse.Context, parent uint64, name string, flags, mode uint32) (fuse.Entry, uint64, error) {
	p, target, err := fs.child(parent, name, true)
	if err != nil {
		return fuse.Entry{}, 0, err
	}
	disposition := uint32(windows.FILE_OPEN_IF)
	if flags&fuse.OExcl != 0 {
		disposition = windows.FILE_CREATE
	}
	var ea []byte
	if p.share.eas {
		ea = lxMetadata(c.UID, c.GID, fuse.ModeFile|mode&0o7777)
	}
	access, fallback := fs.openAccess(flags)
	h, created, err := open(target, access, disposition, windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, ea)
	if err != nil && fallback != 0 && errors.Is(errno(err), fuse.EACCES) {
		h, created, err = open(target, fallback, disposition, windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, ea)
	}
	if err != nil {
		return fuse.Entry{}, 0, errno(err)
	}
	if !created && flags&fuse.OTrunc != 0 {
		if err := truncate(h, 0); err != nil {
			windows.CloseHandle(h)
			return fuse.Entry{}, 0, errno(err)
		}
	}
	st, err := statHandle(h)
	if err != nil {
		windows.CloseHandle(h)
		return fuse.Entry{}, 0, errno(err)
	}
	return fs.entry(p, baseName(target), &st), fs.addHandle(h), nil
}

func (fs *FS) Read(c fuse.Context, id, fh uint64, off int64, buf []byte) (int, error) {
	h, err := fs.file(fh)
	if err != nil {
		return 0, err
	}
	n := 0
	for n < len(buf) {
		at := off + int64(n)
		ov := windows.Overlapped{Offset: uint32(at), OffsetHigh: uint32(at >> 32)}
		var done uint32
		err := windows.ReadFile(h, buf[n:], &done, &ov)
		n += int(done)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || (err == nil && done == 0) {
			break
		}
		if err != nil {
			return n, errno(err)
		}
	}
	return n, nil
}

func (fs *FS) Write(c fuse.Context, id, fh uint64, off int64, data []byte) (int, error) {
	h, err := fs.file(fh)
	if err != nil {
		return 0, err
	}
	n := 0
	for n < len(data) {
		at := off + int64(n)
		ov := windows.Overlapped{Offset: uint32(at), OffsetHigh: uint32(at >> 32)}
		var done uint32
		err := windows.WriteFile(h, data[n:], &done, &ov)
		n += int(done)
		if err != nil {
			return n, errno(err)
		}
		if done == 0 {
			return n, fuse.EIO
		}
	}
	return n, nil
}

func (fs *FS) Flush(c fuse.Context, id, fh uint64) error { return nil }

func (fs *FS) Fsync(c fuse.Context, id, fh uint64, datasync bool) error {
	h, err := fs.file(fh)
	if err != nil {
		return err
	}
	return errno(windows.FlushFileBuffers(h))
}

func (fs *FS) Release(id, fh uint64) {
	fs.mu.Lock()
	h := fs.handles[fh]
	delete(fs.handles, fh)
	fs.mu.Unlock()
	if h != nil && h.h != 0 {
		windows.CloseHandle(h.h)
	}
}
