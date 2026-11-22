//go:build windows

package main

import (
	"context"
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

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
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
	name        string
	rm          bool
	detach      bool

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
	fs.StringVar(&o.name, "name", "", "")
	fs.BoolVar(&o.rm, "rm", false, "")
	fs.BoolVar(&o.detach, "d", false, "")
	fs.BoolVar(&o.detach, "detach", false, "")
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
  -d, --detach           run the container in the background and print its ID; its output
                         goes to files stdout and stderr are redirected to, else to its log
      --name NAME        name the container, as well as its ID
      --rm               remove the container when it stops; otherwise it is kept, with
                         its writable layer, until runwhp delete
      --hostname NAME    container hostname (default: the start of its ID)
      --network MODE     user (default: outbound access through this machine) or none
  -p, --publish [IP:]HOSTPORT:PORT  forward a host port to a container TCP port (repeatable);
                         IP defaults to 127.0.0.1
      --add-host NAME:IP add an /etc/hosts entry (repeatable); IP host-gateway reaches
                         this machine's 127.0.0.1
  -v, --volume HOSTPATH:PATH[:ro]  bind a host directory or file into the container (repeatable);
                         HOSTPATH is absolute or starts with a dot, and is created if missing
  -v, --volume NAME:PATH[:ro]  mount a named volume, created if missing; PATH alone makes an
                         anonymous volume, removed with the container
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
	if o.detach {
		return runDetached(&o, entrypointSet, fs.Args())
	}

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
	sweepStore(root, logf)
	cfg, err := resolveConfig(ctx, root, o, entrypointSet, args, policy, logf)
	if err != nil {
		return fail, err
	}
	cfg.AutoRemove = o.rm
	store := container.Open(root)
	rec, err := store.Create(cfg, o.name)
	if err != nil {
		return fail, err
	}
	sup, err := supervise(root, store, rec, container.OutputTerminal, proxy)
	if err != nil {
		store.Remove(rec)
		return fail, err
	}
	logf("%4d ms  container %s recorded: %d layers", time.Since(start).Milliseconds(), rec.Short(), max(len(cfg.Layers), len(cfg.LayerFiles)))
	code, err := bootContainer(ctx, root, rec, policy, proxy, foregroundIO(cfg), sup.hooks(false), logf, start)
	sup.finish(code, err)
	return code, err
}

