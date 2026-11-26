//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestInitLayer(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, to give files owners: run it in a container")
	}
	image := t.TempDir()
	old := root
	root = image
	t.Cleanup(func() { root = old })

	// The image has /proc and an /etc of its own, with /etc/hosts, but no
	// /sys, /dev, /etc/hostname or /etc/resolv.conf.
	os.Mkdir(filepath.Join(image, "proc"), 0o555)
	etc := filepath.Join(image, "etc")
	os.Mkdir(etc, 0o700)
	os.WriteFile(filepath.Join(etc, "hosts"), nil, 0o644)
	os.Lchown(etc, 5, 6)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(etc, when, when)

	spec := &guestspec.Spec{Hostname: "h", Resolv: &guestspec.Resolv{}}
	plan, err := planInitLayer(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range plan {
		got = append(got, e.rel)
	}
	if want := []string{"sys", "dev", "etc/hostname", "etc/resolv.conf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planned %q, want %q", got, want)
	}

	layer := t.TempDir()
	if err := makeInitLayer(layer, plan); err != nil {
		t.Fatal(err)
	}
	for _, e := range plan {
		fi, err := os.Lstat(filepath.Join(layer, e.rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.IsDir() != e.dir {
			t.Errorf("%s: directory %v, want %v", e.rel, fi.IsDir(), e.dir)
		}
	}
	fi, err := os.Lstat(filepath.Join(layer, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o700 || st.Uid != 5 || st.Gid != 6 || !fi.ModTime().Equal(when) {
		t.Errorf("etc is %v %d:%d %v, want the image's: drwx------ 5:6 %v", fi.Mode(), st.Uid, st.Gid, fi.ModTime(), when)
	}

	// Mounted over, or not written: no entry.
	spec = &guestspec.Spec{Mounts: []guestspec.Mount{{Kind: guestspec.MountVolume, Target: "/etc/hostname"}}, Hostname: "h"}
	plan, err = planInitLayer(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan {
		if e.rel == "etc/hostname" || e.rel == "etc/resolv.conf" {
			t.Errorf("planned %s", e.rel)
		}
	}
}
