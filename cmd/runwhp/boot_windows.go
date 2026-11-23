//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/hostfs"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
	"github.com/quasidock/runwhp/internal/volume"
)

// bootIO is where a container's streams go.
type bootIO struct {
	// stdin is the container's standard input, when its config asks for
	// one; nil gives it end of file at once.
	stdin          io.Reader
	stdout, stderr io.Writer
	// terminal says this process's console is the container's: with -t it
	// is put in raw mode and its size followed.
	terminal bool
	// diag takes runwhp's own diagnostics: the kernel console with
	// --debug, and its last lines when something fails.
	diag io.Writer
}

// foregroundIO is a foreground run's: this process's standard streams.
func foregroundIO(cfg *container.Config) bootIO {
	bio := bootIO{stdout: os.Stdout, stderr: os.Stderr, terminal: true, diag: os.Stderr}
	if cfg.Stdin {
		bio.stdin = os.Stdin
		if isTerminal(os.Stdin) {
			bio.stdin = &consoleReader{h: windows.Handle(os.Stdin.Fd()), lines: !cfg.TTY}
		}
	}
	return bio
}

// bootHooks tell bootContainer's caller how the container is doing.
type bootHooks struct {
	// waitStart boots the container without starting its process: ready
	// is called once it is prepared, and the process starts when the
	// caller sends a Control with Start.
	waitStart bool
	ready     func()
	// started, if set, is called when the container process has started,
	// with its pid in the guest.
	started func(pid int)
	// failed, if set, is given the last kernel console lines when the VM
	// failed, or stopped without saying how the container ended.
	failed func(console []string)
	// attached, if set, is given what reaches into the VM before it boots.
	attached func(*vmHandles)
}

// vmHandles are what the runwhp running a container reaches its VM
// through while it runs.
type vmHandles struct {
	vsock *virtio.Vsock
	nic   *virtio.Net // nil without a network
	blks  []*virtio.Blk
	probe *vm.Probe
	guest *guestStats
}

// Page cache reclaim: once the guest has been idle for reclaimIdle, init
// gives back some of its page cache every reclaimInterval.
const (
	reclaimIdle     = 60 * time.Second
	reclaimInterval = 5 * time.Second
)

// reclaimPolicy is when init reclaims the guest's page cache:
// RUNWHP_RECLAIM_IDLE, in seconds, overrides reclaimIdle, and 0 turns
// reclaiming off.
func reclaimPolicy(logf func(string, ...any)) guestspec.Reclaim {
	r := guestspec.Reclaim{IdleSeconds: int(reclaimIdle / time.Second), IntervalSeconds: int(reclaimInterval / time.Second)}
	if v, ok := os.LookupEnv("RUNWHP_RECLAIM_IDLE"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			logf("RUNWHP_RECLAIM_IDLE=%q is not a number of seconds; ignored", v)
			return r
		}
		r.IdleSeconds = n
		r.IntervalSeconds = min(r.IntervalSeconds, max(n, 1))
	}
	return r
}

// detachedRows and detachedCols size the terminal of a container run with
// -t but no console of runwhp's own.
const detachedRows, detachedCols = 24, 80

