//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
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
// first, and old temporary files. A scratch disk still in use cannot be
// removed, since its runwhp holds it open exclusively, and is skipped.
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
}

// newScratch makes this run's scratch disk: a sparse file of size bytes
// in the store holding an empty ext4 file system, deleted when it is closed
// or runwhp ends, however it ends.
func newScratch(root string, size int64) (*disk.File, error) {
	dir := filepath.Join(root, "scratch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var b [8]byte
	rand.Read(b[:])
	f, err := disk.Create(filepath.Join(dir, hex.EncodeToString(b[:])+".ext4"), size, disk.Scratch)
	if err != nil {
		return nil, err
	}
	if err := ext4.Format(f, size, ext4.Options{Label: "scratch"}); err != nil {
		f.Close()
		return nil, fmt.Errorf("formatting the scratch disk: %w", err)
	}
	return f, nil
}

// scratchResult is what making the scratch disk in the background produced.
// file is nil, with err nil, when the store's volume has no sparse files
// and the writable layer stays in memory.
type scratchResult struct {
	file *disk.File
	err  error
	took time.Duration
}

// pendingScratch is the scratch disk being made in the background.
type pendingScratch struct {
	ch   <-chan scratchResult
	once sync.Once
	res  scratchResult
}

// startScratch starts making the scratch disk while the caller does other
// work.
func startScratch(root string, size int64) *pendingScratch {
	ch := make(chan scratchResult, 1)
	go func() {
		start := time.Now()
		f, err := newScratch(root, size)
		if errors.Is(err, disk.ErrNotSparse) {
			errorf("warning: the store's volume has no sparse files, so the container's writable layer stays in memory")
			f, err = nil, nil
		}
		ch <- scratchResult{f, err, time.Since(start)}
	}()
	return &pendingScratch{ch: ch}
}

// get waits for the scratch disk.
func (p *pendingScratch) get() scratchResult {
	p.once.Do(func() { p.res = <-p.ch })
	return p.res
}

// close waits for the scratch disk and closes it, which deletes it.
func (p *pendingScratch) close() {
	if r := p.get(); r.file != nil {
		r.file.Close()
	}
}
