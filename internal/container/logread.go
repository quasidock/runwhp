package container

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogFile is one file of a container's log.
type LogFile struct {
	Path    string
	N       int    // 0 for log, n for log.n
	Version int    // 1 or 2
	Gen     uint64 // 0 for version 1
	Partial byte   // the streams it starts in the middle of a line of
}

// LogFiles lists the files of the log in dir, oldest first.
func LogFiles(dir string) ([]LogFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []LogFile
	for _, e := range ents {
		n, ok := logFileNumber(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		lf := LogFile{Path: filepath.Join(dir, e.Name()), N: n}
		f, err := OpenShared(lf.Path)
		if err != nil {
			continue // rotated away meanwhile
		}
		h, err := readLogHeader(f)
		f.Close()
		if err != nil {
			h = logHeader{Version: 1}
		}
		lf.Version, lf.Gen, lf.Partial = h.Version, h.Gen, h.Partial
		out = append(out, lf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	return out, nil
}

// logFileNumber parses log and log.N.
func logFileNumber(name string) (int, bool) {
	if name == logPrefix {
		return 0, true
	}
	rest, ok := strings.CutPrefix(name, logPrefix+".")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// LogFrame is a frame of output from a container's log. Time is zero in a
// version 1 log.
type LogFrame struct {
	Kind byte
	Time time.Time
	Data []byte
}

// LogReader reads the frames of a container's log, oldest first, from one
// file to the next: by generation, so that files renamed as the log is
// rotated meanwhile are not missed, nor read twice. Next returns io.EOF when
// it has read every whole frame written so far; it can be called again
// for what is written later.
type LogReader struct {
	dir     string
	f       *os.File
	version int
	gen     uint64
	ended   bool // the file ends with LogNext
	buf     []byte
	started bool
	// skip has bit 1<<stream set for each stream whose output is skipped
	// up to the end of its line: the start of the line is in a file
	// that is gone.
	skip byte
	// Skipped counts the files rotated away before they could be read.
	Skipped int
}

func NewLogReader(dir string) *LogReader { return &LogReader{dir: dir} }

// Close closes the file being read.
func (r *LogReader) Close() error {
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Next returns the next frame of output.
func (r *LogReader) Next() (LogFrame, error) {
	for {
		if r.f == nil {
			ok, err := r.openNext()
			if err != nil || !ok {
				if err == nil {
					err = io.EOF
				}
				return LogFrame{}, err
			}
		}
		fr, err := r.frame()
		if err == io.EOF {
			// Move on once this file is done with: it was rotated away, or it
			// is a version 1 log that newer files follow.
			if r.ended || r.version == 1 {
				ok, oerr := r.openAfter()
				if oerr != nil {
					return LogFrame{}, oerr
				}
				if ok {
					continue
				}
			}
			return LogFrame{}, io.EOF
		}
		if err != nil {
			return LogFrame{}, err
		}
		if fr.Kind == LogNext {
			r.ended = true
			continue
		}
		if bit := byte(1) << fr.Kind; r.skip&bit != 0 {
			i := bytes.IndexByte(fr.Data, '\n')
			if i < 0 {
				continue
			}
			r.skip &^= bit
			if fr.Data = fr.Data[i+1:]; len(fr.Data) == 0 {
				continue
			}
		}
		return fr, nil
	}
}

// openNext opens the oldest file, the first time.
func (r *LogReader) openNext() (bool, error) {
	if r.started {
		return false, nil
	}
	files, err := LogFiles(r.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, lf := range files {
		if r.open(lf) {
			r.started, r.skip = true, lf.Partial
			return true, nil
		}
	}
	return false, nil
}

// openAfter opens the file after the current one: the next generation, or
// after a version 1 file, the first.
func (r *LogReader) openAfter() (bool, error) {
	files, err := LogFiles(r.dir)
	if err != nil {
		return false, nil
	}
	var next *LogFile
	for i := range files {
		lf := &files[i]
		if lf.Version != 2 || lf.Gen <= r.gen {
			continue
		}
		if next == nil || lf.Gen < next.Gen {
			next = lf
		}
	}
	if next == nil {
		return false, nil
	}
	old, prevGen, prevVersion := r.f, r.gen, r.version
	if !r.open(*next) {
		return false, nil
	}
	if prevVersion == 2 && next.Gen > prevGen+1 {
		// The files between are gone, and with them the start of any line
		// this one starts in the middle of.
		r.Skipped += int(next.Gen - prevGen - 1)
		r.skip = next.Partial
	}
	old.Close()
	return true, nil
}

// open opens lf and reads past its header.
func (r *LogReader) open(lf LogFile) bool {
	f, err := OpenShared(lf.Path)
	if err != nil {
		return false
	}
	h, err := readLogHeader(f)
	if err != nil || h.Gen != lf.Gen || h.Version != lf.Version {
		// Rotated and replaced since it was listed.
		f.Close()
		return false
	}
	if h.Version == 2 {
		if _, err := f.Seek(int64(logHeaderSize), io.SeekStart); err != nil {
			f.Close()
			return false
		}
	}
	r.f, r.version, r.gen, r.ended, r.buf = f, h.Version, h.Gen, false, r.buf[:0]
	return true
}

// frame reads one whole frame from the current file, keeping one cut
// short, as when it is being written, for the next call.
func (r *LogReader) frame() (LogFrame, error) {
	hsize := frameHeader
	if r.version == 2 {
		hsize = frameHeaderV2
	}
	for {
		if len(r.buf) >= hsize {
			var size int
			var err error
			if r.version == 2 {
				size, err = frameSizeV2(r.buf)
			} else {
				size, err = frameSize(r.buf)
			}
			if err != nil {
				return LogFrame{}, fmt.Errorf("the log is damaged")
			}
			if len(r.buf) >= hsize+size {
				fr := LogFrame{Kind: r.buf[0], Data: bytes.Clone(r.buf[hsize : hsize+size])}
				if r.version == 2 {
					fr.Time = time.Unix(0, int64(binary.BigEndian.Uint64(r.buf[8:16])))
				}
				r.buf = r.buf[:copy(r.buf, r.buf[hsize+size:])]
				return fr, nil
			}
		}
		var chunk [64 << 10]byte
		n, err := r.f.Read(chunk[:])
		r.buf = append(r.buf, chunk[:n]...)
		if n == 0 {
			if err == nil || err == io.EOF {
				return LogFrame{}, io.EOF
			}
			return LogFrame{}, err
		}
	}
}

// LogFilter chooses what of a log to print, as docker logs does.
type LogFilter struct {
	// Since and Until, if set, keep the lines written at or after Since and
	// at or before Until. Lines of a version 1 log have no time: Since
	// leaves them out, Until keeps them.
	Since, Until time.Time
	// Tail keeps only the last Tail lines; negative keeps all.
	Tail int
	// Timestamps prints the time each line was written before it.
	Timestamps bool
}

// LogTimeFormat is how times are printed, as Docker prints them.
const LogTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// logLines cuts each stream's frames into lines: a line is its bytes up to
// and including a newline, and takes the time of the frame its first byte
// came in.
type logLines struct {
	open [3]bool      // whether the stream's last line lacks its newline
	at   [3]time.Time // when each stream's open line started
}

// piece is the part of a frame within one line.
type piece struct {
	stream byte
	start  bool      // the line starts here
	time   time.Time // the line's
	data   []byte
}

func (l *logLines) cut(fr LogFrame, emit func(piece) error) error {
	s := fr.Kind
	if s != Stdout && s != Stderr {
		s = Stdout
	}
	data := fr.Data
	for len(data) > 0 {
		p := piece{stream: s}
		if !l.open[s] {
			l.at[s], l.open[s] = fr.Time, true
			p.start = true
		}
		p.time = l.at[s]
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			p.data, data = data[:i+1], data[i+1:]
			l.open[s] = false
		} else {
			p.data, data = data, nil
		}
		if err := emit(p); err != nil {
			return err
		}
	}
	return nil
}

// LogPrinter prints a container's log through a LogFilter.
type LogPrinter struct {
	Stdout, Stderr io.Writer
	Filter         LogFilter

	r     *LogReader
	lines logLines
	// first is the first line kept by time to print, for Tail; kept counts
	// the lines kept by time so far, and printing says whether each
	// stream's current line is printed.
	first    int
	kept     int
	printing [3]bool
	// Done is set once a line written after Until has been read, after
	// which following can stop.
	Done bool

	// Output is buffered, a stream's buffer flushed before the other is
	// written to, so that they keep their order; last is the stream
	// written to last. stamp is the last time printed, as text.
	out, errs *bufio.Writer
	last      byte
	stampAt   time.Time
	stamp     string
}

// writer is where stream's output goes, the other stream's flushed first.
func (p *LogPrinter) writer(stream byte) (*bufio.Writer, error) {
	if p.out == nil {
		p.out, p.errs = bufio.NewWriterSize(p.Stdout, 64<<10), bufio.NewWriterSize(p.Stderr, 64<<10)
	}
	w, other := p.out, p.errs
	if stream == Stderr {
		w, other = p.errs, p.out
	}
	if p.last != stream && p.last != 0 {
		if err := other.Flush(); err != nil {
			return nil, err
		}
	}
	p.last = stream
	return w, nil
}

// flush writes out what is buffered.
func (p *LogPrinter) flush() error {
	if p.out == nil {
		return nil
	}
	err := p.out.Flush()
	if e := p.errs.Flush(); err == nil {
		err = e
	}
	return err
}

// NewLogPrinter prepares to print the log in dir. With a Tail, it reads the
// log through once to count its lines.
func NewLogPrinter(dir string, stdout, stderr io.Writer, f LogFilter) (*LogPrinter, error) {
	p := &LogPrinter{Stdout: stdout, Stderr: stderr, Filter: f, r: NewLogReader(dir)}
	if f.Tail >= 0 {
		count := NewLogReader(dir)
		defer count.Close()
		var lines logLines
		kept := 0
		for {
			fr, err := count.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			lines.cut(fr, func(pc piece) error {
				if pc.start && p.keepTime(pc.time) {
					kept++
				}
				return nil
			})
		}
		p.first = max(kept-f.Tail, 0)
	}
	return p, nil
}

// keepTime reports whether a line written at t is printed, by time alone.
func (p *LogPrinter) keepTime(t time.Time) bool {
	if !p.Filter.Since.IsZero() && (t.IsZero() || t.Before(p.Filter.Since)) {
		return false
	}
	if !p.Filter.Until.IsZero() && t.After(p.Filter.Until) {
		return false
	}
	return true
}

// Copy prints what the log holds now, and returns how many bytes of output
// it printed.
func (p *LogPrinter) Copy() (int64, error) {
	total, err := p.copy()
	if ferr := p.flush(); err == nil {
		err = ferr
	}
	return total, err
}

func (p *LogPrinter) copy() (int64, error) {
	var total int64
	for {
		fr, err := p.r.Next()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		err = p.lines.cut(fr, func(pc piece) error {
			if pc.start {
				if !p.Filter.Until.IsZero() && pc.time.After(p.Filter.Until) {
					p.Done = true
				}
				print := false
				if p.keepTime(pc.time) {
					print = p.kept >= p.first
					p.kept++
				}
				p.printing[pc.stream] = print
			}
			if !p.printing[pc.stream] {
				return nil
			}
			w, err := p.writer(pc.stream)
			if err != nil {
				return err
			}
			if pc.start && p.Filter.Timestamps && !pc.time.IsZero() {
				// Lines from one frame share its time.
				if !pc.time.Equal(p.stampAt) || p.stamp == "" {
					p.stampAt, p.stamp = pc.time, pc.time.UTC().Format(LogTimeFormat)+" "
				}
				if _, err := w.WriteString(p.stamp); err != nil {
					return err
				}
			}
			n, err := w.Write(pc.data)
			total += int64(n)
			return err
		})
		if err != nil {
			return total, err
		}
	}
}

// Skipped counts the files rotated away before they could be printed.
func (p *LogPrinter) Skipped() int { return p.r.Skipped }

// Close closes the file being read.
func (p *LogPrinter) Close() error { return p.r.Close() }
