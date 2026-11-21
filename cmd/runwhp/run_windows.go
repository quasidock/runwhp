//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/hostfs"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
	"github.com/quasidock/runwhp/internal/volume"
)

const (
	// pci=off: ACPI needs PCI support built in, but there is no PCI bus to
	// probe. no5lvl: on CPUs with 5-level paging, the bzImage decompressor
	// would otherwise switch paging modes through a trampoline in low
	// memory, which runwhp's boot layout has not been tried with.
	baseCmdline = "console=ttyS0 nokaslr no5lvl pci=off panic=-1 reboot=acpi"
	consoleTail = 200 // kernel console lines kept for error reports
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

type runOptions struct {
	interactive bool
	tty         bool
	entrypoint  string
	env         listFlag
	workdir     string
	user        string
	memory      string
	cpus        string
	hostname    string
	pull        string
	kernel      string
	debug       bool
	network     string
	publish     listFlag
	addHosts    listFlag
	volumes     listFlag
	mounts      listFlag
	tmpfs       listFlag
	storageOpts listFlag

	// Development flags, left out of the usage text.
	init    string
	layers  listFlag
	modules listFlag
}

func runFlags(o *runOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.BoolVar(&o.interactive, "i", false, "")
	fs.BoolVar(&o.interactive, "interactive", false, "")
	fs.BoolVar(&o.tty, "t", false, "")
	fs.BoolVar(&o.tty, "tty", false, "")
	// The flag package does not combine short flags, and -it is how
	// everyone types it.
	both := func(string) error { o.interactive, o.tty = true, true; return nil }
	fs.BoolFunc("it", "", both)
	fs.BoolFunc("ti", "", both)
	fs.StringVar(&o.entrypoint, "entrypoint", "", "")
	fs.Var(&o.env, "e", "")
	fs.Var(&o.env, "env", "")
	fs.StringVar(&o.workdir, "w", "", "")
	fs.StringVar(&o.workdir, "workdir", "", "")
	fs.StringVar(&o.user, "u", "", "")
	fs.StringVar(&o.user, "user", "", "")
	fs.StringVar(&o.memory, "m", "512m", "")
	fs.StringVar(&o.memory, "memory", "512m", "")
	fs.StringVar(&o.cpus, "cpus", "", "")
	fs.StringVar(&o.hostname, "hostname", "", "")
	fs.StringVar(&o.pull, "pull", "missing", "")
	fs.StringVar(&o.kernel, "kernel", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.StringVar(&o.network, "network", "user", "")
	fs.StringVar(&o.network, "net", "user", "")
	fs.Var(&o.publish, "p", "")
	fs.Var(&o.publish, "publish", "")
	fs.Var(&o.addHosts, "add-host", "")
	fs.Var(&o.volumes, "v", "")
	fs.Var(&o.volumes, "volume", "")
	fs.Var(&o.mounts, "mount", "")
	fs.Var(&o.tmpfs, "tmpfs", "")
	fs.Var(&o.storageOpts, "storage-opt", "")
	fs.StringVar(&o.init, "init", "", "")
	fs.Var(&o.layers, "layer", "")
	fs.Var(&o.modules, "module", "")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: runwhp run [flags] IMAGE [COMMAND [ARG...]]

Flags:
  -i, --interactive      pass this program's standard input to the container
  -t, --tty              give the container a terminal; with -i, keys go to it raw
      --entrypoint CMD   override the image's entrypoint ("" removes it)
  -e, --env KEY[=VALUE]  set an environment variable (repeatable); KEY alone copies it from here
  -w, --workdir DIR      working directory inside the container
  -u, --user USER[:GROUP]  user to run as: name or uid, optionally with a group
  -m, --memory SIZE      guest memory, such as 512m or 2g (default 512m)
      --cpus N           virtual processors (default: this machine's, up to 4)
      --hostname NAME    container hostname (default: random)
      --network MODE     user (default: outbound access through this machine) or none
  -p, --publish [IP:]HOSTPORT:PORT  forward a host port to a container TCP port (repeatable);
                         IP defaults to 127.0.0.1
      --add-host NAME:IP add an /etc/hosts entry (repeatable); IP host-gateway reaches
                         this machine's 127.0.0.1
  -v, --volume HOSTPATH:PATH[:ro]  bind a host directory or file into the container (repeatable);
                         HOSTPATH is absolute or starts with a dot, and is created if missing
  -v, --volume NAME:PATH[:ro]  mount a named volume, created if missing; PATH alone makes an
                         anonymous volume, gone after the run
      --mount type=bind|volume|tmpfs,source=...,target=PATH[,readonly]  the same, Docker's long form
      --tmpfs PATH[:OPTIONS]  mount a tmpfs, by default noexec,nosuid,nodev (repeatable)
      --storage-opt size=SIZE  how large the container's writable layer may grow (default 64g)
      --pull POLICY      when to pull the image: missing, always or never (default missing);
                         never also keeps runwhp from fetching its own kernel
      --kernel PATH      Linux bzImage to boot instead of runwhp's own kernel
      --debug            show the kernel console and VMM diagnostics on stderr
`)
	}
	return fs
}

func cmdRun(args []string) int {
	var o runOptions
	fs := runFlags(&o)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return guestspec.ExitRuntime
	}
	entrypointSet := false
	fs.Visit(func(f *flag.Flag) { entrypointSet = entrypointSet || f.Name == "entrypoint" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy := &signalProxy{stop: cancel, queue: newControlQueue(), logf: func(string, ...any) {}}
	if o.debug {
		proxy.logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go proxy.run(sigs)
	code, err := run(ctx, &o, entrypointSet, fs.Args(), proxy)
	if err != nil {
		if ctx.Err() != nil {
			errorf("interrupted")
			return 130
		}
		errorf("%v", err)
	}
	return code
}

func run(ctx context.Context, o *runOptions, entrypointSet bool, args []string, proxy *signalProxy) (int, error) {
	const fail = guestspec.ExitRuntime
	if o.interactive && o.tty && !isTerminal(os.Stdin) {
		return fail, errors.New("the input device is not a TTY; if you are using mintty, try prefixing the command with 'winpty'")
	}
	memory, err := parseMemory(o.memory)
	if err != nil {
		return fail, err
	}
	cpus, err := parseCPUs(o.cpus, runtime.NumCPU())
	if err != nil {
		return fail, err
	}
	network, err := parseNetworkFlags(o.network, o.publish, o.addHosts)
	if err != nil {
		return fail, err
	}
	plan, err := parseMounts(o.volumes, o.mounts, o.tmpfs)
	if err != nil {
		return fail, err
	}
	scratchSize, err := parseStorageOpts(o.storageOpts)
	if err != nil {
		return fail, err
	}
	root, err := storeRoot()
	if err != nil {
		return fail, err
	}
	logf := func(format string, a ...any) {}
	if o.debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	policy, err := image.ParsePolicy(o.pull)
	if err != nil {
		return fail, err
	}
	start := time.Now()

	// The scratch disk is made while the image and kernel are read.
	sweepStore(root, logf)
	scratch := startScratch(root, scratchSize)
	defer scratch.close()

	// The process and its layers, from an image or (for development)
	// from ext4 files given with --layer.
	overrides := image.Overrides{
		Entrypoint: o.entrypoint, EntrypointSet: entrypointSet,
		Env: o.env, Workdir: o.workdir, User: o.user, LookupEnv: os.LookupEnv,
	}
	var config image.Process
	var layerFiles []string
	if len(o.layers) > 0 {
		overrides.Cmd = args
		if config, err = image.BuildProcess(v1.Config{}, overrides); err != nil {
			return fail, err
		}
		layerFiles = o.layers
	} else {
		if len(args) == 0 {
			return fail, fmt.Errorf("no image given; run 'runwhp run -h' for usage")
		}
		store := image.Open(root)
		store.Progress = os.Stderr
		img, err := store.Ensure(ctx, args[0], policy)
		if err != nil {
			return fail, err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return fail, err
		}
		overrides.Cmd = args[1:]
		if config, err = image.BuildProcess(cf.Config, overrides); err != nil {
			return fail, err
		}
		diffIDs, err := img.Layers()
		if err != nil {
			return fail, err
		}
		for _, d := range diffIDs {
			p, err := store.Layers.Path(d)
			if err != nil {
				return fail, err
			}
			layerFiles = append(layerFiles, p)
		}
	}
	// Each device takes an interrupt line: the console, the scratch disk,
	// and the others named here.
	others, users := 2, []string{}
	if network.enabled {
		others++
		users = append(users, "the network")
	}
	if len(plan.shares) > 0 {
		others++
		users = append(users, "bind mounts")
	}
	if n := len(plan.volumes); n > 0 {
		others += n
		users = append(users, fmt.Sprintf("%d volumes", n))
	}
	if n := others + len(layerFiles); n > vm.MaxDevices {
		what := fmt.Sprintf("the image's %d layers", len(layerFiles))
		if len(users) > 0 {
			what += ", " + strings.Join(users, ", ")
		}
		return fail, fmt.Errorf("too many devices: %s, the console and the scratch disk make %d, and runwhp has room for %d", what, n, vm.MaxDevices)
	}
	logf("%4d ms  image ready: %d layers", time.Since(start).Milliseconds(), len(layerFiles))

	// Volumes, opened exclusively: a volume is in one container at a time.
	var volumeDevices []virtio.Device
	volumes := volume.Open(root)
	for i, u := range plan.volumes {
		size := int64(defaultDiskSize)
		if u.size != "" {
			if size, err = parseDiskSize(u.size); err != nil {
				return fail, err
			}
		}
		v, created, err := volumes.Ensure(u.name, size)
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
		if created {
			logf("%4d ms  created volume %s", time.Since(start).Milliseconds(), v.Name)
		}
		volumeDevices = append(volumeDevices, &virtio.Blk{File: f, Sectors: uint64(fsize) / 512, Flush: true, Discard: true,
			Serial: guestspec.VolumeSerial(i)})
	}

	kernelPath, err := resolveKernel(ctx, root, o, policy)
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
	modules, err := readModules(o.modules)
	if err != nil {
		return fail, err
	}
	initPath := o.init
	if initPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fail, err
		}
		initPath = filepath.Join(filepath.Dir(exe), "runwhp-init")
	}
	initBinary, err := os.ReadFile(initPath)
	if err != nil {
		return fail, fmt.Errorf("reading the guest init program: %w", err)
	}

	sr := scratch.get()
	if sr.err != nil {
		return fail, sr.err
	}
	if sr.file != nil {
		logf("%4d ms  scratch disk ready: %d GiB, formatted in %d ms", time.Since(start).Milliseconds(), scratchSize>>30, sr.took.Milliseconds())
	}

	hostname := o.hostname
	if hostname == "" {
		hostname = randomHostname()
	}
	spec := guestspec.Spec{
		Args: config.Args, Env: config.Env, Cwd: config.Cwd, User: config.User,
		Hostname: hostname, Hosts: network.hosts, Layers: len(layerFiles), Scratch: sr.file != nil,
		Volumes: len(volumeDevices), Debug: o.debug, Stdin: o.interactive, TTY: o.tty, Network: network.spec(),
		Mounts: plan.mounts,
	}
	if o.tty {
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
	if o.interactive {
		stdin = os.Stdin
		if isTerminal(os.Stdin) {
			stdin = &consoleReader{h: windows.Handle(os.Stdin.Fd()), lines: !o.tty}
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
	if network.enabled {
		nic := &virtio.Net{MAC: netstack.GuestMAC, MTU: netstack.MTU}
		stack, err := netstack.New(netstack.Config{
			Output:      nic.Deliver,
			HostGateway: network.hostGateway,
			Publish:     network.publish,
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
	if len(plan.shares) > 0 {
		fsys, err := hostfs.New(plan.shares, logf)
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
		devices = append(devices, &virtio.Blk{File: sr.file, Sectors: uint64(scratchSize) / 512, Discard: true,
			Serial: guestspec.ScratchSerial})
	}
	devices = append(devices, volumeDevices...)

	cmdline := baseCmdline + " quiet loglevel=3"
	if o.debug {
		cmdline = baseCmdline + " loglevel=7"
	}
	tail := &ring{max: consoleTail}
	consoleLine := tail.add
	if o.debug {
		consoleLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	}

	var term *consoleState
	if o.tty {
		term, err = setupConsole(o.interactive, isTerminal(os.Stdout))
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
		Memory:      memory,
		CPUs:        cpus,
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

// resolveKernel returns the bzImage to boot: the one --kernel names, else the
// one RUNWHP_KERNEL names, else runwhp's own kernel from the store, fetched
// first if it is missing and the pull policy allows.
func resolveKernel(ctx context.Context, root string, o *runOptions, policy image.Policy) (string, error) {
	if o.kernel != "" {
		return o.kernel, nil
	}
	if p := os.Getenv("RUNWHP_KERNEL"); p != "" {
		return p, nil
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

// parseMemory reads a size the way docker run -m does (b, k, m or g suffix,
// bytes without one) and rounds it up to 2 MiB.
func parseMemory(s string) (uint64, error) {
	n, err := parseSize(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	size := (n + (2<<20 - 1)) &^ (2<<20 - 1)
	if size < 128<<20 || size > vm.MaxMemory {
		return 0, fmt.Errorf("memory must be between 128m and %dm", vm.MaxMemory>>20)
	}
	return size, nil
}

// defaultCPUs caps the default vCPU count. Build tools size their work by
// nproc, and four jobs still fit in the default 512m.
const defaultCPUs = 4

// parseCPUs reads --cpus: a whole number of vCPUs, from 1 to the host's
// logical processors. Docker's --cpus is a CPU-time quota and takes
// fractions; runwhp's counts processors, so a fraction is refused rather
// than quietly meaning something else. Empty means the host's count, up to
// defaultCPUs.
func parseCPUs(s string, host int) (int, error) {
	most := min(host, vm.MaxCPUs)
	s = strings.TrimSpace(s)
	if s == "" {
		return min(most, defaultCPUs), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		if _, ferr := strconv.ParseFloat(s, 64); ferr == nil {
			return 0, fmt.Errorf("--cpus counts vCPUs in runwhp; give a whole number, not %s", s)
		}
		return 0, fmt.Errorf("invalid --cpus %q", s)
	}
	if n < 1 || n > most {
		if most < host {
			return 0, fmt.Errorf("--cpus must be between 1 and %d", most)
		}
		return 0, fmt.Errorf("--cpus must be between 1 and %d, the logical processors on this machine", most)
	}
	return n, nil
}

func randomHostname() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusWriter collects the JSON status line init sends on the control port.
type statusWriter struct {
	mu     sync.Mutex
	buf    []byte
	status guestspec.Status
	ok     bool
}

func (w *statusWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		var st guestspec.Status
		if err := json.Unmarshal(w.buf[:i], &st); err == nil {
			w.status, w.ok = st, true
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *statusWriter) get() (guestspec.Status, bool) {
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
