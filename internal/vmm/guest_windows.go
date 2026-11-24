//go:build windows

package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
)

// guestBoot is what every guest boots from, a container's or one runwhp
// boots for itself: a kernel, and an initramfs of init, its spec and any
// modules.
type guestBoot struct {
	kernel  *boot.Kernel
	init    []byte
	modules []boot.File
}

// loadGuest reads the kernel a guest boots, runwhp's own unless kernelPath
// names another, init, and the modules in modulePaths.
func loadGuest(ctx context.Context, root, kernelPath string, modulePaths []string, initPath string, policy image.Policy) (*guestBoot, error) {
	kernelPath, err := ResolveKernel(ctx, root, kernelPath, policy)
	if err != nil {
		return nil, err
	}
	kernelImage, err := os.ReadFile(kernelPath)
	if err != nil {
		return nil, fmt.Errorf("reading the kernel: %w", err)
	}
	bzImage, err := boot.ParseKernel(kernelImage)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kernelPath, err)
	}
	modules, err := readModules(modulePaths)
	if err != nil {
		return nil, err
	}
	initBinary, err := readInit(initPath)
	if err != nil {
		return nil, err
	}
	return &guestBoot{kernel: bzImage, init: initBinary, modules: modules}, nil
}

// initramfs is the guest's initramfs, which gives init spec.
func (g *guestBoot) initramfs(spec *guestspec.Spec) ([]byte, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	files := []boot.File{
		{Name: "dev", Mode: boot.ModeDir | 0o755},
		{Name: "proc", Mode: boot.ModeDir | 0o755},
		{Name: "sys", Mode: boot.ModeDir | 0o755},
		{Name: "init", Mode: boot.ModeFile | 0o755, Data: g.init},
		{Name: "runwhp", Mode: boot.ModeDir | 0o755},
		{Name: guestspec.SpecPath, Mode: boot.ModeFile | 0o644, Data: specJSON},
	}
	return boot.CPIO(append(files, g.modules...)), nil
}

// guestCmdline is the kernel's command line: quiet unless debug.
func guestCmdline(debug bool) string {
	if debug {
		return baseCmdline + " loglevel=7"
	}
	return baseCmdline + " quiet loglevel=3"
}
