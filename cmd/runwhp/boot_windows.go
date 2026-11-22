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

// boot runs a container's VM until it stops, and returns the container's
// exit code.
func bootContainer(ctx context.Context, root string, cfg *container.Config, policy image.Policy, proxy *signalProxy, logf func(string, ...any), start time.Time) (int, error) {
	const fail = guestspec.ExitRuntime

	// The scratch disk is made while the kernel is read.
	scratch := startScratch(root, cfg.StorageSize)
	defer scratch.close()

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

	sr := scratch.get()
	if sr.err != nil {
		return fail, sr.err
	}
	if sr.file != nil {
		logf("%4d ms  scratch disk ready: %d GiB, formatted in %d ms", time.Since(start).Milliseconds(), cfg.StorageSize>>30, sr.took.Milliseconds())
	}

	spec := guestspec.Spec{
		Args: cfg.Args, Env: cfg.Env, Cwd: cfg.Cwd, User: cfg.User,
		Hostname: cfg.Hostname, Hosts: cfg.Hosts, Layers: len(layerFiles), Scratch: sr.file != nil,
		Volumes: len(volumeDevices), Debug: cfg.Debug, Stdin: cfg.Stdin, TTY: cfg.TTY, Network: guestNetwork(cfg.Network),
		Mounts: cfg.Mounts,
	}
	if cfg.TTY {
		spec.Rows, spec.Cols, _ = terminalSize(os.Stdout)
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

	// Devices: the console first, then the network device and the shares'
	// file system if any, then one disk per layer, bottom first, then the
	// scratch disk, then the volumes.
	var stdin io.Reader
	if cfg.Stdin {
		stdin = os.Stdin
		if isTerminal(os.Stdin) {
			stdin = &consoleReader{h: windows.Handle(os.Stdin.Fd()), lines: !cfg.TTY}
		}
	}
	status := &statusWriter{}
	console := &virtio.Console{
		Ports: []virtio.Port{
			{Name: guestspec.PortStdout, Out: os.Stdout},
			{Name: guestspec.PortStderr, Out: os.Stderr},
			{Name: guestspec.PortControl, Out: status, In: proxy.queue},
			{Name: guestspec.PortStdin, In: stdin},
		},
		Logf: logf,
	}
	proxy.console.Store(console)
	devices := []virtio.Device{console}
	if cfg.Network {
		nic := &virtio.Net{MAC: netstack.GuestMAC, MTU: netstack.MTU}
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
	if sr.file != nil {
		// No FLUSH: the disk is thrown away after the run, so the guest
		// need never wait for the host's disk.
		devices = append(devices, &virtio.Blk{File: sr.file, Sectors: uint64(cfg.StorageSize) / 512, Discard: true,
			Serial: guestspec.ScratchSerial})
	}
	devices = append(devices, volumeDevices...)

	cmdline := baseCmdline + " quiet loglevel=3"
	if cfg.Debug {
		cmdline = baseCmdline + " loglevel=7"
	}
	tail := &ring{max: consoleTail}
	consoleLine := tail.add
	if cfg.Debug {
		consoleLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	}

	var term *consoleState
	if cfg.TTY {
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
		ConsoleLine: consoleLine,
		Logf:        logf,
	})
	term.restore() // before anything else is printed
	for _, s := range res.Stats {
		logf("%s", s)
	}
	if err != nil {
		tail.print(os.Stderr)
		return fail, err
	}
	if ctx.Err() != nil {
		return 130, ctx.Err()
	}
	st, ok := status.get()
	if !ok {
		tail.print(os.Stderr)
		return fail, fmt.Errorf("the guest stopped without reporting how the container ended (%s)", res.Reason)
	}
	if st.Error != "" {
		errorf("%s", st.Error)
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
