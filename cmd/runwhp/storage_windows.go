//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/ext4"
	"github.com/quasidock/runwhp/internal/storefile"
)

// defaultDiskSize is the size of a scratch disk or volume unless asked
// otherwise. Disks are sparse, so the size is a limit, not what they take.
const defaultDiskSize = 64 << 30

// parseSize reads a size the way Docker does: a number, possibly with a
// fraction, and a b, k, m, g or t suffix, bytes without one.
func parseSize(s string) (uint64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := float64(1)
	if t != "" {
		switch t[len(t)-1] {
		case 't':
			mult, t = 1<<40, t[:len(t)-1]
		case 'g':
			mult, t = 1<<30, t[:len(t)-1]
		case 'm':
			mult, t = 1<<20, t[:len(t)-1]
		case 'k':
			mult, t = 1<<10, t[:len(t)-1]
		case 'b':
			t = t[:len(t)-1]
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n <= 0 || n*mult >= 1<<63 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(n * mult), nil
}

// parseDiskSize reads the size of a scratch disk or volume, which must fit
// an ext4 file system runwhp can make: from 1g to 16t.
func parseDiskSize(s string) (int64, error) {
	size, err := parseSize(s)
	if err != nil {
		return 0, err
	}
	if size < 1<<30 || size > 16<<40 {
		return 0, fmt.Errorf("size %s is out of range: give 1g to 16t", strings.TrimSpace(s))
	}
	return min(int64(size), ext4.MaxSize) &^ 4095, nil
}

// parseShared reads a volume's shared option: a volume made shared is
// served by its own guest from its first use, so containers can use it
// at once without any being started again.
func parseShared(s string) (bool, error) {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid shared=%q: give true or false", s)
	}
	return v, nil
}

// parseStorageOpts reads --storage-opt, of which runwhp knows size=SIZE:
// how large the container's writable layer may grow.
func parseStorageOpts(opts []string) (int64, error) {
	size := int64(defaultDiskSize)
	for _, o := range opts {
		k, v, _ := strings.Cut(o, "=")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "size":
			var err error
			if size, err = parseDiskSize(v); err != nil {
				return 0, fmt.Errorf("--storage-opt %s: %w", o, err)
			}
		default:
			return 0, fmt.Errorf("--storage-opt %s: unknown option; runwhp knows size=SIZE", o)
		}
	}
	return size, nil
}

// staleTemp is how old a file in the store's tmp directory must be before
// sweepStore removes it. Younger ones may be a pull in progress.
const staleTemp = time.Hour

// sweepStore removes what runs that did not end normally left in the store:
// scratch disks, which Windows deletes itself unless the machine stopped
// first, old temporary files, and image records a pull stopped writing. A
// scratch disk still in use cannot be removed, since its runwhp holds it
// open exclusively, and is skipped.
func sweepStore(root string, logf func(string, ...any)) {
	if entries, err := os.ReadDir(filepath.Join(root, "scratch")); err == nil {
		for _, e := range entries {
			p := filepath.Join(root, "scratch", e.Name())
			if err := os.Remove(p); err == nil {
				logf("removed scratch disk %s left by an earlier run", e.Name())
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "tmp")); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleTemp {
				os.RemoveAll(filepath.Join(root, "tmp", e.Name()))
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "images")); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json.tmp") && !strings.Contains(e.Name(), ".json"+storefile.TempSuffix) {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleTemp {
				os.Remove(filepath.Join(root, "images", e.Name()))
			}
		}
	}
}
