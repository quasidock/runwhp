//go:build windows

// Package disk opens the host files behind runwhp's virtual disks: image
// layers, each run's scratch disk and named volumes.
//
// A File serves positional reads and writes from many goroutines at once.
// os.File cannot: on Windows its ReadAt and WriteAt hold a lock for the whole
// call, and a handle opened for synchronous I/O makes Windows itself run one
// request at a time. So a File opens its handle for overlapped I/O and waits
// for each request on an event of its own.
package disk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Mode says how a disk file is opened, and who else may open it meanwhile.
type Mode int

const (
	// ReadOnly opens an image layer: reads only, shared with other readers.
	ReadOnly Mode = iota
	// Exclusive opens a volume for reading and writing, and nobody else may
	// open it until it is closed. A second runwhp gets ErrInUse.
	Exclusive
	// Scratch is Exclusive, and the file is deleted when its last handle
	// closes, which Windows does even when the process is killed. Its data
	// is marked temporary, so Windows avoids writing it out.
	Scratch
)

var (
	// ErrInUse means another process has the file open.
	ErrInUse = errors.New("in use by another process")
	// ErrNotSparse means the file's volume has no sparse files, as FAT and
	// exFAT have not.
	ErrNotSparse = errors.New("the volume does not support sparse files")
	// ErrClosed is returned for requests made after Close.
	ErrClosed = errors.New("disk file already closed")
)

// maxIO bounds a single ReadFile or WriteFile, whose lengths are 32 bits.
const maxIO = 1 << 30

// sharingRetries and sharingDelay give a file another process holds only
// briefly, such as an antivirus scanner or the search indexer, time to be
// let go before ErrInUse is reported.
const (
	sharingRetries = 3
	sharingDelay   = 100 * time.Millisecond
)

// File is an open disk file.
type File struct {
	h    windows.Handle
	path string

	// mu is held for reading by every request and for writing by Close, so
	// Close waits for requests in progress and none starts after it.
	mu     sync.RWMutex
	closed bool

	opsMu sync.Mutex
	ops   []*op // idle requests, each with its own event
	all   []*op
}

// op is one request in progress. It lives on the heap, never on a stack:
// Windows writes into the OVERLAPPED until the request completes, and Go
// moves stacks when they grow.
type op struct {
	ov   windows.Overlapped
	zero windows.FileZeroDataInformation
}

// Create makes a new sparse file of size bytes, which must not exist, and
// opens it in mode m, which must not be ReadOnly.
func Create(path string, size int64, m Mode) (*File, error) {
	if m == ReadOnly {
		return nil, errors.New("disk: cannot create a read-only file")
	}
	f, err := open(path, m, windows.CREATE_NEW)
	if err != nil {
		return nil, err
	}
	if err := f.ioctl(windows.FSCTL_SET_SPARSE, nil, 0); err != nil {
		f.discard()
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotSparse)
		}
		return nil, fmt.Errorf("making %s sparse: %w", path, err)
	}
	eof := size
	if err := windows.SetFileInformationByHandle(f.h, windows.FileEndOfFileInfo, (*byte)(unsafe.Pointer(&eof)), 8); err != nil {
		f.discard()
		return nil, fmt.Errorf("sizing %s: %w", path, err)
	}
	return f, nil
}

// Open opens an existing file in mode m.
func Open(path string, m Mode) (*File, error) {
	return open(path, m, windows.OPEN_EXISTING)
}

func open(path string, m Mode, disposition uint32) (*File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	share := uint32(windows.FILE_SHARE_READ)
	flags := uint32(windows.FILE_FLAG_OVERLAPPED)
	switch m {
	case Exclusive:
		access |= windows.GENERIC_WRITE
		share = 0
	case Scratch:
		access |= windows.GENERIC_WRITE | windows.DELETE
		share = 0
		flags |= windows.FILE_FLAG_DELETE_ON_CLOSE | windows.FILE_ATTRIBUTE_TEMPORARY | windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED
	}
	var h windows.Handle
	for try := 0; ; try++ {
		h, err = windows.CreateFile(p, access, share, nil, disposition, flags, 0)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || try == sharingRetries {
			break
		}
		time.Sleep(sharingDelay)
	}
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, fmt.Errorf("%s: %w", path, ErrInUse)
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &File{h: h, path: path}, nil
}

// discard closes a file Create could not finish, and removes it unless it
// removes itself.
func (f *File) discard() {
	windows.CloseHandle(f.h)
	windows.DeleteFile(windows.StringToUTF16Ptr(f.path))
}

// Name returns the path the file was opened with.
func (f *File) Name() string { return f.path }

// Size returns the file's length in bytes.
func (f *File) Size() (int64, error) {
	if err := f.begin(); err != nil {
		return 0, err
	}
	defer f.mu.RUnlock()
	var info fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(f.h, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, err
	}
	return info.EndOfFile, nil
}

// fileStandardInfo is FILE_STANDARD_INFO.
type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

