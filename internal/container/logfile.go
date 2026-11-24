package container

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A container's log is a file of frames, rotated into numbered files as it
// grows:
//
//	log      the file being written
//	log.1    the one before it, and so on up to log.(MaxFiles-1)
//
// Each file starts with a header frame in the attach framing, eight bytes
// of header followed by the payload:
//
//	LogHeader 0 0 0 | length (uint32, big-endian) | "runwhp-log" | version (uint16) | generation (uint64) | partial (byte)
//
// The generation counts the files a container has written, one more for
// each, so that a reader following the log finds the file that comes after
// the one it has read, whatever it has been renamed to since. Partial has
// bit 1<<stream set for each stream the file starts in the middle of a
// line of, whose start is in the file before: a reader that starts at this
// file skips the rest of that line, rather than print it as one. Every frame
// after the header is a version 2 frame, with the time it was written:
//
//	kind 0 0 0 | length (uint32, big-endian) | time (int64 Unix nanoseconds, big-endian) | data
//
// where kind is Stdout or Stderr, or LogNext, with no data, which ends a
// file that was rotated away. Logs written before iteration 13 are version
// 1: frames of the attach framing alone, with no header and no times.
const (
	// LogHeader starts a log file.
	LogHeader = 0x10
	// LogNext ends a log file that was rotated away.
	LogNext = 0x11
)

const (
	logMagic      = "runwhp-log"
	logVersion    = 2
	logHeaderData = len(logMagic) + 2 + 8 + 1
	logHeaderSize = frameHeader + logHeaderData
	frameHeaderV2 = 16
	logPrefix     = "log"
)

// Docker's local logging driver's defaults: 20 MB a file, five files.
const (
	DefaultLogMaxSize  = 20 << 20
	DefaultLogMaxFiles = 5
)

// LogOptions bound a container's log: it is rotated once a file would grow
// past MaxSize, and at most MaxFiles files are kept, the one being written
// included. MaxSize <= 0 never rotates.
type LogOptions struct {
	MaxSize  int64
	MaxFiles int
}

// LogWriter writes a container's log, from any number of goroutines,
// rotating it as LogOptions say. The container's output never waits on
// its log nor fails because of it: a frame that cannot be written is
// dropped, and diag told once.
type LogWriter struct {
	dir  string
	opts LogOptions
	diag func(format string, a ...any)
	now  func() time.Time

	mu     sync.Mutex
	f      *os.File
	size   int64
	gen    uint64
	failed bool
	closed bool
	// partial has bit 1<<stream set for each stream whose last write did
	// not end its line.
	partial byte
}

// OpenLogWriter opens the log in dir for writing, after the frames it
// holds. A log of version 1, or one already full, is rotated away first.
func OpenLogWriter(dir string, opts LogOptions, diag func(string, ...any)) (*LogWriter, error) {
	return openLogWriter(dir, opts, diag, time.Now)
}

func openLogWriter(dir string, opts LogOptions, diag func(string, ...any), now func() time.Time) (*LogWriter, error) {
	if opts.MaxFiles < 1 {
		opts.MaxFiles = 1
	}
	if diag == nil {
		diag = func(string, ...any) {}
	}
	w := &LogWriter{dir: dir, opts: opts, diag: diag, now: now}
	path := w.path(0)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	switch {
	case errors.Is(err, os.ErrNotExist):
		gen, err := w.lastGeneration()
		if err != nil {
			return nil, err
		}
		return w, w.create(gen + 1)
	case err != nil:
		return nil, err
	}
	gen, end, err := scanLog(f)
	if err != nil || gen == 0 || w.full(end, 0) {
		// Version 1, damaged at its start, or full: start a new file after
		// it; or empty, so start it again as version 2.
		f.Close()
		if end > 0 {
			if err := w.rotate(); err != nil {
				return nil, err
			}
		}
		last, err := w.lastGeneration()
		if err != nil {
			return nil, err
		}
		return w, w.create(max(gen, last) + 1)
	}
	// Drop a frame cut short when its writer died, which would garble what
	// comes after it.
	if err := f.Truncate(end); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	w.f, w.size, w.gen = f, end, gen
	return w, nil
}

