//go:build linux

package main

import (
	"os"
)

// layerRoots are the container's layers as init keeps them for diff and
// commit: the writable layer's upper directory, and the layers under it,
// top first, the init layer included. They are opened before enterRoot
// hides /rw and /layers, and every later access goes through them, so the
// container can neither reach nor move them.
var layerRoots struct {
	upper  *os.Root
	lowers []*os.Root
}

// openLayerRoots opens the writable layer's upper directory and the lower
// directories, top first, as overlayfs takes them. Without them, init
// still runs the container, but cannot report its changes.
func openLayerRoots(lower []string) {
	upper, err := os.OpenRoot("/rw/upper")
	if err != nil {
		logf("opening the writable layer: %v", err)
		return
	}
	lowers := make([]*os.Root, 0, len(lower))
	for _, dir := range lower {
		r, err := os.OpenRoot(dir)
		if err != nil {
			logf("opening %s: %v", dir, err)
			upper.Close()
			for _, r := range lowers {
				r.Close()
			}
			return
		}
		lowers = append(lowers, r)
	}
	layerRoots.upper, layerRoots.lowers = upper, lowers
}