func (f *File) begin() error {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return ErrClosed
	}
	return nil
}

func (f *File) getOp() (*op, error) {
	f.opsMu.Lock()
	defer f.opsMu.Unlock()
	if n := len(f.ops); n > 0 {
		o := f.ops[n-1]
		f.ops = f.ops[:n-1]
		return o, nil
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil) // manual reset, as overlapped I/O wants
	if err != nil {
		return nil, err
	}
	o := &op{}
	o.ov.HEvent = ev
	f.all = append(f.all, o)
	return o, nil
}

func (f *File) putOp(o *op) {
	f.opsMu.Lock()
	f.ops = append(f.ops, o)
	f.opsMu.Unlock()
}

// wait finishes a request started with o: err is what starting it returned.
func (f *File) wait(o *op, err error) (uint32, error) {
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	var n uint32
	err = windows.GetOverlappedResult(f.h, &o.ov, &n, true)
	return n, err
}

func (o *op) at(off int64) {
	o.ov.Internal, o.ov.InternalHigh = 0, 0
	o.ov.Offset, o.ov.OffsetHigh = uint32(off), uint32(off>>32)
}

// ReadAt reads len(p) bytes at off. It is safe to call from several
// goroutines at once. Reading past the end of the file returns io.EOF.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if err := f.begin(); err != nil {
		return 0, err
	}
	defer f.mu.RUnlock()
	o, err := f.getOp()
	if err != nil {
		return 0, err
	}
	defer f.putOp(o)
	n := 0
	for n < len(p) {
		chunk := p[n:min(len(p), n+maxIO)]
		o.at(off + int64(n))
		var done uint32
		done, err = f.wait(o, windows.ReadFile(f.h, chunk, &done, &o.ov))
		runtime.KeepAlive(chunk)
		n += int(done)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || (err == nil && done == 0) {
			return n, io.EOF
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// WriteAt writes p at off. It is safe to call from several goroutines at
// once.
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	if err := f.begin(); err != nil {
		return 0, err
	}
	defer f.mu.RUnlock()
	o, err := f.getOp()
	if err != nil {
		return 0, err
	}
	defer f.putOp(o)
	n := 0
	for n < len(p) {
		chunk := p[n:min(len(p), n+maxIO)]
		o.at(off + int64(n))
		var done uint32
		done, err = f.wait(o, windows.WriteFile(f.h, chunk, &done, &o.ov))
		runtime.KeepAlive(chunk)
		n += int(done)
		if err != nil {
			return n, err
		}
		if done == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

// Sync makes everything written so far durable.
func (f *File) Sync() error {
	if err := f.begin(); err != nil {
		return err
	}
	defer f.mu.RUnlock()
	return windows.FlushFileBuffers(f.h)
}

// Zero makes [off, off+n) read as zeros and, where whole allocation units of
// the file system are covered, gives their space back.
func (f *File) Zero(off, n int64) error {
	if n <= 0 {
		return nil
	}
	if err := f.begin(); err != nil {
		return err
	}
	defer f.mu.RUnlock()
	o, err := f.getOp()
	if err != nil {
		return err
	}
	defer f.putOp(o)
	o.zero = windows.FileZeroDataInformation{FileOffset: off, BeyondFinalZero: off + n}
	return f.ioctlOp(o, windows.FSCTL_SET_ZERO_DATA, (*byte)(unsafe.Pointer(&o.zero)), uint32(unsafe.Sizeof(o.zero)))
}

// ioctl sends a control code that takes only an input buffer. The handle is
// open for overlapped I/O, so it too needs an OVERLAPPED.
func (f *File) ioctl(code uint32, in *byte, inLen uint32) error {
	o, err := f.getOp()
	if err != nil {
		return err
	}
	defer f.putOp(o)
	return f.ioctlOp(o, code, in, inLen)
}

func (f *File) ioctlOp(o *op, code uint32, in *byte, inLen uint32) error {
	o.at(0)
	var done uint32
	_, err := f.wait(o, windows.DeviceIoControl(f.h, code, in, inLen, nil, 0, &done, &o.ov))
	return err
}

// Close cancels requests still waiting on Windows, waits for every request
// in progress to return, and closes the file. A Scratch file is then deleted.
func (f *File) Close() error {
	f.mu.RLock()
	closed := f.closed
	f.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	windows.CancelIoEx(f.h, nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	f.closed = true
	err := windows.CloseHandle(f.h)
	f.opsMu.Lock()
	for _, o := range f.all {
		windows.CloseHandle(o.ov.HEvent)
	}
	f.ops, f.all = nil, nil
	f.opsMu.Unlock()
	return err
}

var procGetCompressedFileSizeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// Allocated returns how many bytes of the host's disk the file at path takes,
// which for a sparse file is far less than its size. It works while another
// process has the file open exclusively.
func Allocated(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xffffffff {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, &fs.PathError{Op: "stat", Path: path, Err: errno}
		}
	}
	return int64(high)<<32 | int64(uint32(low)), nil
}
