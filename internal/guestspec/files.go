package guestspec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ArchivePort is the vsock port on which init, booted with Spec.Serve set
// to ServeLayers, serves the files of the layers it was given, for runwhp
// save, which rebuilds each layer's tar from the tar's metadata and these
// files. One connection serves one layer:
//
//	host:  u32 layer index, u32 count, then count times: u32 length, name
//	guest: for each name, in order: i64 size, then size bytes;
//	       or -1, u32 length, an error message
//
// Numbers are big-endian. Names are as the layer's tar gave them, which
// need not be UTF-8, and are taken beneath the layer's root, as tar2ext4
// stored them: cleaned, without a leading slash. Init reads ahead of the
// host, as far as the connection's buffers let it.
const ArchivePort = 1025

// Limits on a files request, which keep a damaged one from asking init for
// much memory.
const (
	MaxFiles    = 1 << 24
	MaxFileName = 64 << 10
)

// WriteFilesRequest asks for the files names of layer.
func WriteFilesRequest(w io.Writer, layer int, names []string) error {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head, uint32(layer))
	binary.BigEndian.PutUint32(head[4:], uint32(len(names)))
	if _, err := w.Write(head); err != nil {
		return err
	}
	var buf []byte
	for _, n := range names {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(n)))
		buf = append(buf, n...)
		if len(buf) >= 64<<10 {
			if _, err := w.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	_, err := w.Write(buf)
	return err
}

// ReadFilesRequest reads what WriteFilesRequest wrote.
func ReadFilesRequest(r io.Reader) (layer int, names []string, err error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, nil, err
	}
	layer = int(binary.BigEndian.Uint32(head))
	count := binary.BigEndian.Uint32(head[4:])
	if count > MaxFiles {
		return 0, nil, fmt.Errorf("files request for %d files", count)
	}
	names = make([]string, 0, count)
	for range count {
		if _, err := io.ReadFull(r, head[:4]); err != nil {
			return 0, nil, err
		}
		n := binary.BigEndian.Uint32(head)
		if n > MaxFileName {
			return 0, nil, fmt.Errorf("files request with a name of %d bytes", n)
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(r, name); err != nil {
			return 0, nil, err
		}
		names = append(names, string(name))
	}
	return layer, names, nil
}

// WriteFileHeader starts a file of size bytes, which follow it.
func WriteFileHeader(w io.Writer, size int64) error {
	return binary.Write(w, binary.BigEndian, size)
}

// WriteFileError says a file could not be read.
func WriteFileError(w io.Writer, msg string) error {
	if len(msg) > MaxFileName {
		msg = msg[:MaxFileName]
	}
	buf := binary.BigEndian.AppendUint64(nil, ^uint64(0))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(msg)))
	_, err := w.Write(append(buf, msg...))
	return err
}

// FileError is a file init could not read, as it said.
type FileError struct{ Msg string }

func (e *FileError) Error() string { return e.Msg }

// ReadFileHeader reads what WriteFileHeader or WriteFileError wrote: the
// file's size, or a *FileError.
func ReadFileHeader(r io.Reader) (int64, error) {
	var size int64
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return 0, err
	}
	if size >= 0 {
		return size, nil
	}
	if size != -1 {
		return 0, fmt.Errorf("file header of size %d", size)
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return 0, err
	}
	if n > MaxFileName {
		return 0, errors.New("file error message too long")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return 0, err
	}
	return 0, &FileError{string(msg)}
}
