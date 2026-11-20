//go:build windows

package hostfs

import (
	"encoding/binary"
	"errors"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/fuse"
)

var le = binary.LittleEndian

// File information classes for NtQueryInformationFile and
// NtSetInformationFile. x/sys/windows has some of them, and names class 72,
// FileLinkInformationEx, FileLinkInformation.
const (
	classBasic        = 4
	classRename       = 10
	classLink         = 11
	classDisposition  = 13
	classEndOfFile    = 20
	classID           = 59
	classDispositionX = 64
	classRenameX      = 65
	classStat         = 68
	classStatLx       = 70
)

// Reparse tags. Name surrogates are links; the WSL tags mark files whose
// type Windows has no notion of.
const (
	tagMountPoint = 0xa0000003 // junction
	tagSymlink    = 0xa000000c
	tagLxSymlink  = 0xa000001d
	tagAfUnix     = 0x80000023
	tagLxFIFO     = 0x80000024
	tagLxChr      = 0x80000025
	tagLxBlk      = 0x80000026

	tagNameSurrogate = 0x20000000
)

// WSL's metadata flags in FILE_STAT_LX_INFORMATION.
const (
	lxHasUID    = 0x1
	lxHasGID    = 0x2
	lxHasMode   = 0x4
	lxHasDevice = 0x8
)

const (
	statusSuccess = windows.STATUS_SUCCESS
	// The NTSTATUS values runwhp treats specially.
	statusNoMoreFiles     = windows.NTStatus(0x80000006)
	statusEndOfFile       = windows.NTStatus(0xc0000011)
	statusNotImplemented  = windows.NTStatus(0xc0000002)
	statusInvalidInfo     = windows.NTStatus(0xc0000003)
	statusInvalidParam    = windows.NTStatus(0xc000000d)
	statusNotSupported    = windows.NTStatus(0xc00000bb)
	statusEasNotSupported = windows.NTStatus(0xc000004f)
)

// buffer returns n bytes aligned for any of the structures Windows fills in
// or reads; a Go byte array has no alignment, and NT refuses misaligned
// buffers.
func buffer(n int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(&make([]uint64, (n+7)/8)[0])), n)
}

// path is an NT path, such as \??\C:\Users\me\src, which NtCreateFile takes
// without the length limits and name rewriting of Win32 paths.
type path string