// resolveConfig turns run's flags and arguments into a container's
// configuration: it pulls the image if need be, makes missing volumes,
// fetches runwhp's kernel if it is missing, and resolves everything that
// depends on this process, such as its working directory and environment.
func resolveConfig(ctx context.Context, root string, o *runOptions, entrypointSet bool, args []string, policy image.Policy, logf func(string, ...any)) (*container.Config, error) {
	memory, err := parseMemory(o.memory)
	if err != nil {
		return nil, err
	}
	cpus, err := parseCPUs(o.cpus, runtime.NumCPU())
	if err != nil {
		return nil, err
	}
	network, err := parseNetworkFlags(o.network, o.publish, o.addHosts)
	if err != nil {
		return nil, err
	}
	plan, err := parseMounts(o.volumes, o.mounts, o.tmpfs)
	if err != nil {
		return nil, err
	}
	storageSize, err := parseStorageOpts(o.storageOpts)
	if err != nil {
		return nil, err
	}
	cfg := &container.Config{
		Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(),
		Stdin: o.interactive, TTY: o.tty,
		Hosts: network.hosts, Network: network.enabled, Publish: network.publish, HostGateway: network.hostGateway,
		Shares: plan.shares, Mounts: plan.mounts,
		Memory: memory, CPUs: cpus, StorageSize: storageSize, Debug: o.debug,
	}

	// The process and its layers, from an image or (for development)
	// from ext4 files given with --layer.
	overrides := image.Overrides{
		Entrypoint: o.entrypoint, EntrypointSet: entrypointSet,
		Env: o.env, Workdir: o.workdir, User: o.user, LookupEnv: os.LookupEnv,
	}
	var process image.Process
	if len(o.layers) > 0 {
		overrides.Cmd = args
		if process, err = image.BuildProcess(v1.Config{}, overrides); err != nil {
			return nil, err
		}
		for _, p := range o.layers {
			if cfg.LayerFiles, err = appendAbs(cfg.LayerFiles, p); err != nil {
				return nil, err
			}
		}
	} else {
		if len(args) == 0 {
			return nil, fmt.Errorf("no image given; run 'runwhp run -h' for usage")
		}
		store := image.Open(root)
		store.Progress = os.Stderr
		img, err := store.Ensure(ctx, args[0], policy)
		if err != nil {
			return nil, err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		overrides.Cmd = args[1:]
		if process, err = image.BuildProcess(cf.Config, overrides); err != nil {
			return nil, err
		}
		if cfg.Layers, err = img.Layers(); err != nil {
			return nil, err
		}
		cfg.Image, cfg.ImageName, cfg.ImageDigest = args[0], img.Ref, img.Digest
		cfg.StopSignal = cf.Config.StopSignal
	}
	cfg.Args, cfg.Env, cfg.Cwd, cfg.User = process.Args, process.Env, process.Cwd, process.User

	if err := checkDevices(cfg, len(plan.volumes)); err != nil {
		return nil, err
	}

	// Named volumes are made now, as docker create makes them.
	volumes := volume.Open(root)
	for _, u := range plan.volumes {
		size := int64(defaultDiskSize)
		if u.size != "" {
			if size, err = parseDiskSize(u.size); err != nil {
				return nil, err
			}
		}
		v, created, err := volumes.Ensure(u.name, size)
		if err != nil {
			return nil, err
		}
		if created {
			logf("created volume %s", v.Name)
		}
		cfg.Volumes = append(cfg.Volumes, container.Volume{Name: v.Name, Size: size})
	}

	// The kernel is fetched now, if it has to be, so that booting later
	// needs no network. Only a kernel named explicitly is recorded.
	if cfg.Kernel = explicitKernel(o.kernel); cfg.Kernel != "" {
		if cfg.Kernel, err = filepath.Abs(cfg.Kernel); err != nil {
			return nil, err
		}
	} else if _, err := resolveKernel(ctx, root, "", policy); err != nil {
		return nil, err
	}
	if o.init != "" {
		if cfg.Init, err = filepath.Abs(o.init); err != nil {
			return nil, err
		}
	}
	for _, p := range o.modules {
		if cfg.Modules, err = appendAbs(cfg.Modules, p); err != nil {
			return nil, err
		}
	}

	if cfg.Hostname = o.hostname; cfg.Hostname == "" {
		cfg.Hostname = cfg.ID[:12] // as Docker names it
	}
	return cfg, nil
}

func appendAbs(list []string, p string) ([]string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return list, err
	}
	return append(list, abs), nil
}

// checkDevices checks that the container's devices fit the I/O APIC: each
// takes an interrupt line.
func checkDevices(cfg *container.Config, volumes int) error {
	// The console and the scratch disk, and the others named here.
	others, users := 2, []string{}
	if cfg.Network {
		others++
		users = append(users, "the network")
	}
	if len(cfg.Shares) > 0 {
		others++
		users = append(users, "bind mounts")
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
		return fmt.Errorf("too many devices: %s, the console and the scratch disk make %d, and runwhp has room for %d", what, n, vm.MaxDevices)
	}
	return nil
}

// explicitKernel is the bzImage --kernel names, else the one RUNWHP_KERNEL
// names, else empty, which means runwhp's own kernel.
func explicitKernel(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("RUNWHP_KERNEL")
}

// resolveKernel returns the bzImage to boot: path if it is not empty, else
// runwhp's own kernel from the store, fetched first if it is missing and the
// pull policy allows.
func resolveKernel(ctx context.Context, root, path string, policy image.Policy) (string, error) {
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

// eventWriter reads the Events init sends on the control port, as JSON
// lines, and keeps how the container ended.
type eventWriter struct {
	mu     sync.Mutex
	buf    []byte
	status guestspec.Status
	ok     bool
	// onReady, if set, is called when a container booted to wait for its
	// start is ready; onStarted, when the container process starts, with
	// its pid in the guest.
	onReady   func()
	onStarted func(pid int)
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
