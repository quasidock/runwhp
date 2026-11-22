package container

import (
	"encoding/binary"
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
		buf := make([]byte, frameHeader+len(chunk))
		buf[0] = s.stream
		binary.BigEndian.PutUint32(buf[4:], uint32(len(chunk)))
		copy(buf[frameHeader:], chunk)
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
			size := int(binary.BigEndian.Uint32(lr.partial[4:frameHeader]))
			if size > maxFrame || lr.partial[1]|lr.partial[2]|lr.partial[3] != 0 {
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
