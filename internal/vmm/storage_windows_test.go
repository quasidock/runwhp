//go:build windows

package vmm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
)

func TestOpenLayerScratch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rw.ext4")
	f, err := openLayer(path, 2<<30, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ext4.ReadState(f)
	if err != nil || !s.Clean || s.Journal {
		t.Errorf("scratch file system: %+v, %v", s, err)
	}
	if n, err := disk.Allocated(path); err != nil || n > 1<<20 {
		t.Errorf("a new 2 GiB scratch disk takes %d bytes (%v)", n, err)
	}
	if _, err := disk.Open(path, disk.ReadOnly); !errors.Is(err, disk.ErrInUse) {
		t.Errorf("opening a scratch disk in use: %v, want ErrInUse", err)
	}
	f.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch disk left after Close: %v", err)
	}
}

func TestOpenLayerKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rw.ext4")
	f, err := openLayer(path, 2<<30, false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ext4.ReadState(f)
	if err != nil || !s.Clean || !s.Journal {
		t.Errorf("kept file system: %+v, %v", s, err)
	}
	if _, err := openLayer(path, 2<<30, false); !errors.Is(err, disk.ErrInUse) {
		t.Errorf("opening a layer in use: %v, want ErrInUse", err)
	}
	// What is written stays, and is not formatted over.
	if _, err := f.WriteAt([]byte("kept"), 1<<30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	f, err = openLayer(path, 2<<30, false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 4)
	if _, err := f.ReadAt(b, 1<<30); err != nil || string(b) != "kept" {
		t.Errorf("read back %q, %v", b, err)
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Errorf("left %s.new: %v", path, err)
	}
}
