package compose

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// colours are those Docker Compose gives containers' prefixes, in the
// order it gives them.
var colours = []string{"36", "33", "32", "35", "34", "1;36", "1;33", "1;32", "1;35", "1;34"}

// Prefixer writes the output of several containers, a line at a time,
// each line with its container's name before it, as Docker Compose's logs
// and attached up do: "web-1  | line", names padded to the longest,
// coloured on a console.
type Prefixer struct {
	mu      sync.Mutex
	width   int
	colour  bool
	prefix  bool
	colours map[string]string
}

// NewPrefixer makes a Prefixer for names as long as width at most;
// without prefix, lines go as they are.
func NewPrefixer(width int, colour, prefix bool) *Prefixer {
	return &Prefixer{width: width, colour: colour, prefix: prefix, colours: map[string]string{}}
}

// Width is the longest of names, for NewPrefixer.
func Width(names []string) int {
	w := 0
	for _, n := range names {
		w = max(w, len(n))
	}
	return w
}

func (p *Prefixer) colourOf(name string) string {
	c, ok := p.colours[name]
	if !ok {
		c = colours[len(p.colours)%len(colours)]
		p.colours[name] = c
	}
	return c
}

// Writer returns a writer that passes a container's output on to w, a
// line at a time, prefixed.
func (p *Prefixer) Writer(name string, w io.Writer) io.WriteCloser {
	p.mu.Lock()
	p.colourOf(name)
	p.mu.Unlock()
	return &prefixed{p: p, name: name, w: w}
}

func (p *Prefixer) head(name string) string {
	if !p.prefix {
		return ""
	}
	h := fmt.Sprintf("%-*s  | ", p.width, name)
	if p.colour {
		return "\x1b[" + p.colourOf(name) + "m" + h + "\x1b[0m"
	}
	return h
}

// Exited writes Docker Compose's line for a container that has stopped,
// to w.
func (p *Prefixer) Exited(w io.Writer, name string, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := fmt.Sprintf("\r\x1b[K%s exited with code %d\n", name, code)
	if p.colour {
		line = "\x1b[" + p.colourOf(name) + "m" + line + "\x1b[0m"
	}
	io.WriteString(w, line)
}

type prefixed struct {
	p    *Prefixer
	name string
	w    io.Writer
	buf  []byte
}

func (l *prefixed) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(l.buf[:i+1])
		l.buf = l.buf[i+1:]
	}
	return len(b), nil
}

// emit writes one line, whole, so that lines of different containers do
// not mix.
func (l *prefixed) emit(line []byte) {
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	out := append([]byte(l.p.head(l.name)), line...)
	l.w.Write(out)
}

// Close writes what is left of a last line, ended.
func (l *prefixed) Close() error {
	if len(l.buf) > 0 {
		l.emit(append(l.buf, '\n'))
		l.buf = nil
	}
	return nil
}
