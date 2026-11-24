package container

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frames in the framing Docker's attach streams use carry runwhp's attach
// protocol, and were the whole of a container's log before iteration 13
// (see logfile.go): each is an 8-byte header, the kind (1 for stdout, 2
// for stderr) and three zero bytes then the length, big-endian, followed
// by the bytes written.

// Log streams.
const (
	Stdout = 1
	Stderr = 2
)

const frameHeader = 8

// maxFrame bounds one frame, so a reader can tell a log from garbage.
const maxFrame = 1 << 20

// AppendFrame appends p to buf as frames of kind, in the attach framing: as
// many as it takes, and one empty frame if p is empty.
func AppendFrame(buf []byte, kind byte, p []byte) []byte {
	for first := true; first || len(p) > 0; first = false {
		chunk := p[:min(len(p), maxFrame)]
		var h [frameHeader]byte
		h[0] = kind
		binary.BigEndian.PutUint32(h[4:], uint32(len(chunk)))
		buf = append(append(buf, h[:]...), chunk...)
		p = p[len(chunk):]
	}
	return buf
}

// WriteFrame writes p to w as frames of kind, with one write.
func WriteFrame(w io.Writer, kind byte, p []byte) error {
	_, err := w.Write(AppendFrame(nil, kind, p))
	return err
}

// frameSize checks a frame header and returns the length it gives.
func frameSize(h []byte) (int, error) {
	size := int(binary.BigEndian.Uint32(h[4:frameHeader]))
	if size > maxFrame || h[1]|h[2]|h[3] != 0 {
		return 0, errDamaged
	}
	return size, nil
}

var errDamaged = errors.New("damaged frame")

// ReadFrame reads one frame from r.
func ReadFrame(r io.Reader) (kind byte, p []byte, err error) {
	var h [frameHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	size, err := frameSize(h[:])
	if err != nil {
		return 0, nil, err
	}
	p = make([]byte, size)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return h[0], p, nil
}