func (w *LogWriter) path(n int) string {
	if n == 0 {
		return filepath.Join(w.dir, logPrefix)
	}
	return filepath.Join(w.dir, fmt.Sprintf("%s.%d", logPrefix, n))
}

// lastGeneration is the highest generation among the rotated files, 0 if
// there are none.
func (w *LogWriter) lastGeneration() (uint64, error) {
	files, err := LogFiles(w.dir)
	if err != nil {
		return 0, err
	}
	var g uint64
	for _, f := range files {
		g = max(g, f.Gen)
	}
	return g, nil
}

// full reports whether a file of size cannot take n more bytes, and the
// LogNext that would end it then.
func (w *LogWriter) full(size int64, n int) bool {
	return w.opts.MaxSize > 0 && size > int64(logHeaderSize) && size+int64(n)+frameHeaderV2 > w.opts.MaxSize
}

// create starts a new log file of generation gen.
func (w *LogWriter) create(gen uint64) error {
	f, err := os.OpenFile(w.path(0), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	h := appendLogHeader(nil, logHeader{Gen: gen, Partial: w.partial})
	if _, err := f.Write(h); err != nil {
		f.Close()
		return err
	}
	w.f, w.size, w.gen = f, int64(len(h)), gen
	return nil
}

// logHeader is what a log file's header says.
type logHeader struct {
	Version int    // 1 for a file without one
	Gen     uint64 // 0 for version 1
	Partial byte
}

func appendLogHeader(buf []byte, h logHeader) []byte {
	var fh [frameHeader]byte
	fh[0] = LogHeader
	binary.BigEndian.PutUint32(fh[4:], uint32(logHeaderData))
	buf = append(append(buf, fh[:]...), logMagic...)
	buf = binary.BigEndian.AppendUint16(buf, logVersion)
	buf = binary.BigEndian.AppendUint64(buf, h.Gen)
	return append(buf, h.Partial)
}

// appendFrameV2 appends one version 2 frame.
func appendFrameV2(buf []byte, kind byte, t time.Time, p []byte) []byte {
	var h [frameHeaderV2]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[4:], uint32(len(p)))
	binary.BigEndian.PutUint64(h[8:], uint64(t.UnixNano()))
	return append(append(buf, h[:]...), p...)
}

