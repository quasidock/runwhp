package build

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path"
)

// spoolFS is what a COPY copies from another stage or image, read from
// that root's guest and kept in a file meanwhile: a process runs one
// guest at a time, and the step's own guest boots once the source's is
// gone.
type spoolFS struct {
	f     *os.File
	globs map[string][]string
	stats map[string]srcStat
	spans map[string][2]int64 // each path's tar in the file
}

// newSpool reads what srcs name from src into a file in dir.
func newSpool(ctx context.Context, src sourceFS, srcs []copySrc, dir string) (*spoolFS, error) {
	f, err := os.CreateTemp(dir, "copy-*")
	if err != nil {
		return nil, err
	}
	s := &spoolFS{f: f, globs: map[string][]string{}, stats: map[string]srcStat{}, spans: map[string][2]int64{}}
	err = func() error {
		cw := &countWriter{w: f}
		for _, c := range srcs {
			if c.file != nil {
				continue
			}
			pattern := c.pattern
			paths := []string{path.Clean("/" + pattern)}
			if containsWildcards(pattern) {
				m, err := src.glob(ctx, pattern)
				if err != nil {
					return err
				}
				s.globs[path.Clean("/"+pattern)] = m
				paths = m
			}
			for _, p := range paths {
				if _, done := s.stats[p]; done {
					continue
				}
				st, err := src.stat(ctx, p)
				if err != nil {
					return err
				}
				s.stats[p] = st
				start := cw.n
				tw := tar.NewWriter(cw)
				if err := src.walk(ctx, p, func(_ string, hdr *tar.Header, open func() (io.ReadCloser, error)) error {
					if err := tw.WriteHeader(hdr); err != nil {
						return err
					}
					if open == nil {
						return nil
					}
					rc, err := open()
					if err != nil {
						return err
					}
					defer rc.Close()
					_, err = io.Copy(tw, rc)
					return err
				}); err != nil {
					return err
				}
				if err := tw.Close(); err != nil {
					return err
				}
				s.spans[p] = [2]int64{start, cw.n}
			}
		}
		return nil
	}()
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *spoolFS) close() {
	s.f.Close()
	os.Remove(s.f.Name())
}

func (s *spoolFS) glob(_ context.Context, pattern string) ([]string, error) {
	return s.globs[path.Clean("/"+pattern)], nil
}

func (s *spoolFS) stat(_ context.Context, p string) (srcStat, error) {
	st, ok := s.stats[path.Clean("/"+p)]
	if !ok {
		return srcStat{}, notFound(p)
	}
	return st, nil
}

func (s *spoolFS) walk(ctx context.Context, p string, fn walkFunc) error {
	span, ok := s.spans[path.Clean("/"+p)]
	if !ok {
		return notFound(p)
	}
	tr := tar.NewReader(io.NewSectionReader(s.f, span[0], span[1]-span[0]))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var open func() (io.ReadCloser, error)
		if hdr.Typeflag == tar.TypeReg {
			open = func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }
		}
		if err := fn(hdr.Name, hdr, open); err != nil {
			return err
		}
	}
}

// countWriter counts what it writes.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
