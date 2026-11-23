package container

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// A container's log holds its output when no terminal or file takes it,
// in the framing Docker's attach streams use: each write is an 8-byte
// header, the stream (1 for stdout, 2 for stderr) and three zero bytes
// then the length, big-endian, followed by the bytes written.

// Log streams.
const (
	Stdout = 1
	Stderr = 2
)

const frameHeader = 8

// maxFrame bounds one frame, so a reader can tell a log from garbage.
const maxFrame = 1 << 20

// Log appends frames to a log file, from any number of goroutines.
type Log struct {
	mu sync.Mutex
	w  io.Writer
}

func NewLog(w io.Writer) *Log { return &Log{w: w} }

// Stream returns a writer whose writes become frames of stream.
func (l *Log) Stream(stream byte) io.Writer { return &logStream{l, stream} }

type logStream struct {
	l      *Log
	stream byte
}

func (s *logStream) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrame)]
		buf := AppendFrame(nil, s.stream, chunk)
		s.l.mu.Lock()
		_, err := s.l.w.Write(buf)
		s.l.mu.Unlock()
		if err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// AppendFrame appends p to buf as frames of kind, in the log's framing: as
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

// LogReader reads a log's frames, writing each to stdout or stderr. A frame
// cut short at the end of the log, as when it is being written, is kept for
// the next Copy.
type LogReader struct {
	r       io.Reader
	partial []byte
}

func NewLogReader(r io.Reader) *LogReader { return &LogReader{r: r} }

// Copy writes what the log holds now and reports how many bytes of output
// it wrote.
func (lr *LogReader) Copy(stdout, stderr io.Writer) (int64, error) {
	var total int64
	buf := make([]byte, 64<<10)
	for {
		n, err := lr.r.Read(buf)
		lr.partial = append(lr.partial, buf[:n]...)
		for len(lr.partial) >= frameHeader {
			size, ferr := frameSize(lr.partial)
			if ferr != nil {
				return total, fmt.Errorf("the log is damaged")
			}
			if len(lr.partial) < frameHeader+size {
				break
			}
			w := stdout
			if lr.partial[0] == Stderr {
				w = stderr
			}
			if _, werr := w.Write(lr.partial[frameHeader : frameHeader+size]); werr != nil {
				return total, werr
			}
			total += int64(size)
			lr.partial = lr.partial[frameHeader+size:]
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
