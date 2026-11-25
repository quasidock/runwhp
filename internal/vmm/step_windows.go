//go:build windows

package vmm

import (
	"context"

	"github.com/quasidock/runwhp/internal/container"
)

// Step is a guest booted for one step of a build, from the config of the
// state it builds on: no container record, lock or supervisor stands
// behind it.
type Step struct {
	Config *container.Config
	// Process boots the guest for Config's process, which Run starts;
	// else the guest serves its root's files until Close.
	Process bool
	// Layer is the guest's writable layer; nil gives it an empty one in
	// memory, for a guest that is only read from.
	Layer *StepLayer
	// IO takes the process's output.
	IO BootIO
}

// BootStep boots the guest s describes, and returns once init is ready to
// serve its files or to start its process.
func BootStep(ctx context.Context, root string, s Step) (*Guest, error) {
	layer := ReadyLayer(nil, false)
	if s.Layer != nil {
		layer = s.Layer.pending()
	}
	what := "the guest serving a build step's files"
	if s.Process {
		what = "the guest of a build step"
	}
	return bootGuest(ctx, root, s.Config, layer, s.Process, s.IO, what)
}
