//go:build windows

package vmm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
)

// layerHeldWait is how long openLayer waits for a dead runwhp to close a
// writable layer; tests shorten it.
var layerHeldWait = 2 * time.Second

// ScratchPath is where a new scratch disk goes: the store's scratch
// directory, which sweepStore empties of disks left by runs the machine
// stopped under.
func ScratchPath(root string) string {
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
	for deadline := time.Now().Add(layerHeldWait); errors.Is(err, disk.ErrInUse) && time.Now().Before(deadline); {
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

// PendingLayer is the writable layer being opened in the background.
type PendingLayer struct {
	ch <-chan layerResult
	// scratch says the layer is thrown away after this boot: the guest
	// mounts it volatile, and is offered no FLUSH.
	scratch bool
	// borrowed says the file is another's, which Close leaves open.
	borrowed  bool
	once      sync.Once
	res       layerResult
	closeOnce sync.Once
}

// ReadyLayer is a writable layer already open: f, or none, with f nil,
// which gives the guest one in memory. scratch is as for StartLayer.
func ReadyLayer(f *disk.File, scratch bool) *PendingLayer {
	ch := make(chan layerResult, 1)
	ch <- layerResult{file: f}
	return &PendingLayer{ch: ch, scratch: scratch}
}

// StartLayer starts opening the writable layer while the caller does other
// work.
func StartLayer(path string, size int64, scratch bool) *PendingLayer {
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
	return &PendingLayer{ch: ch, scratch: scratch}
}

// get waits for the writable layer.
func (p *PendingLayer) get() layerResult {
	p.once.Do(func() { p.res = <-p.ch })
	return p.res
}

// Close waits for the writable layer and closes it, which deletes a scratch
// one.
func (p *PendingLayer) Close() {
	p.closeOnce.Do(func() {
		if r := p.get(); r.file != nil && !p.borrowed {
			r.file.Close()
		}
	})
}

// A StepLayer is the writable layer of one step of a build. Each guest of
// the step mounts it as a container's kept layer, so a second guest can
// read what the first left, but the build holds it open throughout, so
// nothing sweeps it away between them, and it is deleted when closed.
type StepLayer struct {
	f *disk.File
}

// NewStepLayer makes a step's writable layer of size bytes in the store's
// scratch directory.
func NewStepLayer(root string, size int64) (*StepLayer, error) {
	path := ScratchPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := disk.Create(path, size, disk.Scratch)
	if err != nil {
		return nil, err
	}
	if err := ext4.Format(f, size, ext4.Options{Journal: true, Label: "rw"}); err != nil {
		f.Close()
		return nil, fmt.Errorf("formatting a build step's writable layer: %w", err)
	}
	return &StepLayer{f: f}, nil
}

// pending is the layer for one guest, which leaves it open when it stops.
func (l *StepLayer) pending() *PendingLayer {
	p := ReadyLayer(l.f, false)
	p.borrowed = true
	return p
}

// Close deletes the layer.
func (l *StepLayer) Close() { l.f.Close() }
