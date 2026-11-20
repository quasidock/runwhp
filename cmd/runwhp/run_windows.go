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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/fuse"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/hostfs"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
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
	hostname    string
	pull        string
	kernel      string
	debug       bool
	network     string
	publish     listFlag
	addHosts    listFlag
	volumes     listFlag
	mounts      listFlag

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
      --hostname NAME    container hostname (default: random)
      --network MODE     user (default: outbound access through this machine) or none
  -p, --publish [IP:]HOSTPORT:PORT  forward a host port to a container TCP port (repeatable);
                         IP defaults to 127.0.0.1
      --add-host NAME:IP add an /etc/hosts entry (repeatable); IP host-gateway reaches
                         this machine's 127.0.0.1
  -v, --volume HOSTPATH:PATH[:ro]  bind a host directory or file into the container (repeatable);
                         HOSTPATH is absolute or starts with a dot, and is created if missing
      --mount type=bind,source=HOSTPATH,target=PATH[,readonly]  the same, Docker's long form
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
	network, err := parseNetworkFlags(o.network, o.publish, o.addHosts)
	if err != nil {
		return fail, err
	}
	shares, mounts, err := parseBinds(o.volumes, o.mounts)
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
	others := 1 // the console
	if network.enabled {
		others++
	}
	if len(shares) > 0 {
		others++
	}
	if others+len(layerFiles) > vm.MaxDevices {
		return fail, fmt.Errorf("the image has %d layers; runwhp supports at most %d here (%d with --network none and no -v)",
			len(layerFiles), vm.MaxDevices-others, vm.MaxDevices-1)
	}
	logf("%4d ms  image ready: %d layers", time.Since(start).Milliseconds(), len(layerFiles))

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

	hostname := o.hostname
	if hostname == "" {
		hostname = randomHostname()
	}
	spec := guestspec.Spec{
		Args: config.Args, Env: config.Env, Cwd: config.Cwd, User: config.User,
		Hostname: hostname, Hosts: network.hosts, Layers: len(layerFiles), Debug: o.debug,
		Stdin: o.interactive, TTY: o.tty, Network: network.spec(), Mounts: mounts,
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
	// file system if any, then one disk per layer, bottom first.
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
	if len(shares) > 0 {
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
		f, err := os.Open(path)
		if err != nil {
			return fail, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return fail, err
		}
		devices = append(devices, &virtio.Blk{File: f, Sectors: uint64(st.Size()) / 512, ReadOnly: true,
			Serial: guestspec.LayerSerial(i)})
	}

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
	s = strings.ToLower(strings.TrimSpace(s))
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "b"):
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	size := (uint64(n*float64(mult)) + (2<<20 - 1)) &^ (2<<20 - 1)
	if size < 128<<20 || size > vm.MaxMemory {
		return 0, fmt.Errorf("memory must be between 128m and %dm", vm.MaxMemory>>20)
	}
	return size, nil
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
