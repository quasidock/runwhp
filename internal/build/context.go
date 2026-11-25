package build

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// Context is a build context: a directory on the host, less what its
// .dockerignore excludes. Its files are sent as BuildKit's client sends
// them from Windows: owned by root, directories and files 0755, read-only
// files 0555, with their modification times; symlinks as they are.
type Context struct {
	dir string
	pm  *patternmatcher.PatternMatcher
}

// NewContext opens the context dir, excluding what ignore's patterns match.
func NewContext(dir string, ignore []string) (*Context, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare context: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("unable to prepare context: %s is not a directory", dir)
	}
	c := &Context{dir: abs}
	if len(ignore) > 0 {
		if c.pm, err = patternmatcher.New(ignore); err != nil {
			return nil, fmt.Errorf("the .dockerignore: %w", err)
		}
	}
	return c, nil
}

// ReadIgnore reads the ignore patterns for a build: the Dockerfile's own,
// <Dockerfile>.dockerignore beside it, else the context's .dockerignore.
func ReadIgnore(dir, dockerfile string) ([]string, error) {
	paths := []string{filepath.Join(dir, ".dockerignore")}
	if dockerfile != "" {
		paths = append([]string{dockerfile + ".dockerignore"}, paths...)
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ignorefile.ReadAll(f)
	}
	return nil, nil
}

// Dir is the context's directory.
func (c *Context) Dir() string { return c.dir }

// rel is p, a slash path from the context's root, as the patterns take it.
func rel(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }

// host is p's path on the host.
func (c *Context) host(p string) string {
	r := rel(p)
	if r == "" {
		return c.dir
	}
	return filepath.Join(c.dir, filepath.FromSlash(r))
}

// hostPath is the host path of p, a RUN --mount=type=bind source.
func (c *Context) hostPath(p string) (string, error) {
	r, err := c.resolve(path.Clean("/" + p))
	if err != nil {
		return "", err
	}
	return c.host(r), nil
}

// excluded reports whether .dockerignore excludes p.
func (c *Context) excluded(p string) bool {
	r := rel(p)
	if c.pm == nil || r == "" {
		return false
	}
	ok, err := c.pm.MatchesOrParentMatches(r)
	return err == nil && ok
}