// rotate shifts the closed files up by one, dropping the oldest, and moves
// the current file to log.1; with MaxFiles 1, it removes it.
func (w *LogWriter) rotate() error {
	// Drop the oldest, and any past the limit, as a lower limit leaves.
	// With MaxFiles 1, the oldest is the current file.
	for n := w.opts.MaxFiles - 1; ; n++ {
		err := os.Remove(w.path(n))
		if errors.Is(err, os.ErrNotExist) {
			if n >= w.opts.MaxFiles {
				break
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	if w.opts.MaxFiles == 1 {
		return nil
	}
	for n := w.opts.MaxFiles - 2; n >= 1; n-- {
		if err := os.Rename(w.path(n), w.path(n+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(w.path(0), w.path(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Whatever keeps it from being renamed, the log must not grow
		// without bound: drop it.
		if rerr := os.Remove(w.path(0)); rerr != nil {
			return err
		}
	}
	return nil
}

// Stream returns a writer whose writes become frames of kind.
func (w *LogWriter) Stream(kind byte) io.Writer { return &logWriterStream{w, kind} }

type logWriterStream struct {
	w    *LogWriter
	kind byte
}

// Write writes p to the log, and never fails.
func (s *logWriterStream) Write(p []byte) (int, error) {
	s.w.write(s.kind, p)
	return len(p), nil
}

func (w *LogWriter) write(kind byte, p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrame)]
		p = p[len(chunk):]
		frame := appendFrameV2(nil, kind, w.now(), chunk)
		if w.f != nil && w.full(w.size, len(frame)) {
			w.next()
		}
		if w.f == nil {
			w.fail(errors.New("no log file open"))
			continue
		}
		n, err := w.f.Write(frame)
		if err != nil {
			// A frame cut short would garble the rest: take it back.
			if n > 0 {
				w.f.Truncate(w.size)
				w.f.Seek(w.size, io.SeekStart)
			}
			w.fail(err)
			continue
		}
		w.size += int64(n)
		if bit := byte(1) << kind; chunk[len(chunk)-1] == '\n' {
			w.partial &^= bit
		} else {
			w.partial |= bit
		}
	}
}

// next ends the current file and starts the next.
func (w *LogWriter) next() {
	w.f.Write(appendFrameV2(nil, LogNext, w.now(), nil))
	w.f.Close()
	w.f = nil
	if err := w.rotate(); err != nil {
		w.fail(fmt.Errorf("rotating the log: %w", err))
	}
	if err := w.create(w.gen + 1); err != nil {
		w.fail(fmt.Errorf("starting a new log file: %w", err))
	}
}

func (w *LogWriter) fail(err error) {
	if !w.failed {
		w.failed = true
		w.diag("container log: %v; output is dropped from the log until it can be written again", err)
	}
}

// Close closes the log file.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// scanLog reads a log file's header and walks its frames, returning its
// generation, 0 for version 1, and where its last whole frame ends.
func scanLog(f *os.File) (gen uint64, end int64, err error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := fi.Size()
	h0, err := readLogHeader(f)
	if err != nil {
		return 0, size, err
	}
	if h0.Version == 1 {
		return 0, size, nil
	}
	gen = h0.Gen
	pos := int64(logHeaderSize)
	var h [frameHeaderV2]byte
	for pos+frameHeaderV2 <= size {
		if _, err := f.ReadAt(h[:], pos); err != nil {
			break
		}
		n, err := frameSizeV2(h[:])
		if err != nil || pos+frameHeaderV2+int64(n) > size {
			break
		}
		pos += frameHeaderV2 + int64(n)
	}
	return gen, pos, nil
}

// readLogHeader reads the header at the start of a log file: version 1
// files have none, and an empty file is reported as version 1.
func readLogHeader(r io.ReaderAt) (logHeader, error) {
	var h [logHeaderSize]byte
	n, err := r.ReadAt(h[:], 0)
	if n < logHeaderSize {
		if err == io.EOF || err == nil {
			return logHeader{Version: 1}, nil
		}
		return logHeader{}, err
	}
	return parseLogHeader(h[:])
}

func parseLogHeader(h []byte) (logHeader, error) {
	if h[0] != LogHeader || string(h[frameHeader:frameHeader+len(logMagic)]) != logMagic {
		return logHeader{Version: 1}, nil
	}
	if binary.BigEndian.Uint32(h[4:]) != uint32(logHeaderData) {
		return logHeader{}, errDamaged
	}
	p := h[frameHeader+len(logMagic):]
	if v := binary.BigEndian.Uint16(p); v != logVersion {
		return logHeader{}, fmt.Errorf("log version %d, newer than this runwhp reads", v)
	}
	return logHeader{Version: logVersion, Gen: binary.BigEndian.Uint64(p[2:]), Partial: p[10]}, nil
}

// frameSizeV2 checks a version 2 frame header and returns its length.
func frameSizeV2(h []byte) (int, error) {
	size := int(binary.BigEndian.Uint32(h[4:8]))
	if size > maxFrame || h[1]|h[2]|h[3] != 0 || (h[0] != Stdout && h[0] != Stderr && h[0] != LogNext) {
		return 0, errDamaged
	}
	return size, nil
}
