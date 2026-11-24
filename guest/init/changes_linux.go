//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// changes lists what the writable layer, upper, changes in the layers
// below it, top first, as docker diff does: a path added, one whose file
// or metadata changed, one deleted, and every directory above a change.
// A directory whose times alone changed is not listed, nor is a file the
// container deleted that no layer below had. What is below the paths in
// excludeBelow, anonymous volumes, is passed over. Paths are sorted.
//
// It walks the writable layer alone, as Docker's overlay2 does where the
// kernel lacks redirect_dir, as runwhp's does, so its cost is that of the
// changes, not of the image.
func changes(upper *os.Root, lowers []*os.Root, excludeBelow []string) ([]guestspec.Change, error) {
	c := &changeWalker{upper: upper, lowers: lowers, exclude: excludeBelow, kinds: map[string]int{}}
	if err := c.walk("."); err != nil {
		return nil, err
	}
	// Every directory above a change has changed too.
	for p := range c.kinds {
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := c.kinds[dir]; ok {
				break
			}
			c.kinds[dir] = guestspec.ChangeModify
		}
	}
	out := make([]guestspec.Change, 0, len(c.kinds))
	for p, kind := range c.kinds {
		out = append(out, guestspec.Change{Path: "/" + p, Kind: kind})
	}
	slices.SortFunc(out, func(a, b guestspec.Change) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

type changeWalker struct {
	upper   *os.Root
	lowers  []*os.Root
	exclude []string
	kinds   map[string]int // by path, without the leading "/"
}

func (c *changeWalker) walk(dir string) error {
	f, err := c.upper.Open(dir)
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		rel := path.Join(dir, name)
		fi, err := c.upper.Lstat(rel)
		if err != nil {
			continue // changing as it is read
		}
		if isWhiteout(fi) {
			if _, ok := c.lower(rel); ok {
				c.kinds[rel] = guestspec.ChangeDelete
			}
			continue
		}
		lfi, inLower := c.lower(rel)
		switch {
		case !inLower:
			c.kinds[rel] = guestspec.ChangeAdd
		case fi.IsDir() && lfi.IsDir():
			if metadataDiffers(fi, lfi) {
				c.kinds[rel] = guestspec.ChangeModify
			}
			if c.opaque(c.upper, rel) {
				// The directory was deleted and made again: what the layers
				// below had in it is gone, unless made again too.
				for _, n := range c.lowerNames(rel) {
					child := path.Join(rel, n)
					if _, err := c.upper.Lstat(child); errors.Is(err, fs.ErrNotExist) {
						c.kinds[child] = guestspec.ChangeDelete
					}
				}
			}
		default:
			c.kinds[rel] = guestspec.ChangeModify
		}
		if fi.IsDir() && !slices.Contains(c.exclude, rel) {
			if err := c.walk(rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// metadataDiffers compares two directories as Docker's diff does: by
// their mode and owner, not their size or times.
func metadataDiffers(a, b fs.FileInfo) bool {
	sa, sb := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
	return a.Mode() != b.Mode() || sa.Uid != sb.Uid || sa.Gid != sb.Gid
}

// lower finds rel in the layers below, top first, as overlayfs would show
// it: a whiteout, or an opaque directory above it, hides what is below.
func (c *changeWalker) lower(rel string) (fs.FileInfo, bool) {
	for _, l := range c.lowers {
		fi, err := l.Lstat(rel)
		if err == nil {
			if isWhiteout(fi) {
				return nil, false
			}
			return fi, true
		}
		if c.opaque(l, path.Dir(rel)) {
			return nil, false
		}
	}
	return nil, false
}

// lowerNames lists what the layers below have in the directory rel.
func (c *changeWalker) lowerNames(rel string) []string {
	seen := map[string]bool{}
	var names []string
	for _, l := range c.lowers {
		f, err := l.Open(rel)
		if err != nil {
			continue
		}
		ns, _ := f.Readdirnames(-1)
		f.Close()
		for _, n := range ns {
			if seen[n] {
				continue
			}
			seen[n] = true
			if fi, err := l.Lstat(path.Join(rel, n)); err == nil && !isWhiteout(fi) {
				names = append(names, n)
			}
		}
		if c.opaque(l, rel) {
			break
		}
	}
	slices.Sort(names)
	return names
}

// opaque reports whether the directory rel of layer l hides what the
// layers below have in it.
func (c *changeWalker) opaque(l *os.Root, rel string) bool {
	if rel == "." {
		return false
	}
	f, err := l.Open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	v, _ := fgetxattr(f, opaqueXattr)
	return string(v) == "y"
}

// isWhiteout reports whether fi is overlayfs's mark of a deleted file: a
// character device 0/0.
func isWhiteout(fi fs.FileInfo) bool {
	if fi.Mode()&fs.ModeCharDevice == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Rdev == 0
}