func (p path) child(name string) path { return p + `\` + path(name) }

// ntPath turns a Win32 path, as GetFinalPathNameByHandle returns it, into an
// NT path.
func ntPath(win string) path {
	switch {
	case len(win) >= 8 && win[:8] == `\\?\UNC\`:
		return path(`\??\UNC\` + win[8:])
	case len(win) >= 4 && win[:4] == `\\?\`:
		return path(`\??\` + win[4:])
	case len(win) >= 2 && win[:2] == `\\`:
		return path(`\??\UNC\` + win[2:])
	}
	return path(`\??\` + win)
}

// open opens or creates p. It never follows a reparse point at p itself.
// The handle is synchronous, so reads and writes at an offset complete before
// they return. created reports whether the file is new.
func open(p path, access, disposition, options, attrs uint32, ea []byte) (h windows.Handle, created bool, err error) {
	return createFile(p, access, disposition, options|windows.FILE_OPEN_REPARSE_POINT, attrs, ea)
}

func createFile(p path, access, disposition, options, attrs uint32, ea []byte) (windows.Handle, bool, error) {
	name, err := windows.NewNTUnicodeString(string(p))
	if err != nil {
		return 0, false, fuse.EINVAL
	}
	oa := &windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var h windows.Handle
	var eaPtr uintptr
	if len(ea) > 0 {
		eaPtr = uintptr(unsafe.Pointer(&ea[0]))
	}
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, oa, &iosb, nil, attrs,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT, eaPtr, uint32(len(ea)))
	runtime.KeepAlive(ea)
	if err != nil {
		return 0, false, err
	}
	const fileCreated = 2
	return h, iosb.Information == fileCreated, nil
}

// statAccess is the access open needs for statHandle.
const statAccess = windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA

// fileStat is what runwhp needs to know about a host file.
type fileStat struct {
	key                                 fileID
	created, accessed, written, changed int64 // FILETIME units
	size, alloc                         int64
	attrs, tag, nlink                   uint32
	lxFlags, uid, gid, mode             uint32
	devMajor, devMinor                  uint32
}

// fileID identifies a file on its volume. It is the 64-bit id that stats and
// directory listings both report, or, where that does not fit, as on a large
// ReFS volume, the 128-bit one, whose high half names the file's directory.
type fileID [16]byte

// ino folds a file id into an inode number. Both halves are small in
// practice, so rotating one of them apart keeps the result unique.
func (id fileID) ino() uint64 {
	lo, hi := le.Uint64(id[0:]), le.Uint64(id[8:])
	return lo ^ (hi<<32 | hi>>32)
}

// validID reports whether a 64-bit file id names the file. ReFS reports 0 or
// -1 for a file whose 128-bit id does not fit.
func validID(id uint64) bool { return id != 0 && id != ^uint64(0) }

func (s *fileStat) isDir() bool { return s.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 }

// isLink reports whether the guest sees the file as a symlink: a WSL
// symlink, or a Windows symlink or junction. Other reparse points, such as
// cloud files, are ordinary files to the guest.
func (s *fileStat) isLink() bool {
	return s.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && (s.tag == tagLxSymlink || s.tag&tagNameSurrogate != 0)
}

func statHandle(h windows.Handle) (fileStat, error) {
	var st fileStat
	var iosb windows.IO_STATUS_BLOCK
	b := buffer(96) // FILE_STAT_LX_INFORMATION
	err := windows.NtQueryInformationFile(h, &iosb, &b[0], uint32(len(b)), classStatLx)
	if err != nil {
		// File systems without WSL's metadata, which have no extended
		// attributes either, may still know FILE_STAT_INFORMATION.
		clear(b)
		err = windows.NtQueryInformationFile(h, &iosb, &b[0], 72, classStat)
	}
	if err == nil {
		st = parseStat(b)
	} else {
		// FAT and other simple file systems.
		var fi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
			return st, err
		}
		ft := func(t windows.Filetime) int64 { return int64(t.HighDateTime)<<32 | int64(t.LowDateTime) }
		st.created, st.accessed, st.written = ft(fi.CreationTime), ft(fi.LastAccessTime), ft(fi.LastWriteTime)
		st.changed = st.written
		st.size = int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
		st.alloc = st.size
		st.attrs = fi.FileAttributes
		st.nlink = fi.NumberOfLinks
		le.PutUint32(st.key[0:], fi.FileIndexLow)
		le.PutUint32(st.key[4:], fi.FileIndexHigh)
		if st.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			tag := buffer(8) // FILE_ATTRIBUTE_TAG_INFO
			if windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, &tag[0], 8) == nil {
				st.tag = le.Uint32(tag[4:])
			}
		}
		return st, nil
	}
	if !validID(le.Uint64(st.key[:])) {
		id := buffer(24) // FILE_ID_INFORMATION
		if windows.NtQueryInformationFile(h, &iosb, &id[0], uint32(len(id)), classID) == nil {
			copy(st.key[:], id[8:])
		}
	}
	return st, nil
}

// parseStat decodes FILE_STAT_LX_INFORMATION, or the FILE_STAT_INFORMATION
// it begins with.
func parseStat(b []byte) fileStat {
	var st fileStat
	le.PutUint64(st.key[:], le.Uint64(b[0:]))
	st.created = int64(le.Uint64(b[8:]))
	st.accessed = int64(le.Uint64(b[16:]))
	st.written = int64(le.Uint64(b[24:]))
	st.changed = int64(le.Uint64(b[32:]))
	st.alloc = int64(le.Uint64(b[40:]))
	st.size = int64(le.Uint64(b[48:]))
	st.attrs = le.Uint32(b[56:])
	st.tag = le.Uint32(b[60:])
	st.nlink = le.Uint32(b[64:])
	st.lxFlags = le.Uint32(b[72:])
	st.uid = le.Uint32(b[76:])
	st.gid = le.Uint32(b[80:])
	st.mode = le.Uint32(b[84:])
	st.devMajor = le.Uint32(b[88:])
	st.devMinor = le.Uint32(b[92:])
	return st
}

// procQueryByName stats a file without opening it, which on NTFS takes a
// fifth of the time an open, a query and a close take.
var procQueryByName = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryInformationByName")

// statPath stats p without following a reparse point at p.
func statPath(p path) (fileStat, error) {
	if procQueryByName.Find() == nil {
		b := buffer(96)
		err := queryByName(p, classStatLx, b)
		if err == nil && validID(le.Uint64(b)) {
			return parseStat(b), nil
		}
		if err != nil && !unsupported(err) {
			return fileStat{}, err
		}
		// A file system that cannot answer by name, or a file whose id
		// does not fit in 64 bits: open it.
	}
	h, _, err := open(p, statAccess, windows.FILE_OPEN, 0, 0, nil)
	if err != nil {
		return fileStat{}, err
	}
	defer windows.CloseHandle(h)
	return statHandle(h)
}

// queryByName calls NtQueryInformationByName, which does not follow a
// reparse point at p.
func queryByName(p path, class uint32, b []byte) error {
	name, err := windows.NewNTUnicodeString(string(p))
	if err != nil {
		return fuse.EINVAL
	}
	oa := &windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	r, _, _ := procQueryByName.Call(uintptr(unsafe.Pointer(oa)), uintptr(unsafe.Pointer(&iosb)),
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(class))
	runtime.KeepAlive(oa)
	if r != 0 {
		return windows.NTStatus(r)
	}
	return nil
}

// unsupported reports whether an error says the file system lacks a newer
// information class or flag.
func unsupported(err error) bool {
	var st windows.NTStatus
	if !errors.As(err, &st) {
		return false
	}
	switch st {
	case statusInvalidParam, statusNotSupported, statusNotImplemented, statusInvalidInfo:
		return true
	}
	return false
}

// setInfo calls NtSetInformationFile.
func setInfo(h windows.Handle, class uint32, buf []byte) error {
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(h, &iosb, &buf[0], uint32(len(buf)), class)
}

// nameInfo builds FILE_RENAME_INFORMATION(_EX) or FILE_LINK_INFORMATION
// naming target by its full NT path. The three share one layout: a flags
// word or BOOLEAN, a root directory handle, the name's length and the name.
func nameInfo(flags uint32, target path) []byte {
	name := utf16.Encode([]rune(string(target)))
	b := buffer(20 + 2*len(name))
	le.PutUint32(b[0:], flags)
	le.PutUint32(b[16:], uint32(2*len(name)))
	for i, c := range name {
		le.PutUint16(b[20+2*i:], c)
	}
	return b
}

// filetime converts a time to FILETIME units; 0 leaves a time unchanged when
// setting FILE_BASIC_INFORMATION.
func filetime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()/100 + 116444736000000000
}

func fromFiletime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	return time.Unix(0, (ft-116444736000000000)*100)
}

// setBasic sets times and attributes. Zero leaves a field unchanged.
func setBasic(h windows.Handle, accessed, written, attrs int64) error {
	b := buffer(40) // FILE_BASIC_INFORMATION
	le.PutUint64(b[8:], uint64(accessed))
	le.PutUint64(b[16:], uint64(written))
	le.PutUint32(b[32:], uint32(attrs))
	return setInfo(h, classBasic, b)
}

// ea is one of WSL's metadata attributes.
type ea struct {
	name  string
	value []byte
}

func eaU32(name string, v uint32) ea { return ea{name, le.AppendUint32(nil, v)} }

// eaBuffer encodes a FILE_FULL_EA_INFORMATION list.
func eaBuffer(eas ...ea) []byte {
	var b []byte
	for i, e := range eas {
		start := len(b)
		b = le.AppendUint32(b, 0) // NextEntryOffset
		b = append(b, 0, byte(len(e.name)))
		b = le.AppendUint16(b, uint16(len(e.value)))
		b = append(b, e.name...)
		b = append(b, 0)
		b = append(b, e.value...)
		if i < len(eas)-1 {
			for len(b)%4 != 0 {
				b = append(b, 0)
			}
			le.PutUint32(b[start:], uint32(len(b)-start))
		}
	}
	out := buffer(len(b))
	copy(out, b)
	return out
}

// lxMetadata is WSL's metadata for a new file.
func lxMetadata(uid, gid, mode uint32, rdev ...uint32) []byte {
	eas := []ea{eaU32("$LXUID", uid), eaU32("$LXGID", gid), eaU32("$LXMOD", mode)}
	if len(rdev) == 2 {
		eas = append(eas, ea{"$LXDEV", le.AppendUint32(le.AppendUint32(nil, rdev[0]), rdev[1])})
	}
	return eaBuffer(eas...)
}

func setEA(h windows.Handle, buf []byte) error {
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetEaFile(h, &iosb, &buf[0], uint32(len(buf)))
}

// reparse reads a reparse point's tag and data.
func reparse(h windows.Handle) (uint32, []byte, error) {
	b := buffer(windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &b[0], uint32(len(b)), &n, nil); err != nil {
		return 0, nil, err
	}
	if n < 8 {
		return 0, nil, fuse.EIO
	}
	size := min(int(le.Uint16(b[4:])), int(n)-8)
	return le.Uint32(b), b[8 : 8+size], nil
}

// setReparse makes the file h a reparse point with a Microsoft tag.
func setReparse(h windows.Handle, tag uint32, data []byte) error {
	b := buffer(8 + len(data))
	le.PutUint32(b[0:], tag)
	le.PutUint16(b[4:], uint16(len(data)))
	copy(b[8:], data)
	var n uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &b[0], uint32(len(b)), nil, 0, &n, nil)
}

// errno maps a Windows error to the Linux error the guest should see.
func errno(err error) error {
	if err == nil {
		return nil
	}
	var e fuse.Errno
	if errors.As(err, &e) {
		return e
	}
	var st windows.NTStatus
	if errors.As(err, &st) {
		switch st {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND, windows.STATUS_DELETE_PENDING,
			windows.STATUS_NO_SUCH_FILE:
			return fuse.ENOENT
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return fuse.EEXIST
		case windows.STATUS_ACCESS_DENIED, windows.STATUS_CANNOT_DELETE:
			return fuse.EACCES
		case windows.STATUS_SHARING_VIOLATION, windows.STATUS_FILE_LOCK_CONFLICT, windows.STATUS_LOCK_NOT_GRANTED:
			return fuse.EBUSY
		case windows.STATUS_DIRECTORY_NOT_EMPTY:
			return fuse.ENOTEMPTY
		case windows.STATUS_NOT_A_DIRECTORY, windows.STATUS_OBJECT_PATH_SYNTAX_BAD:
			return fuse.ENOTDIR
		case windows.STATUS_FILE_IS_A_DIRECTORY:
			return fuse.EISDIR
		case windows.STATUS_DISK_FULL:
			return fuse.ENOSPC
		case windows.STATUS_NOT_SAME_DEVICE:
			return fuse.EXDEV
		case windows.STATUS_OBJECT_NAME_INVALID, statusInvalidParam:
			return fuse.EINVAL
		case windows.STATUS_NAME_TOO_LONG:
			return fuse.ENAMETOOLONG
		case windows.STATUS_PRIVILEGE_NOT_HELD:
			return fuse.EPERM
		case statusNotSupported, statusEasNotSupported, statusNotImplemented, statusInvalidInfo:
			return fuse.EOPNOTSUPP
		case windows.STATUS_TOO_MANY_LINKS:
			return fuse.EMLINK
		case windows.STATUS_MEDIA_WRITE_PROTECTED:
			return fuse.EROFS
		case windows.STATUS_STOPPED_ON_SYMLINK, windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED, windows.STATUS_REPARSE_POINT_NOT_RESOLVED:
			return fuse.ELOOP
		case windows.STATUS_INVALID_HANDLE:
			return fuse.EBADF
		}
		return err
	}
	var w syscall.Errno
	if errors.As(err, &w) {
		switch w {
		case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_DELETE_PENDING:
			return fuse.ENOENT
		case windows.ERROR_FILE_EXISTS, windows.ERROR_ALREADY_EXISTS:
			return fuse.EEXIST
		case windows.ERROR_ACCESS_DENIED, windows.ERROR_CANT_ACCESS_FILE:
			return fuse.EACCES
		case windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
			return fuse.EBUSY
		case windows.ERROR_DIR_NOT_EMPTY:
			return fuse.ENOTEMPTY
		case windows.ERROR_DIRECTORY:
			return fuse.ENOTDIR
		case windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL:
			return fuse.ENOSPC
		case windows.ERROR_NOT_SAME_DEVICE:
			return fuse.EXDEV
		case windows.ERROR_INVALID_NAME, windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_A_REPARSE_POINT:
			return fuse.EINVAL
		case windows.ERROR_FILENAME_EXCED_RANGE:
			return fuse.ENAMETOOLONG
		case windows.ERROR_PRIVILEGE_NOT_HELD:
			return fuse.EPERM
		case windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION, windows.ERROR_EAS_NOT_SUPPORTED:
			return fuse.EOPNOTSUPP
		case windows.ERROR_TOO_MANY_LINKS:
			return fuse.EMLINK
		case windows.ERROR_WRITE_PROTECT:
			return fuse.EROFS
		case windows.ERROR_INVALID_HANDLE:
			return fuse.EBADF
		case windows.ERROR_CANT_RESOLVE_FILENAME:
			return fuse.ELOOP
		}
	}
	return err
}