// resolve follows p's symlinks within the context: a symlink cannot lead
// out of it, as a target past its root ends at its root.
func (c *Context) resolve(p string) (string, error) {
	p = path.Clean("/" + p)
	for range 255 {
		cur := "/"
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		changed := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := path.Join(cur, part)
			fi, err := os.Lstat(c.host(next))
			if err != nil {
				return "", notFound(p)
			}
			if fi.Mode()&fs.ModeSymlink == 0 {
				cur = next
				continue
			}
			target, err := os.Readlink(c.host(next))
			if err != nil {
				return "", err
			}
			target = filepath.ToSlash(target)
			if !path.IsAbs(target) && !filepath.IsAbs(filepath.FromSlash(target)) {
				target = path.Join(cur, target)
			} else if vol := filepath.VolumeName(target); vol != "" {
				// An absolute Windows target: within the context, or nowhere.
				t, err := filepath.Rel(c.dir, filepath.FromSlash(target))
				if err != nil || strings.HasPrefix(t, "..") {
					return "", notFound(p)
				}
				target = "/" + filepath.ToSlash(t)
			}
			p = path.Join(append([]string{"/", target}, parts[i+1:]...)...)
			changed = true
			break
		}
		if !changed {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
}

// notFound is BuildKit's error for a source that is not there.
func notFound(p string) error {
	return fmt.Errorf("failed to compute cache key: %q: not found", path.Clean("/"+p))
}

// glob lists what pattern matches in the context, sorted, less what is
// excluded.
func (c *Context) glob(_ context.Context, pattern string) ([]string, error) {
	pattern = path.Clean("/" + pattern)
	matches, err := filepath.Glob(c.host(pattern))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		r, err := filepath.Rel(c.dir, m)
		if err != nil || strings.HasPrefix(r, "..") {
			continue
		}
		p := "/" + filepath.ToSlash(r)
		if !c.excluded(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// stat describes p, its symlinks followed.
func (c *Context) stat(_ context.Context, p string) (srcStat, error) {
	if c.excluded(p) {
		return srcStat{}, notFound(p)
	}
	r, err := c.resolve(p)
	if err != nil {
		return srcStat{}, err
	}
	fi, err := os.Stat(c.host(r))
	if err != nil {
		return srcStat{}, notFound(p)
	}
	return srcStat{path: r, dir: fi.IsDir()}, nil
}

// walk calls fn with p, its symlinks followed, named ".", then, for a
// directory, everything below it the patterns do not exclude, in order,
// named from p.
func (c *Context) walk(ctx context.Context, p string, fn walkFunc) error {
	st, err := c.stat(ctx, p)
	if err != nil {
		return err
	}
	root := c.host(st.path)
	return filepath.WalkDir(root, func(hp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r, _ := filepath.Rel(root, hp)
		name := filepath.ToSlash(r)
		full := path.Join(st.path, name)
		if name != "." && c.excluded(full) {
			if d.IsDir() && (c.pm == nil || !c.pm.Exclusions()) {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			// Something below may be let back in: go on, but leave the
			// directory itself out.
			return nil
		}
		var fi fs.FileInfo
		if name == "." {
			fi, err = os.Stat(hp)
		} else {
			fi, err = d.Info()
		}
		if err != nil {
			return err
		}
		hdr, err := contextHeader(fi, hp, name)
		if err != nil {
			return fmt.Errorf("invalid file request %s", rel(full))
		}
		if hdr.Typeflag == tar.TypeReg {
			return fn(name, hdr, func() (io.ReadCloser, error) { return os.Open(hp) })
		}
		return fn(name, hdr, nil)
	})
}

// contextHeader is a context file's tar header, as BuildKit's client sends
// it from Windows.
func contextHeader(fi fs.FileInfo, hostPath, name string) (*tar.Header, error) {
	hdr := &tar.Header{Name: name, ModTime: fi.ModTime().Truncate(time.Second), Format: tar.FormatPAX}
	hdr.ModTime = fi.ModTime()
	mode := fi.Mode()
	switch {
	case mode.IsDir():
		hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
	case mode.IsRegular():
		hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o755, fi.Size()
		if mode.Perm()&0o200 == 0 {
			hdr.Mode = 0o555
		}
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(hostPath)
		if err != nil {
			return nil, err
		}
		hdr.Typeflag, hdr.Mode, hdr.Linkname = tar.TypeSymlink, 0o777, filepath.ToSlash(target)
	default:
		// Junctions, devices and the like.
		return nil, fmt.Errorf("%s: unsupported file type", name)
	}
	if runtime.GOOS != "windows" && !mode.IsDir() && mode&fs.ModeSymlink == 0 {
		hdr.Mode = int64(mode.Perm())
	}
	return hdr, nil
}

// hash is a digest of what copying p would send: names, types, modes,
// sizes, link targets and contents, but not times, which a checkout or a
// copy changes, as BuildKit leaves them out.
func (c *Context) hash(p string) (string, error) {
	h := sha256.New()
	err := c.walk(context.Background(), p, func(name string, hdr *tar.Header, open func() (io.ReadCloser, error)) error {
		fmt.Fprintf(h, "%q %c %o %d %q\n", name, hdr.Typeflag, hdr.Mode, hdr.Size, hdr.Linkname)
		if open == nil {
			return nil
		}
		f, err := open()
		if err != nil {
			return err
		}
		defer f.Close()
		fh := sha256.New()
		if _, err := io.Copy(fh, f); err != nil {
			return err
		}
		h.Write(fh.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
