//go:build windows

package vmm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

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

// BootIO is where a container's streams go.
type BootIO struct {
	// Stdin is the container's standard input, when its config asks for
	// one; nil gives it end of file at once.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Terminal says this process's console is the container's: with -t it
	// is put in raw mode and its size followed.
	Terminal bool
	// Diag takes runwhp's own diagnostics: the kernel console with
	// --debug, and its last lines when something fails.
	Diag io.Writer
}

// ForegroundIO is a foreground run's: this process's standard streams.
func ForegroundIO(cfg *container.Config) BootIO {
	bio := BootIO{Stdout: os.Stdout, Stderr: os.Stderr, Terminal: true, Diag: os.Stderr}
	if cfg.Stdin {
		bio.Stdin = os.Stdin
		if IsTerminal(os.Stdin) {
			bio.Stdin = &ConsoleReader{h: windows.Handle(os.Stdin.Fd()), lines: !cfg.TTY}
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
	vsock   *virtio.Vsock
	nic     *virtio.Net // nil without a network
	blks    []*virtio.Blk
	probe   *vm.Probe
	guest   *guestStats
	freezer *guestFreezer
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

// clockSync is how often the host sends the guest its time, besides once
// as init starts listening and whenever Windows resumes from sleep.
const clockSync = 60 * time.Second

// clockPeriod is how often the guest's clock is set from the host's:
// RUNWHP_CLOCK_SYNC, in seconds, overrides clockSync, and 0 turns setting
// it off altogether, leaving the guest with the clock it booted with.
func clockPeriod(logf func(string, ...any)) time.Duration {
	v, ok := os.LookupEnv("RUNWHP_CLOCK_SYNC")
	if !ok {
		return clockSync
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		logf("RUNWHP_CLOCK_SYNC=%q is not a number of seconds; ignored", v)
		return clockSync
	}
	return time.Duration(n) * time.Second
}

// DetachedRows and DetachedCols size the terminal of a container run with
// -t but no console of runwhp's own.
const DetachedRows, DetachedCols = 24, 80

// guestOpts shape a guest booted from a container's config for something
// other than running its process.
type guestOpts struct {
	// serve, if set, boots the container's root with no process: init
	// serves it as guestspec.Spec.Serve says until the host sends
	// Shutdown.
	serve string
	// noNIC leaves the network device out, though init names the network
	// as the config says; noBalloon leaves the balloon out.
	noNIC, noBalloon bool
	// memory and cpus, if set, size the guest instead of the config.
	memory uint64
	cpus   int
}

// bootContainer runs the VM of a container, configured by cfg, until it
// stops, and returns the container's exit code. layer is its writable
// layer, being opened; the caller closes it, and holds whatever lock the
// container's storage needs.
func bootContainer(ctx context.Context, root string, cfg *container.Config, layer *PendingLayer, opts guestOpts, policy image.Policy, proxy *SignalProxy, bio BootIO, hooks bootHooks, logf func(string, ...any), start time.Time) (int, error) {
	const fail = guestspec.ExitRuntime

	storage, err := openStorage(root, cfg, logf)
	if err != nil {
		return fail, err
	}
	defer storage.close()

	guestFiles, err := loadGuest(ctx, root, cfg.Kernel, cfg.Modules, cfg.Init, policy)
	if err != nil {
		return fail, err
	}

	lr := layer.get()
	if lr.err != nil {
		return fail, lr.err
	}
	keep := lr.file != nil && !layer.scratch
	var layerSize int64
	if lr.file != nil {
		if layerSize, err = lr.file.Size(); err != nil {
			return fail, err
		}
		logf("%4d ms  writable layer ready: %d GiB, kept %v, opened in %d ms", time.Since(start).Milliseconds(), layerSize>>30, keep, lr.took.Milliseconds())
	}

	spec := guestspec.Spec{
		Args: cfg.Args, Env: cfg.Env, Cwd: cfg.Cwd, User: cfg.User,
		Hostname: cfg.Hostname, Hosts: cfg.Hosts, Layers: len(storage.layers), Scratch: lr.file != nil, KeepLayer: keep,
		Volumes: len(storage.volumes), Debug: cfg.Debug, Stdin: cfg.Stdin, TTY: cfg.TTY, Network: GuestNetwork(cfg.Network),
		Mounts: cfg.Mounts, WaitStart: hooks.waitStart, Serve: opts.serve, HideMountPoints: cfg.HideMountPoints,
	}
	if opts.serve == "" {
		spec.Reclaim = reclaimPolicy(logf)
		spec.ClockSync = clockPeriod(logf) > 0
	}
	if cfg.TTY {
		if bio.Terminal {
			spec.Rows, spec.Cols, _ = TerminalSize(os.Stdout)
		} else {
			spec.Rows, spec.Cols = DetachedRows, DetachedCols
		}
	}
	initrd, err := guestFiles.initramfs(&spec)
	if err != nil {
		return fail, err
	}

	// Devices: the console, the vsock device and the balloon first, then
	// the network device and the shares' file systems if any, the delegated
	// shares' second, then one disk per layer, bottom first, then the
	// writable layer, then the volumes.
	var stdin io.Reader
	if cfg.Stdin {
		stdin = bio.Stdin
		if stdin == nil {
			stdin = strings.NewReader("")
		}
	}
	stdout, stderr := bio.Stdout, bio.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	guest := newGuestStats(proxy)
	freezer := newGuestFreezer(proxy)
	status := &eventWriter{onReady: hooks.ready, onStarted: hooks.started, onStats: guest.deliver, onFreeze: freezer.deliver}
	console := &virtio.Console{
		Ports: []virtio.Port{
			{Name: guestspec.PortStdout, Out: stdout},
			{Name: guestspec.PortStderr, Out: stderr},
			{Name: guestspec.PortControl, Out: status, In: proxy.queue},
			{Name: guestspec.PortStdin, In: stdin},
		},
		Logf: logf,
	}
	proxy.console.Store(console)
	// The vsock device carries runwhp exec's connections to init.
	vsock := &virtio.Vsock{CID: guestspec.GuestCID, Logf: logf}
	defer func() { logf("%s", vsock.Stats()) }()
	handles := &vmHandles{vsock: vsock, probe: &vm.Probe{}, guest: guest, freezer: freezer}
	// The balloon gives back to Windows the memory the guest reports free.
	devices := []virtio.Device{console, vsock}
	if !opts.noBalloon {
		devices = append(devices, &virtio.Balloon{})
	}
	if cfg.Network && !opts.noNIC {
		nic := &virtio.Net{MAC: netstack.GuestMAC, MTU: netstack.MTU}
		handles.nic = nic
		stack, err := netstack.New(netstack.Config{
			Output:         nic.Deliver,
			GuestChecksums: nic.GuestChecksums,
			HostGateway:    cfg.HostGateway,
			Publish:        cfg.Publish,
			Logf:           logf,
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
	if lr.file != nil {
		// A kept layer offers FLUSH, so fsync in the container reaches the
		// host's disk, as it would under Docker. A scratch disk does not:
		// it is thrown away after the run, so the guest need never wait for
		// the host's disk.
		storage.setScratch(lr.file, layerSize, keep)
	}
	devices = append(devices, storage.devices()...)
	for _, d := range devices {
		if b, ok := d.(*virtio.Blk); ok {
			handles.blks = append(handles.blks, b)
		}
	}
	if len(devices) > vm.MaxDevices {
		return fail, fmt.Errorf("%d devices are more than a guest can take", len(devices))
	}
	if hooks.attached != nil {
		hooks.attached(handles)
	}

	tail := &ring{max: consoleTail}
	consoleLine := tail.add
	if cfg.Debug {
		consoleLine = func(line string) { fmt.Fprintln(bio.Diag, line) }
	}

	var term *ConsoleState
	if cfg.TTY && bio.Terminal {
		term, err = SetupConsole(cfg.Stdin, IsTerminal(os.Stdout))
		if err != nil {
			return fail, fmt.Errorf("setting up the console: %w", err)
		}
		defer term.Restore()
		done := make(chan struct{})
		defer close(done)
		go watchSize(done, spec.Rows, spec.Cols, proxy.queue)
	}
	res, err := vm.Run(ctx, vm.Config{
		Kernel:      guestFiles.kernel,
		Initrd:      initrd,
		Cmdline:     guestCmdline(cfg.Debug),
		Memory:      cmp.Or(opts.memory, cfg.Memory),
		CPUs:        cmp.Or(opts.cpus, cfg.CPUs),
		Devices:     devices,
		Probe:       handles.probe,
		ConsoleLine: consoleLine,
		Logf:        logf,
	})
	term.Restore() // before anything else is printed
	for _, s := range res.Stats {
		logf("%s", s)
	}
	failed := func() {
		tail.print(bio.Diag)
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

// containerStorage is what a container's guest stores its files on, as
// devices, in the order it numbers them: the shares' file systems, the
// delegated shares' second, one disk per layer, bottom first, the writable
// layer, and the volumes.
type containerStorage struct {
	fs      []virtio.Device
	layers  []virtio.Device
	scratch virtio.Device
	volumes []virtio.Device
	closers []func()
}

// openStorage opens a container's storage but its writable layer: its
// layers, read-only, its volumes, exclusively, as a volume is in one
// container at a time, and its shares' file systems.
func openStorage(root string, cfg *container.Config, logf func(string, ...any)) (_ *containerStorage, err error) {
	s := &containerStorage{}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	layerFiles, err := layerPaths(root, cfg)
	if err != nil {
		return nil, err
	}
	for i, u := range cfg.Volumes {
		v, _, err := volume.Open(cmp.Or(u.Root, root)).Ensure(u.Name, u.Size)
		if err != nil {
			return nil, err
		}
		f, err := disk.Open(v.Disk(), disk.Exclusive)
		if errors.Is(err, disk.ErrInUse) {
			return nil, fmt.Errorf("volume %s is in use by another container", v.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		s.closers = append(s.closers, func() { f.Close() })
		fsize, err := f.Size()
		if err != nil {
			return nil, err
		}
		s.volumes = append(s.volumes, &virtio.Blk{File: f, Sectors: uint64(fsize) / 512, Flush: true, Discard: true,
			Serial: guestspec.VolumeSerial(i)})
	}
	// Delegated shares get a file system of their own, since a FUSE
	// connection takes the writeback cache, or not, as a whole.
	for _, delegated := range []bool{false, true} {
		var group []container.Share
		for _, sh := range cfg.Shares {
			if sh.Delegated == delegated {
				group = append(group, sh)
			}
		}
		if len(group) == 0 {
			continue
		}
		shares, err := hostShares(group)
		if err != nil {
			return nil, err
		}
		fsys, err := hostfs.New(shares, logf)
		if err != nil {
			return nil, err
		}
		fsys.Writeback = delegated
		server := &fuse.Server{FS: fsys, Logf: logf, Writeback: delegated}
		s.closers = append(s.closers, func() {
			logf("%s", server.Stats())
			fsys.Close()
		})
		tag := guestspec.FSTag
		if delegated {
			tag = guestspec.FSTagDelegated
		}
		s.fs = append(s.fs, &virtio.Fs{Tag: tag, Handler: server})
	}
	for i, path := range layerFiles {
		f, err := disk.Open(path, disk.ReadOnly)
		if err != nil {
			return nil, err
		}
		s.closers = append(s.closers, func() { f.Close() })
		size, err := f.Size()
		if err != nil {
			return nil, err
		}
		s.layers = append(s.layers, &virtio.Blk{File: f, Sectors: uint64(size) / 512, ReadOnly: true,
			Serial: guestspec.LayerSerial(i)})
	}
	return s, nil
}

// setScratch adds the writable layer, f, of size bytes; flush offers the
// guest FLUSH.
func (s *containerStorage) setScratch(f *disk.File, size int64, flush bool) {
	s.scratch = &virtio.Blk{File: f, Sectors: uint64(size) / 512, Discard: true, Flush: flush, Serial: guestspec.ScratchSerial}
}

// devices lists the storage's devices in the guest's order.
func (s *containerStorage) devices() []virtio.Device {
	out := append(append([]virtio.Device(nil), s.fs...), s.layers...)
	if s.scratch != nil {
		out = append(out, s.scratch)
	}
	return append(out, s.volumes...)
}

// close lets go of what openStorage opened, last first; not the writable
// layer.
func (s *containerStorage) close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
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