// bootContainer runs a container's VM until it stops, and returns the
// container's exit code. The caller holds the container's lock.
//
// layer is the writable layer, being opened; if nil, bootContainer opens
// the container's own, while it reads the kernel.
func bootContainer(ctx context.Context, root string, rec *container.Container, layer *pendingLayer, policy image.Policy, proxy *signalProxy, bio bootIO, hooks bootHooks, logf func(string, ...any), start time.Time) (int, error) {
	const fail = guestspec.ExitRuntime
	cfg := rec.Config

	if layer == nil {
		layer = startLayer(rec.RW(), cfg.StorageSize, cfg.AutoRemove)
		defer layer.close()
	}

	layerFiles, err := layerPaths(root, cfg)
	if err != nil {
		return fail, err
	}

	// Volumes, opened exclusively: a volume is in one container at a time.
	var volumeDevices []virtio.Device
	volumes := volume.Open(root)
	for i, u := range cfg.Volumes {
		v, _, err := volumes.Ensure(u.Name, u.Size)
		if err != nil {
			return fail, err
		}
		f, err := disk.Open(v.Disk(), disk.Exclusive)
		if errors.Is(err, disk.ErrInUse) {
			return fail, fmt.Errorf("volume %s is in use by another container", v.Name)
		}
		if err != nil {
			return fail, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		defer f.Close()
		fsize, err := f.Size()
		if err != nil {
			return fail, err
		}
		volumeDevices = append(volumeDevices, &virtio.Blk{File: f, Sectors: uint64(fsize) / 512, Flush: true, Discard: true,
			Serial: guestspec.VolumeSerial(i)})
	}

	kernelPath, err := resolveKernel(ctx, root, cfg.Kernel, policy)
	if err != nil {
		return fail, err
	}
	kernelImage, err := os.ReadFile(kernelPath)
	if err != nil {
		return fail, fmt.Errorf("reading the kernel: %w", err)
	}
	bzImage, err := boot.ParseKernel(kernelImage)
	if err != nil {
		return fail, fmt.Errorf("%s: %w", kernelPath, err)
	}
	modules, err := readModules(cfg.Modules)
	if err != nil {
		return fail, err
	}
	initBinary, err := readInit(cfg.Init)
	if err != nil {
		return fail, err
	}

	lr := layer.get()
	if lr.err != nil {
		return fail, lr.err
	}
	keep := lr.file != nil && !cfg.AutoRemove
	if lr.file != nil {
		logf("%4d ms  writable layer ready: %d GiB, kept %v, opened in %d ms", time.Since(start).Milliseconds(), cfg.StorageSize>>30, keep, lr.took.Milliseconds())
	}

	spec := guestspec.Spec{
		Args: cfg.Args, Env: cfg.Env, Cwd: cfg.Cwd, User: cfg.User,
		Hostname: cfg.Hostname, Hosts: cfg.Hosts, Layers: len(layerFiles), Scratch: lr.file != nil, KeepLayer: keep,
		Volumes: len(volumeDevices), Debug: cfg.Debug, Stdin: cfg.Stdin, TTY: cfg.TTY, Network: guestNetwork(cfg.Network),
		Mounts: cfg.Mounts, WaitStart: hooks.waitStart, Reclaim: reclaimPolicy(logf),
	}
	if cfg.TTY {
		if bio.terminal {
			spec.Rows, spec.Cols, _ = terminalSize(os.Stdout)
		} else {
			spec.Rows, spec.Cols = detachedRows, detachedCols
		}
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return fail, err
	}
	files := []boot.File{
		{Name: "dev", Mode: boot.ModeDir | 0o755},
		{Name: "proc", Mode: boot.ModeDir | 0o755},
		{Name: "sys", Mode: boot.ModeDir | 0o755},
		{Name: "init", Mode: boot.ModeFile | 0o755, Data: initBinary},
		{Name: "runwhp", Mode: boot.ModeDir | 0o755},
		{Name: guestspec.SpecPath, Mode: boot.ModeFile | 0o644, Data: specJSON},
	}
	files = append(files, modules...)

	// Devices: the console, the vsock device and the balloon first, then
	// the network device and the shares' file system if any, then one disk
	// per layer, bottom first, then the writable layer, then the volumes.
	var stdin io.Reader
	if cfg.Stdin {
		stdin = bio.stdin
		if stdin == nil {
			stdin = strings.NewReader("")
		}
	}
	guest := newGuestStats(proxy)
	status := &eventWriter{onReady: hooks.ready, onStarted: hooks.started, onStats: guest.deliver}
	console := &virtio.Console{
		Ports: []virtio.Port{
			{Name: guestspec.PortStdout, Out: bio.stdout},
			{Name: guestspec.PortStderr, Out: bio.stderr},
			{Name: guestspec.PortControl, Out: status, In: proxy.queue},
			{Name: guestspec.PortStdin, In: stdin},
		},
		Logf: logf,
	}
	proxy.console.Store(console)
	// The vsock device carries runwhp exec's connections to init.
	vsock := &virtio.Vsock{CID: guestspec.GuestCID, Logf: logf}
	defer func() { logf("%s", vsock.Stats()) }()
	handles := &vmHandles{vsock: vsock, probe: &vm.Probe{}, guest: guest}
	// The balloon gives back to Windows the memory the guest reports free.
	devices := []virtio.Device{console, vsock, &virtio.Balloon{}}
	if cfg.Network {
		nic := &virtio.Net{MAC: netstack.GuestMAC, MTU: netstack.MTU}
		handles.nic = nic
		stack, err := netstack.New(netstack.Config{
			Output:      nic.Deliver,
			HostGateway: cfg.HostGateway,
			Publish:     cfg.Publish,
			Logf:        logf,
		})
		if err != nil {
			return fail, err
		}
		defer func() {
			logf("%s", stack.Stats())
			stack.Close()
		}()
		nic.Recv = stack.Inject
		devices = append(devices, nic)
	}
	if len(cfg.Shares) > 0 {
		shares, err := hostShares(cfg.Shares)
		if err != nil {
			return fail, err
		}
		fsys, err := hostfs.New(shares, logf)
		if err != nil {
			return fail, err
		}
		defer fsys.Close()
		server := &fuse.Server{FS: fsys, Logf: logf}
		defer func() { logf("%s", server.Stats()) }()
		devices = append(devices, &virtio.Fs{Tag: guestspec.FSTag, Handler: server})
	}
	for i, path := range layerFiles {
		f, err := disk.Open(path, disk.ReadOnly)
		if err != nil {
			return fail, err
		}
		defer f.Close()
		size, err := f.Size()
		if err != nil {
			return fail, err
		}
		devices = append(devices, &virtio.Blk{File: f, Sectors: uint64(size) / 512, ReadOnly: true,
			Serial: guestspec.LayerSerial(i)})
	}
	if lr.file != nil {
		// A kept layer offers FLUSH, so fsync in the container reaches the
		// host's disk, as it would under Docker. A scratch disk does not:
		// it is thrown away after the run, so the guest need never wait for
		// the host's disk.
		devices = append(devices, &virtio.Blk{File: lr.file, Sectors: uint64(cfg.StorageSize) / 512, Discard: true, Flush: keep,
			Serial: guestspec.ScratchSerial})
	}
	devices = append(devices, volumeDevices...)
	for _, d := range devices {
		if b, ok := d.(*virtio.Blk); ok {
			handles.blks = append(handles.blks, b)
		}
	}
	if hooks.attached != nil {
		hooks.attached(handles)
	}

	cmdline := baseCmdline + " quiet loglevel=3"
	if cfg.Debug {
		cmdline = baseCmdline + " loglevel=7"
	}
	tail := &ring{max: consoleTail}
	consoleLine := tail.add
	if cfg.Debug {
		consoleLine = func(line string) { fmt.Fprintln(bio.diag, line) }
	}

	var term *consoleState
	if cfg.TTY && bio.terminal {
		term, err = setupConsole(cfg.Stdin, isTerminal(os.Stdout))
		if err != nil {
			return fail, fmt.Errorf("setting up the console: %w", err)
		}
		defer term.restore()
		done := make(chan struct{})
		defer close(done)
		go watchSize(done, spec.Rows, spec.Cols, proxy.queue)
	}
	res, err := vm.Run(ctx, vm.Config{
		Kernel:      bzImage,
		Initrd:      boot.CPIO(files),
		Cmdline:     cmdline,
		Memory:      cfg.Memory,
		CPUs:        cfg.CPUs,
		Devices:     devices,
		Probe:       handles.probe,
		ConsoleLine: consoleLine,
		Logf:        logf,
	})
	term.restore() // before anything else is printed
	for _, s := range res.Stats {
		logf("%s", s)
	}
	failed := func() {
		tail.print(bio.diag)
		if hooks.failed != nil {
			hooks.failed(tail.lines)
		}
	}
	if err != nil {
		failed()
		return fail, err
	}
	if ctx.Err() != nil {
		return 130, ctx.Err()
	}
	st, ok := status.get()
	if !ok {
		failed()
		return fail, fmt.Errorf("the guest stopped without reporting how the container ended (%s)", res.Reason)
	}
	if st.Error != "" {
		return st.Exit, errors.New(st.Error)
	}
	return st.Exit, nil
}

// layerPaths returns the ext4 files of a container's layers, bottom first.
func layerPaths(root string, cfg *container.Config) ([]string, error) {
	if len(cfg.LayerFiles) > 0 {
		return cfg.LayerFiles, nil
	}
	store := image.Open(root)
	var paths []string
	for _, d := range cfg.Layers {
		p, err := store.Layers.Path(d)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("layer %s of %s is missing from the store; pull the image again", d, cfg.Image)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// readInit reads runwhp-init: the one path names, for development, else the
// one next to runwhp.exe.
func readInit(path string) ([]byte, error) {
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(filepath.Dir(exe), "runwhp-init")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the guest init program: %w", err)
	}
	return data, nil
}

// hostShares returns the shares for hostfs, first making again any missing
// directory -v made when the container was created.
func hostShares(shares []container.Share) ([]hostfs.Share, error) {
	out := make([]hostfs.Share, len(shares))
	for i, s := range shares {
		if s.Create && !s.File {
			if _, err := os.Stat(s.Path); os.IsNotExist(err) {
				if err := os.MkdirAll(s.Path, 0o755); err != nil {
					return nil, err
				}
			}
		}
		out[i] = hostfs.Share{Path: s.Path, File: s.File, ReadOnly: s.ReadOnly, Target: s.Target}
	}
	return out, nil
}
