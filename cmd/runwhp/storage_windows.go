//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/disk"
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

// scratchPath is where a new scratch disk goes: the store's scratch
// directory, which sweepStore empties of disks left by runs the machine
// stopped under.
func scratchPath(root string) string {
	var b [8]byte
	rand.Read(b[:])
	return filepath.Join(root, "scratch", hex.EncodeToString(b[:])+".ext4")
}

// openLayer opens a container's writable layer, a sparse file at path
// holding an ext4 file system of size bytes. A kept layer is made and
// formatted, with a journal, the first time, and opened as it is after
// that; a scratch one is made for this run, and deleted when it is closed
// or runwhp ends, however it ends.
func openLayer(path string, size int64, scratch bool) (*disk.File, error) {
	if scratch {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := disk.Create(path, size, disk.Scratch)
		if err != nil {
			return nil, err
		}
		if err := ext4.Format(f, size, ext4.Options{Label: "scratch"}); err != nil {
			f.Close()
			return nil, fmt.Errorf("formatting the scratch disk: %w", err)
		}
		return f, nil
	}
	f, err := disk.Open(path, disk.Exclusive)
	// The caller holds the container's lock, so whoever else has the layer
	// open is a runwhp that died and is still closing its handles, which
	// a killed process closes in no set order.
	for deadline := time.Now().Add(2 * time.Second); errors.Is(err, disk.ErrInUse) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		f, err = disk.Open(path, disk.Exclusive)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	// Made aside and moved into place once formatted, so a layer is never
	// left half made.
	tmp := path + ".new"
	os.Remove(tmp)
	if f, err = disk.Create(tmp, size, disk.Exclusive); err != nil {
		return nil, err
	}
	err = ext4.Format(f, size, ext4.Options{Journal: true, Label: "rw"})
	f.Close()
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("making the writable layer: %w", err)
	}
	return disk.Open(path, disk.Exclusive)
}

// layerResult is what opening the writable layer in the background
// produced. file is nil, with err nil, when the store's volume has no
// sparse files and the writable layer stays in memory.
type layerResult struct {
	file *disk.File
	err  error
	took time.Duration
}

// pendingLayer is the writable layer being opened in the background.
type pendingLayer struct {
	ch        <-chan layerResult
	once      sync.Once
	res       layerResult
	closeOnce sync.Once
}

// startLayer starts opening the writable layer while the caller does other
// work.
func startLayer(path string, size int64, scratch bool) *pendingLayer {
	ch := make(chan layerResult, 1)
	go func() {
		start := time.Now()
		f, err := openLayer(path, size, scratch)
		if errors.Is(err, disk.ErrNotSparse) {
			errorf("warning: the store's volume has no sparse files, so the container's writable layer stays in memory and is lost when it stops")
			f, err = nil, nil
		}
		ch <- layerResult{f, err, time.Since(start)}
	}()
	return &pendingLayer{ch: ch}
}

// get waits for the writable layer.
func (p *pendingLayer) get() layerResult {
	p.once.Do(func() { p.res = <-p.ch })
	return p.res
}

// close waits for the writable layer and closes it, which deletes a scratch
// one.
func (p *pendingLayer) close() {
	p.closeOnce.Do(func() {
		if r := p.get(); r.file != nil {
			r.file.Close()
		}
	})
}
