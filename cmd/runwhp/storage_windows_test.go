//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/ext4"
	"github.com/quasidock/runwhp/internal/storefile"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"1g": 1 << 30, "1.5G": 1536 << 20, "64k": 64 << 10, "3t": 3 << 40, "100b": 100, "4096": 4096, " 2m ": 2 << 20,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "g", "x", "-1g", "0", "1q", "1e30t"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}

func TestParseStorageOpts(t *testing.T) {
	for _, c := range []struct {
		opts []string
		want int64
	}{
		{nil, 64 << 30},
		{[]string{"size=1g"}, 1 << 30},
		{[]string{"size=20G"}, 20 << 30},
		{[]string{"SIZE=1.5g"}, 1536 << 20},
		{[]string{"size=1g", "size=2g"}, 2 << 30},
		{[]string{"size=16t"}, ext4.MaxSize},
	} {
		got, err := parseStorageOpts(c.opts)
		if err != nil || got != c.want {
			t.Errorf("parseStorageOpts(%q) = %d, %v; want %d", c.opts, got, err, c.want)
		}
	}
	for _, c := range []struct {
		opts []string
		msg  string
	}{
		{[]string{"size=512m"}, "out of range"},
		{[]string{"size=17t"}, "out of range"},
		{[]string{"size=big"}, "invalid size"},
		{[]string{"size"}, "invalid size"},
		{[]string{"dm.basesize=10g"}, "unknown option"},
	} {
		if _, err := parseStorageOpts(c.opts); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("parseStorageOpts(%q) = %v, want an error with %q", c.opts, err, c.msg)
		}
	}
}

func TestSweepStore(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"scratch", "tmp", "images"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	left := filepath.Join(root, "scratch", "left.ext4")
	os.WriteFile(left, nil, 0o644)
	busy, err := disk.Create(filepath.Join(root, "scratch", "busy.ext4"), 1<<30, disk.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	old := filepath.Join(root, "tmp", "old")
	young := filepath.Join(root, "tmp", "young")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(young, nil, 0o644)
	os.Chtimes(old, time.Now().Add(-2*staleTemp), time.Now().Add(-2*staleTemp))
	// Image records a pull stopped writing go once they are old; records
	// themselves stay, however old.
	oldRecord := filepath.Join(root, "images", "abc.json.tmp") // as iteration 11 named them
	youngRecord := filepath.Join(root, "images", "def.json"+storefile.TempSuffix+"0a1b2c")
	oldDone := filepath.Join(root, "images", "ghi.json")
	for _, p := range []string{oldRecord, youngRecord, oldDone} {
		os.WriteFile(p, nil, 0o644)
	}
	os.Chtimes(oldRecord, time.Now().Add(-2*staleTemp), time.Now().Add(-2*staleTemp))
	os.Chtimes(oldDone, time.Now().Add(-2*staleTemp), time.Now().Add(-2*staleTemp))

	var logged []string
	sweepStore(root, func(f string, a ...any) { logged = append(logged, f) })
	for path, want := range map[string]bool{left: false, busy.Name(): true, old: false, young: true, oldRecord: false, youngRecord: true, oldDone: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
	if len(logged) != 1 {
		t.Errorf("logged %q, want one removed scratch disk", logged)
	}
	sweepStore(filepath.Join(root, "missing"), t.Logf) // a new store has neither directory
}
