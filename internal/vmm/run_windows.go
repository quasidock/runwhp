//go:build windows

package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/vm"
)

const (
	// pci=off: ACPI needs PCI support built in, but there is no PCI bus to
	// probe. no5lvl: on CPUs with 5-level paging, the bzImage decompressor
	// would otherwise switch paging modes through a trampoline in low
	// memory, which runwhp's boot layout has not been tried with.
	// swiotlb=noforce: with RAM above 4 GiB, the kernel would otherwise
	// set aside and zero 64 MiB of bounce buffers for devices that cannot
	// reach it, and no runwhp device needs them.
	// page_reporting_order=9: report free RAM to the balloon in blocks of
	// 2 MiB, the unit runwhp gives back, rather than the default 4 MiB.
	baseCmdline = "console=ttyS0 nokaslr no5lvl pci=off swiotlb=noforce page_reporting.page_reporting_order=9 panic=-1 reboot=acpi"
	consoleTail = 200 // kernel console lines kept for error reports
)

// CheckDevices checks that the container's devices fit the I/O APIC: each
// takes an interrupt line.
func CheckDevices(cfg *container.Config, volumes int) error {
	// The console, the vsock device, the balloon and the writable layer,
	// and the others named here.
	others, users := 4, []string{}
	switch {
	case len(cfg.Networks) > 1:
		others += len(cfg.Networks)
		users = append(users, fmt.Sprintf("%d networks", len(cfg.Networks)))
	case cfg.Network:
		others++
		users = append(users, "the network")
	}
	consistent, delegated := false, false
	for _, s := range cfg.Shares {
		consistent, delegated = consistent || !s.Delegated, delegated || s.Delegated
	}
	if consistent {
		others++
		users = append(users, "bind mounts")
	}
	if delegated {
		// Delegated bind mounts have a file system of their own.
		others++
		users = append(users, "delegated bind mounts")
	}
	if volumes > 0 {
		others += volumes
		users = append(users, fmt.Sprintf("%d volumes", volumes))
	}
	layers := max(len(cfg.Layers), len(cfg.LayerFiles))
	if n := others + layers; n > vm.MaxDevices {
		what := fmt.Sprintf("the image's %d layers", layers)
		if len(users) > 0 {
			what += ", " + strings.Join(users, ", ")
		}
		return fmt.Errorf("too many devices: %s, the console, the vsock device, the balloon and the writable layer make %d, and runwhp has room for %d", what, n, vm.MaxDevices)
	}
	return nil
}

// ExplicitKernel is the bzImage --kernel names, else the one RUNWHP_KERNEL
// names, else empty, which means runwhp's own kernel.
func ExplicitKernel(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("RUNWHP_KERNEL")
}

// ResolveKernel returns the bzImage to boot: path if it is not empty, else
// runwhp's own kernel from the store, fetched first if it is missing and the
// pull policy allows.
func ResolveKernel(ctx context.Context, root, path string, policy image.Policy) (string, error) {
	if path != "" {
		return path, nil
	}
	store := kernel.Open(root)
	store.Progress = os.Stderr
	path, err := store.Ensure(ctx, kernel.Default, policy != image.PullNever)
	if errors.Is(err, kernel.ErrNotPresent) {
		return "", fmt.Errorf("%w, and --pull=never was given; run 'runwhp pull IMAGE' once to fetch it, or pass --kernel PATH", err)
	}
	return path, err
}

// readModules reads the kernel modules given with --module, for booting a
// kernel that builds drivers runwhp needs as modules. WSL's kernel, for one,
// needs its virtio_blk.ko.
func readModules(paths []string) ([]boot.File, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	files := []boot.File{{Name: "modules", Mode: boot.ModeDir | 0o755}}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		files = append(files, boot.File{Name: "modules/" + filepath.Base(p), Mode: boot.ModeFile | 0o644, Data: data})
	}
	return files, nil
}

// DefaultMemory is half this machine's memory, in whole 2 MiB pages, and
// at least 512 MiB, as WSL sizes its VM.
func DefaultMemory() uint64 {
	return max(vm.MaxMemory()/2&^(2<<20-1), 512<<20)
}

// DefaultCPUs caps the default vCPU count. Build tools size their work by
// nproc, and four jobs still fit in the default 512m.
const DefaultCPUs = 4

// eventWriter reads the Events init sends on the control port, as JSON
// lines, and keeps how the container ended.
type eventWriter struct {
	mu     sync.Mutex
	buf    []byte
	status guestspec.Status
	ok     bool
	// onReady, if set, is called when a container booted to wait for its
	// start is ready; onStarted, when the container process starts, with
	// its pid in the guest; onStats and onTop, with the statistics or
	// processes init was asked for; onFreeze, with init's answer to a
	// freeze or thaw.
	// They are called with mu held, on the console's output goroutine, so
	// they must not block.
	onReady   func()
	onStarted func(pid int)
	onStats   func(*guestspec.GuestStats)
	onTop     func(*guestspec.Processes)
	onFreeze  func(*guestspec.Freeze)
}

func (w *eventWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		var ev guestspec.Event
		if err := json.Unmarshal(w.buf[:i], &ev); err == nil {
			switch {
			case ev.Exited:
				w.status, w.ok = guestspec.Status{Exit: ev.Exit, Error: ev.Error}, true
			case ev.Ready && w.onReady != nil:
				w.onReady()
			case ev.Started && w.onStarted != nil:
				w.onStarted(ev.Pid)
			case ev.Stats != nil && w.onStats != nil:
				w.onStats(ev.Stats)
			case ev.Top != nil && w.onTop != nil:
				w.onTop(ev.Top)
			case ev.Freeze != nil && w.onFreeze != nil:
				w.onFreeze(ev.Freeze)
			}
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// get returns how the container ended, once init has said.
func (w *eventWriter) get() (guestspec.Status, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.ok
}

// ring keeps the last lines of the kernel console.
type ring struct {
	max   int
	lines []string
}

func (r *ring) add(line string) {
	if len(r.lines) == r.max {
		r.lines = r.lines[1:]
	}
	r.lines = append(r.lines, line)
}

func (r *ring) print(w io.Writer) {
	if len(r.lines) == 0 {
		return
	}
	fmt.Fprintln(w, "runwhp: last kernel console lines:")
	for _, l := range r.lines {
		fmt.Fprintln(w, "  "+l)
	}
}
