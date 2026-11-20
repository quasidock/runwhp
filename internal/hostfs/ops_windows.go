//go:build windows

package hostfs

import (
	"errors"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
)

func (fs *FS) Setattr(c fuse.Context, id uint64, in fuse.SetattrIn) (fuse.Attr, error) {
	n, p, err := fs.getPath(id)
	if err != nil {
		return fuse.Attr{}, err
	}
	if n == fs.root || n.share.ReadOnly {
		return fuse.Attr{}, fuse.EROFS
	}
	const timeBits = fuse.SetAtime | fuse.SetMtime | fuse.SetAtimeNow | fuse.SetMtimeNow
	access := uint32(statAccess)
	if in.Valid&fuse.SetSize != 0 {
		access |= windows.FILE_WRITE_DATA
	}
	if in.Valid&(timeBits|fuse.SetMode) != 0 {
		access |= windows.FILE_WRITE_ATTRIBUTES
	}
	meta := in.Valid&(fuse.SetMode|fuse.SetUID|fuse.SetGID) != 0 && n.share.eas
	if meta {
		access |= windows.FILE_WRITE_EA
	}
	h, _, err := open(p, access, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		if in.Valid&fuse.SetHandle == 0 || !errors.Is(errno(err), fuse.ENOENT) {
			return fuse.Attr{}, errno(err)
		}
		// The file is open but no longer has a name, as after unlink:
		// change it through the guest's handle.
		fh, ferr := fs.file(in.Handle)
		if ferr != nil {
			return fuse.Attr{}, errno(err)
		}
		if h, err = dup(fh); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	defer windows.CloseHandle(h)
	st, err := statHandle(h)
	if err != nil {
		return fuse.Attr{}, errno(err)
	}
	cur := fs.attr(&st)

	if in.Valid&fuse.SetMode != 0 && cur.Mode&fuse.ModeType == fuse.ModeFile {
		// The owner's write bit is also the read-only attribute, which is
		// all Windows programs see. Clear it before writing the metadata,
		// which it would otherwise forbid, and set it after.
		readOnly := in.Mode&0o200 == 0
		if !readOnly && st.attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			if err := setAttrs(h, st.attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
				return fuse.Attr{}, errno(err)
			}
		}
		defer func() {
			if readOnly && st.attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
				if err := setAttrs(h, st.attrs|windows.FILE_ATTRIBUTE_READONLY); err != nil {
					fs.logf("setting the read-only attribute: %v", err)
				}
			}
		}()
	}
	if meta {
		var eas []ea
		if in.Valid&fuse.SetUID != 0 {
			eas = append(eas, eaU32("$LXUID", in.UID))
		}
		if in.Valid&fuse.SetGID != 0 {
			eas = append(eas, eaU32("$LXGID", in.GID))
		}
		if in.Valid&fuse.SetMode != 0 {
			eas = append(eas, eaU32("$LXMOD", cur.Mode&fuse.ModeType|in.Mode&0o7777))
		} else if st.lxFlags&lxHasMode == 0 {
			// Keep the mode the file showed before it had an owner of
			// its own; chown(2) clears the set-id bits, which defaults
			// never have.
			eas = append(eas, eaU32("$LXMOD", cur.Mode))
		}
		if err := setEA(h, eaBuffer(eas...)); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if in.Valid&fuse.SetSize != 0 {
		if err := truncate(h, in.Size); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if in.Valid&timeBits != 0 {
		now := time.Now()
		var accessed, written int64
		switch {
		case in.Valid&fuse.SetAtimeNow != 0:
			accessed = filetime(now)
		case in.Valid&fuse.SetAtime != 0:
			accessed = filetime(in.Atime)
		}
		switch {
		case in.Valid&fuse.SetMtimeNow != 0:
			written = filetime(now)
		case in.Valid&fuse.SetMtime != 0:
			written = filetime(in.Mtime)
		}
		if err := setBasic(h, accessed, written, 0); err != nil {
			return fuse.Attr{}, errno(err)
		}
	}
	if st, err = statHandle(h); err != nil {
		return fuse.Attr{}, errno(err)
	}
	a := fs.attr(&st)
	if in.Valid&fuse.SetMode != 0 && !n.share.eas {
		a.Mode = a.Mode&fuse.ModeType | in.Mode&0o7777 // what a cached inode should hold until it expires
	}
	return a, nil
}

func setAttrs(h windows.Handle, attrs uint32) error {
	if attrs == 0 {
		attrs = windows.FILE_ATTRIBUTE_NORMAL
	}
	return setBasic(h, 0, 0, int64(attrs))
}

func dup(h windows.Handle) (windows.Handle, error) {
	p := windows.CurrentProcess()
	var d windows.Handle
	err := windows.DuplicateHandle(p, h, p, &d, 0, false, windows.DUPLICATE_SAME_ACCESS)
	return d, err
}

// create makes a new file or directory with WSL metadata, runs then on it,
// and counts a lookup of it. If then fails, the file is removed again.
func (fs *FS) create(c fuse.Context, parent uint64, name string, mode uint32, rdev []uint32,
	then func(windows.Handle) error) (fuse.Entry, error) {
	p, target, err := fs.child(parent, name, true)
	if err != nil {
		return fuse.Entry{}, err
	}
	var ea []byte
	if p.share.eas {
		ea = lxMetadata(c.UID, c.GID, mode, rdev...)
	}
	options, attrs := uint32(windows.FILE_NON_DIRECTORY_FILE), uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if mode&fuse.ModeType == fuse.ModeDir {
		options, attrs = windows.FILE_DIRECTORY_FILE, 0
	}
	access := uint32(statAccess | windows.DELETE)
	if then != nil {
		access |= windows.FILE_WRITE_DATA | windows.FILE_WRITE_ATTRIBUTES
	}
	h, _, err := open(target, access, windows.FILE_CREATE, options, attrs, ea)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	defer windows.CloseHandle(h)
	if then != nil {
		if err := then(h); err != nil {
			if derr := remove(h); derr != nil {
				fs.logf("removing %s after a failed create: %v", target, derr)
			}
			return fuse.Entry{}, errno(err)
		}
	}
	st, err := statHandle(h)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	return fs.entry(p, baseName(target), &st), nil
}

func (fs *FS) Mkdir(c fuse.Context, parent uint64, name string, mode uint32) (fuse.Entry, error) {
	return fs.create(c, parent, name, fuse.ModeDir|mode&0o7777, nil, nil)
}

func (fs *FS) Mknod(c fuse.Context, parent uint64, name string, mode, rdev uint32) (fuse.Entry, error) {
	var tag uint32
	var dev []uint32
	switch mode & fuse.ModeType {
	case fuse.ModeFile:
		return fs.create(c, parent, name, mode, nil, nil)
	case fuse.ModeFIFO:
		tag = tagLxFIFO
	case fuse.ModeSocket:
		tag = tagAfUnix
	case fuse.ModeChar, fuse.ModeBlock:
		tag = tagLxChr
		if mode&fuse.ModeType == fuse.ModeBlock {
			tag = tagLxBlk
		}
		major, minor := decodeDev(rdev)
		dev = []uint32{major, minor}
	default:
		return fuse.Entry{}, fuse.EINVAL
	}
	return fs.create(c, parent, name, mode, dev, func(h windows.Handle) error { return setReparse(h, tag, nil) })
}

func (fs *FS) Symlink(c fuse.Context, parent uint64, name, target string) (fuse.Entry, error) {
	data := append(le.AppendUint32(nil, 2), target...) // WSL's format, version 2
	return fs.create(c, parent, name, fuse.ModeLink|0o777, nil, func(h windows.Handle) error {
		return setReparse(h, tagLxSymlink, data)
	})
}

func (fs *FS) Readlink(c fuse.Context, id uint64) (string, error) {
	n, p, err := fs.getPath(id)
	if err != nil {
		return "", err
	}
	if n == fs.root {
		return "", fuse.EINVAL
	}
	h, _, err := open(p, statAccess, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		return "", errno(err)
	}
	defer windows.CloseHandle(h)
	tag, data, err := reparse(h)
	if err != nil {
		return "", errno(err)
	}
	switch tag {
	case tagLxSymlink:
		if len(data) < 4 {
			return "", fuse.EIO
		}
		return string(data[4:]), nil
	case tagSymlink, tagMountPoint:
		target, relative, ok := windowsLink(tag, data)
		if !ok {
			return "", fuse.EIO
		}
		if relative {
			return guestPath(target), nil
		}
		return fs.translate(target), nil
	}
	return "", fuse.EINVAL
}

// windowsLink decodes a Windows symlink's or junction's target.
func windowsLink(tag uint32, data []byte) (target string, relative, ok bool) {
	head := 8 // a junction has no flags
	if tag == tagSymlink {
		head = 12
	}
	if len(data) < head {
		return "", false, false
	}
	str := func(off, n uint16) (string, bool) {
		start, end := head+int(off), head+int(off)+int(n)
		if n%2 != 0 || end > len(data) {
			return "", false
		}
		u := make([]uint16, n/2)
		for i := range u {
			u[i] = le.Uint16(data[start+2*i:])
		}
		return string(utf16.Decode(u)), true
	}
	sub, ok1 := str(le.Uint16(data[0:]), le.Uint16(data[2:]))
	print, ok2 := str(le.Uint16(data[4:]), le.Uint16(data[6:]))
	if !ok1 || !ok2 {
		return "", false, false
	}
	relative = tag == tagSymlink && le.Uint32(data[8:])&1 != 0
	if print != "" {
		return print, relative, true
	}
	return sub, relative, true
}

// guestPath turns a relative Windows path into a Linux one.
func guestPath(win string) string {
	parts := strings.Split(win, `\`)
	for i, p := range parts {
		parts[i] = guestName(p)
	}
	return strings.Join(parts, "/")
}

// translate turns an absolute Windows path into the path the guest sees it
// at, when it lies in a directory share. Any other path is shown as it is on
// Windows, which in the guest leads nowhere.
func (fs *FS) translate(win string) string {
	strip := func(s string) string {
		for _, prefix := range []string{`\??\UNC\`, `\\?\UNC\`} {
			if strings.HasPrefix(s, prefix) {
				return `\\` + s[len(prefix):]
			}
		}
		for _, prefix := range []string{`\??\`, `\\?\`} {
			if strings.HasPrefix(s, prefix) {
				return s[len(prefix):]
			}
		}
		return s
	}
	p := strip(win)
	for _, sh := range fs.shares {
		if sh.File || sh.Target == "" {
			continue
		}
		root := strip(sh.win)
		switch {
		case strings.EqualFold(p, root):
			return sh.Target
		case len(p) > len(root) && p[len(root)] == '\\' && strings.EqualFold(p[:len(root)], root):
			return strings.TrimSuffix(sh.Target, "/") + "/" + guestPath(p[len(root)+1:])
		}
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// remove deletes the file h, with POSIX semantics where the file system has
// them: the name goes at once, even while the file is open.
func remove(h windows.Handle) error {
	b := buffer(4)
	le.PutUint32(b, windows.FILE_DISPOSITION_DELETE|windows.FILE_DISPOSITION_POSIX_SEMANTICS|
		windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	err := setInfo(h, classDispositionX, b)
	if err == nil || !unsupported(err) {
		return err
	}
	b = buffer(1) // FILE_DISPOSITION_INFORMATION
	b[0] = 1      // DeleteFile
	return setInfo(h, classDisposition, b)
}

func (fs *FS) unlink(parent uint64, name string, dir bool) error {
	p, target, err := fs.child(parent, name, true)
	if err != nil {
		return err
	}
	h, _, err := open(target, statAccess|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		return errno(err)
	}
	defer windows.CloseHandle(h)
	st, err := statHandle(h)
	if err != nil {
		return errno(err)
	}
	isDir := st.isDir() && !st.isLink()
	switch {
	case dir && !isDir:
		return fuse.ENOTDIR
	case !dir && isDir:
		return fuse.EISDIR
	}
	if err := remove(h); err != nil {
		return errno(err)
	}
	fs.mu.Lock()
	fs.forgetKeyLocked(p.share, &st)
	fs.mu.Unlock()
	return nil
}

func (fs *FS) Unlink(c fuse.Context, parent uint64, name string) error {
	return fs.unlink(parent, name, false)
}

func (fs *FS) Rmdir(c fuse.Context, parent uint64, name string) error {
	return fs.unlink(parent, name, true)
}

func (fs *FS) Rename(c fuse.Context, parent uint64, name string, newParent uint64, newName string, flags uint32) error {
	if flags&^fuse.RenameNoreplace != 0 {
		return fuse.EINVAL // RENAME_EXCHANGE and RENAME_WHITEOUT
	}
	p, src, err := fs.child(parent, name, true)
	if err != nil {
		return err
	}
	np, dst, err := fs.child(newParent, newName, true)
	if err != nil {
		return err
	}
	if p.share != np.share {
		return fuse.EXDEV
	}
	h, _, err := open(src, statAccess|windows.DELETE, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		return errno(err)
	}
	defer windows.CloseHandle(h)
	st, err := statHandle(h)
	if err != nil {
		return errno(err)
	}
	old, oldErr := statPath(dst) // the file the rename replaces, if any
	replace := flags&fuse.RenameNoreplace == 0
	if oldErr == nil && old.key != st.key && replace && old.isDir() != st.isDir() {
		if st.isDir() {
			return fuse.ENOTDIR
		}
		return fuse.EISDIR
	}
	f := uint32(windows.FILE_RENAME_POSIX_SEMANTICS | windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE)
	if replace {
		f |= windows.FILE_RENAME_REPLACE_IF_EXISTS
	}
	err = setInfo(h, classRenameX, nameInfo(f, dst))
	if err != nil && unsupported(err) {
		var b uint32
		if replace {
			b = 1
		}
		err = setInfo(h, classRename, nameInfo(b, dst))
	}
	if err != nil && oldErr == nil && old.isDir() && st.isDir() && replace && old.key != st.key {
		// Linux replaces an empty directory; Windows refuses to.
		if rerr := fs.removeEmptyDir(dst); rerr != nil {
			return rerr
		}
		err = setInfo(h, classRenameX, nameInfo(f, dst))
	}
	if err != nil {
		if errors.Is(errno(err), fuse.EEXIST) || (!replace && oldErr == nil) {
			return fuse.EEXIST
		}
		return errno(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if oldErr == nil && old.key != st.key {
		fs.forgetKeyLocked(p.share, &old)
	}
	if n := fs.byKey[nodeKey{p.share, st.key}]; n != nil && n.parent != nil {
		if n.parent != np {
			n.parent.kids--
			np.kids++
			oldParent := n.parent
			n.parent = np
			fs.reapLocked(oldParent)
		}
		n.name = baseName(dst)
	}
	return nil
}

func (fs *FS) removeEmptyDir(p path) error {
	h, _, err := open(p, statAccess|windows.DELETE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, 0, nil)
	if err != nil {
		return errno(err)
	}
	defer windows.CloseHandle(h)
	return errno(remove(h))
}

func (fs *FS) Link(c fuse.Context, id, newParent uint64, newName string) (fuse.Entry, error) {
	n, src, err := fs.getPath(id)
	if err != nil {
		return fuse.Entry{}, err
	}
	np, dst, err := fs.child(newParent, newName, true)
	if err != nil {
		return fuse.Entry{}, err
	}
	if n == fs.root || n.share != np.share {
		return fuse.Entry{}, fuse.EXDEV
	}
	h, _, err := open(src, statAccess|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	defer windows.CloseHandle(h)
	if err := setInfo(h, classLink, nameInfo(0, dst)); err != nil {
		return fuse.Entry{}, errno(err)
	}
	st, err := statHandle(h)
	if err != nil {
		return fuse.Entry{}, errno(err)
	}
	return fs.entry(np, baseName(dst), &st), nil
}
