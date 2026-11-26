//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// initLayerDir holds the init layer: a tmpfs, stacked over the image's
// layers and under the writable one, holding the mount points init makes
// for itself that the image lacks. Docker keeps the same in a container's
// init layer: /proc, /sys and /dev, and the /etc files it binds over the
// image's. Mount points of the container's own mounts, and its working
// directory, go in the writable layer, as they do in Docker's, and show in
// its changes.
const initLayerDir = "/layers/init"

// initEntry is a path init makes in the init layer, with the directories
// above it as the image has them, so that the merged root shows them as
// it would without the layer.
type initEntry struct {
	rel     string // below the root, as in etc/resolv.conf
	dir     bool
	parents []initParent
}

type initParent struct {
	rel      string
	mode     fs.FileMode
	uid, gid int
	mtime    time.Time
	inImage  bool // false: made for the entry, 0755 root
}

// planInitLayer finds the mount points init will make that the image, with
// the writable layer over it, lacks.
func planInitLayer(spec *guestspec.Spec) ([]initEntry, error) {
	targets := []struct {
		p   string
		dir bool
	}{{"/proc", true}, {"/sys", true}, {"/dev", true}}
	for _, name := range initEtcFiles(spec) {
		targets = append(targets, struct {
			p   string
			dir bool
		}{"/etc/" + name, false})
	}
	if spec.HideMountPoints {
		for _, m := range spec.Mounts {
			targets = append(targets, struct {
				p   string
				dir bool
			}{path.Clean("/" + m.Target), !m.File})
		}
	}
	var plan []initEntry
	for _, t := range targets {
		p, err := resolveIn(t.p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.p, err)
		}
		if _, err := os.Lstat(p); err == nil || !os.IsNotExist(err) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		e := initEntry{rel: rel, dir: t.dir}
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			par := initParent{rel: dir, mode: fs.ModeDir | 0o755}
			if fi, err := os.Lstat(filepath.Join(root, dir)); err == nil && fi.IsDir() {
				st := fi.Sys().(*syscall.Stat_t)
				par.mode, par.uid, par.gid, par.mtime, par.inImage = fi.Mode(), int(st.Uid), int(st.Gid), fi.ModTime(), true
			} else if err == nil {
				return nil, fmt.Errorf("%s: /%s is not a directory", t.p, dir)
			}
			e.parents = append(e.parents, par)
		}
		plan = append(plan, e)
	}
	return plan, nil
}

// initEtcFiles are the files of etcDir prepareRoot will write and bindEtc
// bind, for spec.
func initEtcFiles(spec *guestspec.Spec) []string {
	var names []string
	for _, name := range etcFiles {
		var written bool
		switch name {
		case "hostname":
			written = spec.Hostname != ""
		case "hosts":
			written = spec.Hostname != "" || len(spec.Hosts) > 0
		case "resolv.conf":
			written = spec.Resolv != nil
		}
		if written && !mountedOver(spec, "/etc/"+name) {
			names = append(names, name)
		}
	}
	return names
}

// makeInitLayer makes the plan's entries below dir.
func makeInitLayer(dir string, plan []initEntry) error {
	var parents []initParent
	seen := map[string]bool{}
	for _, e := range plan {
		p := filepath.Join(dir, e.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if e.dir {
			if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		} else {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			f.Close()
		}
		for _, par := range e.parents {
			if !seen[par.rel] {
				seen[par.rel] = true
				parents = append(parents, par)
			}
		}
	}
	// Once every entry is made, since making one changes its parent's
	// times.
	for _, par := range parents {
		if !par.inImage {
			continue
		}
		p := filepath.Join(dir, par.rel)
		if err := os.Lchown(p, par.uid, par.gid); err != nil {
			return err
		}
		if err := os.Chmod(p, par.mode); err != nil {
			return err
		}
		if err := os.Chtimes(p, par.mtime, par.mtime); err != nil {
			return err
		}
	}
	return nil
}
