package build

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// progress prints a build's steps as BuildKit's plain progress does: each
// step a numbered vertex, its output lines with the seconds since it
// started, and how it ended.
type Progress struct {
	w   io.Writer
	now func() time.Time
	mu  sync.Mutex
	n   int
}

// NewProgress prints a build's progress to w, nil for none, timed by now.
func NewProgress(w io.Writer, now func() time.Time) *Progress {
	if w == nil {
		w = io.Discard
	}
	if now == nil {
		now = time.Now
	}
	return &Progress{w: w, now: now}
}

// next numbers a new vertex.
func (p *Progress) next() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return p.n
}

// Vertex starts a vertex of the build's own, for its caller to end, as
// the CLI does for exporting the image.
func (p *Progress) Vertex(name string) *Vertex { return p.start(name) }

// cached prints a step the cache had.
func (p *Progress) cached(name string) {
	n := p.next()
	fmt.Fprintf(p.w, "#%d %s\n#%d CACHED\n\n", n, name, n)
}

// instant prints a step that took no time.
func (p *Progress) instant(name string) {
	n := p.next()
	fmt.Fprintf(p.w, "#%d %s\n#%d DONE 0.0s\n\n", n, name, n)
}

// start prints a step's name, and returns its vertex.
func (p *Progress) start(name string) *Vertex {
	v := &Vertex{p: p, n: p.next(), name: name, start: p.now()}
	fmt.Fprintf(p.w, "#%d %s\n", v.n, name)
	return v
}

// Vertex is a step under way; its output goes through Write.
type Vertex struct {
	p     *Progress
	n     int
	name  string
	start time.Time
	mu    sync.Mutex
	buf   []byte
	// tail keeps the last lines, for the error's account.
	tail []string
}

// Write prints the step's output, line by line.
func (v *Vertex) Write(b []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buf = append(v.buf, b...)
	for {
		i := bytes.IndexByte(v.buf, '\n')
		if i < 0 {
			break
		}
		v.line(string(v.buf[:i]))
		v.buf = v.buf[i+1:]
	}
	return len(b), nil
}

func (v *Vertex) line(s string) {
	s = strings.TrimSuffix(s, "\r")
	elapsed := v.p.now().Sub(v.start).Seconds()
	fmt.Fprintf(v.p.w, "#%d %.3f %s\n", v.n, elapsed, s)
	v.tail = append(v.tail, fmt.Sprintf("%.3f %s", elapsed, s))
	if len(v.tail) > 10 {
		v.tail = v.tail[1:]
	}
}

// Log prints a line of the build's own in the vertex.
func (v *Vertex) Log(format string, a ...any) {
	fmt.Fprintf(v.p.w, "#%d "+format+"\n", append([]any{v.n}, a...)...)
}

// end prints how the step ended.
func (v *Vertex) end(err error, cached bool) {
	v.mu.Lock()
	if len(v.buf) > 0 {
		v.line(string(v.buf))
		v.buf = nil
	}
	v.mu.Unlock()
	switch {
	case err != nil:
		fmt.Fprintf(v.p.w, "#%d ERROR: %v\n", v.n, err)
		fmt.Fprintf(v.p.w, "------\n > %s:\n", v.name)
		for _, l := range v.tail {
			fmt.Fprintln(v.p.w, l)
		}
		fmt.Fprintf(v.p.w, "------\n")
	case cached:
		fmt.Fprintf(v.p.w, "#%d CACHED\n\n", v.n)
	default:
		fmt.Fprintf(v.p.w, "#%d DONE %.1fs\n\n", v.n, v.p.now().Sub(v.start).Seconds())
	}
}

// End is end, for the CLI's own vertices.
func (v *Vertex) End(err error) { v.end(err, false) }
