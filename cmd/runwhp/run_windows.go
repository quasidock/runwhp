//go:build windows

package main

import (
	"context"
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
	"syscall"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/vm"
	"github.com/quasidock/runwhp/internal/vmm"
	"github.com/quasidock/runwhp/internal/volume"
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
	networks    listFlag
	aliases     listFlag
	ip          string
	publish     listFlag
	publishAll  bool
	expose      listFlag
	addHosts    listFlag
	volumes     listFlag
	mounts      listFlag
	tmpfs       listFlag
	storageOpts listFlag
	logDriver   string
	logOpts     listFlag
	restart     string
	name        string
	rm          bool
	detach      bool
	labels      listFlag
	labelFiles  listFlag
	stopSignal  string
	stopTimeout string
	privileged  bool

	healthCmd, healthInterval, healthTimeout              string
	healthStartPeriod, healthStartInterval, healthRetries string
	noHealthcheck                                         bool

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
	fs.StringVar(&o.memory, "m", "", "")
	fs.StringVar(&o.memory, "memory", "", "")
	fs.StringVar(&o.cpus, "cpus", "", "")
	fs.StringVar(&o.hostname, "hostname", "", "")
	fs.StringVar(&o.pull, "pull", "missing", "")
	fs.StringVar(&o.kernel, "kernel", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.Var(&o.networks, "network", "")
	fs.Var(&o.networks, "net", "")
	fs.Var(&o.aliases, "network-alias", "")
	fs.StringVar(&o.ip, "ip", "", "")
	fs.Var(&o.publish, "p", "")
	fs.Var(&o.publish, "publish", "")
	fs.BoolVar(&o.publishAll, "P", false, "")
	fs.BoolVar(&o.publishAll, "publish-all", false, "")
	fs.Var(&o.expose, "expose", "")
	fs.Var(&o.addHosts, "add-host", "")
	fs.Var(&o.volumes, "v", "")
	fs.Var(&o.volumes, "volume", "")
	fs.Var(&o.mounts, "mount", "")
	fs.Var(&o.tmpfs, "tmpfs", "")
	fs.Var(&o.storageOpts, "storage-opt", "")
	fs.StringVar(&o.restart, "restart", "no", "")
	fs.StringVar(&o.logDriver, "log-driver", "", "")
	fs.Var(&o.logOpts, "log-opt", "")
	fs.StringVar(&o.name, "name", "", "")
	fs.BoolVar(&o.rm, "rm", false, "")
	fs.BoolVar(&o.detach, "d", false, "")
	fs.BoolVar(&o.detach, "detach", false, "")
	fs.Var(&o.labels, "l", "")
	fs.Var(&o.labels, "label", "")
	fs.Var(&o.labelFiles, "label-file", "")
	fs.StringVar(&o.stopSignal, "stop-signal", "", "")
	fs.StringVar(&o.stopTimeout, "stop-timeout", "", "")
	fs.BoolVar(&o.privileged, "privileged", false, "")
	fs.StringVar(&o.healthCmd, "health-cmd", "", "")
	fs.StringVar(&o.healthInterval, "health-interval", "", "")
	fs.StringVar(&o.healthTimeout, "health-timeout", "", "")
	fs.StringVar(&o.healthStartPeriod, "health-start-period", "", "")
	fs.StringVar(&o.healthStartInterval, "health-start-interval", "", "")
	fs.StringVar(&o.healthRetries, "health-retries", "", "")
	fs.BoolVar(&o.noHealthcheck, "no-healthcheck", false, "")
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
  -m, --memory SIZE      guest memory, such as 512m or 16g (default: half this machine's)
      --cpus N           virtual processors (default: this machine's, up to 4)
  -d, --detach           run the container in the background and print its ID; its output
                         goes to files stdout and stderr are redirected to, else to its log
      --name NAME        name the container, as well as its ID
      --rm               remove the container when it stops; otherwise it is kept, with
                         its writable layer, until runwhp delete
      --restart POLICY   start the container again when its process ends: no (default),
                         on-failure[:N] (when it fails, at most N times), always or
                         unless-stopped (the same here), unless stopped or killed
  -l, --label KEY[=VALUE]  set a label on the container, over the image's (repeatable)
      --label-file FILE  read labels from a file of KEY=VALUE lines (repeatable)
      --stop-signal SIG  the signal stop sends first (default: the image's, or SIGTERM)
      --stop-timeout N   seconds stop waits before SIGKILL, unless told (default 10)
      --privileged       give the container its cgroup read-write, with controllers to
                         delegate, /sys read-write, and the FUSE, TUN and kmsg devices
      --health-cmd CMD   check the container's health by running CMD in it with /bin/sh,
                         over the image's HEALTHCHECK; healthy if it exits 0
      --health-interval D  time between checks (default 30s); and --health-timeout D
                         (default 30s), --health-start-period D (failures do not count
                         while it is starting), --health-start-interval D (checks while
                         starting, default 5s), --health-retries N (failures in a row
                         that make it unhealthy, default 3)
      --no-healthcheck   run no healthcheck, though the image has one
      --hostname NAME    container hostname (default: the start of its ID)
      --network NETWORK  user (default: outbound access through this machine, alone), none,
                         or a network made with runwhp network create, whose containers reach
                         each other by name (repeatable); NAME=N,alias=A,ip=X gives a network
                         its own alias and address
      --network-alias NAME  another name for the container on its first network (repeatable)
      --ip ADDRESS       the container's address on its first network
  -p, --publish [IP:][HOSTPORT:]PORT  forward a host port to a container TCP port (repeatable);
                         IP defaults to 127.0.0.1, and without HOSTPORT any free port is
                         taken, which runwhp port shows
  -P, --publish-all      publish every exposed TCP port, the image's and --expose's, on
                         a free host port
      --expose PORT[/PROTO]  a port the container listens on, published only with -P
                         (repeatable)
      --add-host NAME:IP add an /etc/hosts entry (repeatable); IP host-gateway reaches
                         this machine's 127.0.0.1
  -v, --volume HOSTPATH:PATH[:ro]  bind a host directory or file into the container (repeatable);
                         HOSTPATH is absolute or starts with a dot, and is created if missing
  -v, --volume NAME:PATH[:ro]  mount a named volume, created if missing; PATH alone makes an
                         anonymous volume, removed with the container
      --mount type=bind|volume|tmpfs,source=...,target=PATH[,readonly]  the same, Docker's long form
      --tmpfs PATH[:OPTIONS]  mount a tmpfs, by default noexec,nosuid,nodev (repeatable)
      --storage-opt size=SIZE  how large the container's writable layer may grow (default 64g)
      --log-driver local|none  keep the container's output in its log (local, the default)
                         or not; json-file is local, under Docker's name
      --log-opt max-size=SIZE|max-file=N  rotate the log as it reaches SIZE (default 20m;
                         -1 never), keeping N files (default 5) (repeatable)
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
	spec, err := parseRunOptions(&o, entrypointSet, fs.Args())
	if err != nil {
		errorf("%v", err)
		return guestspec.ExitRuntime
	}
	policy, err := image.ParsePolicy(o.pull)
	if err != nil {
		errorf("%v", err)
		return guestspec.ExitRuntime
	}
	return runForeground(&spec, o.name, policy)
}

// runForeground runs a container in this process, its output on this
// console, and returns its exit code, as run without -d does.
func runForeground(spec *runSpec, name string, policy image.Policy) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyLogf := func(string, ...any) {}
	if spec.debug {
		proxyLogf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	proxy := vmm.NewSignalProxy(cancel, proxyLogf)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go proxy.Run(sigs)
	code, err := run(ctx, spec, name, policy, proxy)
	if err != nil {
		if ctx.Err() != nil {
			errorf("interrupted")
			return 130
		}
		errorf("%v", err)
	}
	return code
}

func run(ctx context.Context, spec *runSpec, name string, policy image.Policy, proxy *vmm.SignalProxy) (int, error) {
	const fail = guestspec.ExitRuntime
	if spec.interactive && spec.tty && !vmm.IsTerminal(os.Stdin) {
		return fail, errors.New("the input device is not a TTY; if you are using mintty, try prefixing the command with 'winpty'")
	}
	root, err := storeRoot()
	if err != nil {
		return fail, err
	}
	logf := func(format string, a ...any) {}
	if spec.debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	start := time.Now()
	sweepStore(root, logf)
	// A --rm container's writable layer is a scratch disk, made while the
	// image is resolved; a kept one is made in the container's directory
	// once there is one.
	var layer *vmm.PendingLayer
	if spec.rm {
		layer = vmm.StartLayer(vmm.ScratchPath(root), spec.storageSize, true)
		defer layer.Close()
	}
	// A prune waits until the container is recorded, and keeps its layers.
	release, err := gc.Hold(root)
	if err != nil {
		return fail, err
	}
	cfg, err := resolveSpec(ctx, root, spec, policy, os.Stderr, logf)
	if err != nil {
		release()
		return fail, err
	}
	store := container.Open(root)
	rec, err := store.CreateWith(cfg, name, container.State{Pid: os.Getpid(), Output: container.OutputTerminal})
	release()
	if err != nil {
		return fail, err
	}
	if layer == nil {
		layer = vmm.StartLayer(rec.RW(), cfg.StorageSize, false)
		defer layer.Close()
	}
	sup, err := vmm.Supervise(root, store, rec, container.OutputTerminal)
	if err != nil {
		layer.Close()
		store.Remove(rec)
		return fail, err
	}
	logf("%4d ms  container %s recorded: %d layers", time.Since(start).Milliseconds(), rec.Short(), max(len(cfg.Layers), len(cfg.LayerFiles)))
	bio := vmm.ForegroundIO(cfg)
	var lw *container.LogWriter
	if !cfg.AutoRemove && cfg.KeepsLog() {
		// A kept container's output also goes to its log, as Docker keeps
		// one for every container, so that logs shows it later.
		if lw, err = container.OpenLogWriter(rec.Dir(), cfg.LogOptions(), errorf); err != nil {
			errorf("opening container %s's log: %v", rec.Short(), err)
		} else {
			defer lw.Close()
			bio.Stdout = teeWriter{bio.Stdout, lw.Stream(container.Stdout)}
			bio.Stderr = teeWriter{bio.Stderr, lw.Stream(container.Stderr)}
		}
	}
	return sup.RunForeground(ctx, vmm.Foreground{
		Layer: layer, Policy: policy, Proxy: proxy, IO: bio,
		Backoff: restartBackoff(errorf), Logf: logf, Start: start,
		BeforeHandOff: func() {
			if lw != nil {
				lw.Close()
			}
		},
		HandOff: spawnHandOff,
	})
}

// runSpec is a container to make: run's or create's flags and arguments,
// parsed, or a compose service, translated. resolveSpec turns it into the
// container's configuration.
type runSpec struct {
	image   string   // the image reference; empty with layers
	command []string // replaces the image's Cmd when not empty
	// entrypoint replaces the image's entrypoint when entrypointSet; empty,
	// it removes it.
	entrypoint    []string
	entrypointSet bool
	env           []string // KEY=VALUE, or KEY to copy from lookupEnv
	// lookupEnv resolves a bare KEY in env: os.LookupEnv for run's and
	// compose's; nil, for the Engine API's, drops it.
	lookupEnv   func(string) (string, bool)
	workdir     string
	user        string
	hostname    string // empty: the start of the container's ID
	interactive bool
	tty         bool

	memory  uint64
	cpus    int
	network networkRequest
	mounts  []mountSpec
	// apiPipe is the Engine API's pipe, which Docker's socket in mounts
	// reaches: the server's own, for the API's containers; empty, the
	// store's, for run's and compose's.
	apiPipe     string
	storageSize int64
	logs        logConfig
	restart     container.RestartPolicy
	rm          bool
	labels      map[string]string // over the image's
	stopSignal  string            // over the image's
	stopTimeout *int
	health      healthOverrides
	privileged  bool
	// exclusiveVolumes gives the container its volumes' disks, and never
	// shares them: the BuildKit builder's overlayfs cannot have its upper
	// layer on a shared volume's FUSE.
	exclusiveVolumes bool

	// Development flags: the kernel, init and kernel modules to use, and
	// ext4 layers instead of an image.
	kernel  string
	init    string
	layers  []string
	modules []string
	debug   bool
}

// parseRunOptions checks run's or create's flags and arguments, args being
// the image and the command, without yet touching the store.
func parseRunOptions(o *runOptions, entrypointSet bool, args []string) (runSpec, error) {
	s := runSpec{
		env: o.env, workdir: o.workdir, user: o.user, hostname: o.hostname,
		interactive: o.interactive, tty: o.tty, rm: o.rm, privileged: o.privileged,
		kernel: o.kernel, init: o.init, layers: o.layers, modules: o.modules, debug: o.debug,
		entrypointSet: entrypointSet,
		lookupEnv:     os.LookupEnv,
	}
	if entrypointSet && o.entrypoint != "" {
		s.entrypoint = []string{o.entrypoint}
	}
	switch {
	case len(o.layers) > 0:
		s.command = args
	case len(args) == 0:
		return s, fmt.Errorf("no image given; run 'runwhp run -h' for usage")
	default:
		s.image, s.command = args[0], args[1:]
	}
	var err error
	if s.memory, err = parseMemory(o.memory); err != nil {
		return s, err
	}
	if s.cpus, err = parseCPUs(o.cpus, runtime.NumCPU()); err != nil {
		return s, err
	}
	if s.network, err = parseNetworkRequest(networkFlags{modes: o.networks, aliases: o.aliases, ip: o.ip, publish: o.publish, addHosts: o.addHosts, expose: o.expose, publishAll: o.publishAll}); err != nil {
		return s, err
	}
	if s.mounts, err = parseMountFlags(o.volumes, o.mounts, o.tmpfs); err != nil {
		return s, err
	}
	if s.storageSize, err = parseStorageOpts(o.storageOpts); err != nil {
		return s, err
	}
	if s.logs, err = parseLogOptions(o.logDriver, o.logOpts); err != nil {
		return s, err
	}
	if s.restart, err = container.ParseRestartPolicy(o.restart); err != nil {
		return s, err
	}
	if s.restart.IsSet() && o.rm {
		return s, errors.New("conflicting options: cannot specify both --restart and --rm")
	}
	if s.labels, err = parseLabels(o.labels, o.labelFiles); err != nil {
		return s, err
	}
	if s.stopSignal, err = parseStopSignal(o.stopSignal); err != nil {
		return s, err
	}
	if o.stopTimeout != "" {
		n, err := strconv.Atoi(o.stopTimeout)
		if err != nil || n < 0 {
			return s, fmt.Errorf("invalid --stop-timeout %q: give a whole number of seconds", o.stopTimeout)
		}
		s.stopTimeout = &n
	}
	if s.health, err = parseHealthFlags(o); err != nil {
		return s, err
	}
	return s, nil
}

// resolveSpec turns a container to make into its configuration: it
// resolves the networks it names, pulls the image if need be, writing
// progress to out, makes missing volumes and bind sources, fetches
// runwhp's kernel if it is missing, and resolves everything that depends on
// this process, such as its working directory and environment.
func resolveSpec(ctx context.Context, root string, s *runSpec, policy image.Policy, out io.Writer, logf func(string, ...any)) (*container.Config, error) {
	network, err := planNetwork(root, s.network)
	if err != nil {
		return nil, err
	}
	pipe := s.apiPipe
	if pipe == "" {
		pipe = apiPipe(root)
	}
	plan, err := planMounts(s.mounts, pipe)
	if err != nil {
		return nil, err
	}
	if s.apiPipe == "" {
		warnUnservedPipes(out, plan.mounts)
	}
	cfg := &container.Config{
		Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(),
		Stdin: s.interactive, TTY: s.tty,
		Hosts: network.hosts, Network: network.enabled, NetworkMode: network.mode, Networks: network.networks, Publish: network.publish, HostGateway: network.hostGateway,
		Shares: plan.shares, Mounts: plan.mounts,
		Memory: s.memory, CPUs: s.cpus, StorageSize: s.storageSize, Debug: s.debug,
		LogDriver: s.logs.driver, LogMaxSize: s.logs.maxSize, LogMaxFiles: s.logs.maxFiles,
		Restart: s.restart, AutoRemove: s.rm,
	}

	// The process and its layers, from an image or (for development)
	// from ext4 files given with --layer.
	overrides := image.Overrides{
		Entrypoint: s.entrypoint, EntrypointSet: s.entrypointSet, Cmd: s.command,
		Env: s.env, Workdir: s.workdir, User: s.user, LookupEnv: s.lookupEnv,
	}
	var process image.Process
	var imageExposed map[string]struct{}
	if len(s.layers) > 0 {
		if process, err = image.BuildProcess(v1.Config{}, overrides); err != nil {
			return nil, err
		}
		cfg.Labels = withLabels(nil, s.labels)
		cfg.Healthcheck = resolveHealth(nil, s.health)
		for _, p := range s.layers {
			if cfg.LayerFiles, err = appendAbs(cfg.LayerFiles, p); err != nil {
				return nil, err
			}
		}
	} else {
		store := image.Open(root)
		store.Progress = out
		img, err := store.Ensure(ctx, s.image, policy)
		if err != nil {
			return nil, err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		if process, err = image.BuildProcess(cf.Config, overrides); err != nil {
			return nil, err
		}
		if cfg.Layers, err = img.Layers(); err != nil {
			return nil, err
		}
		cfg.Image, cfg.ImageName, cfg.ImageDigest, cfg.ImageID = s.image, img.Ref, img.Digest, img.ID()
		cfg.StopSignal = cf.Config.StopSignal
		cfg.Labels = withLabels(cf.Config.Labels, s.labels)
		cfg.Healthcheck = resolveHealth(imageHealth(img.Config), s.health)
		imageExposed = cf.Config.ExposedPorts
	}
	cfg.Expose = exposedPorts(imageExposed, s.network.expose, cfg.Publish)
	if s.network.publishAll && cfg.Network {
		addr, err := netstack.DefaultPublishAddress()
		if err != nil {
			return nil, fmt.Errorf("-P: %w", err)
		}
		more, skipped := publishAll(cfg.Expose, cfg.Publish, addr)
		cfg.Publish, cfg.PublishAll = append(cfg.Publish, more...), true
		if len(skipped) > 0 && out != nil {
			fmt.Fprintf(out, "WARNING: -P publishes TCP and UDP ports only; not %s\n", strings.Join(skipped, ", "))
		}
	}
	if s.stopSignal != "" {
		cfg.StopSignal = s.stopSignal
	}
	cfg.StopTimeout = s.stopTimeout
	cfg.Privileged = s.privileged
	cfg.Args, cfg.Env, cfg.Cwd, cfg.User = process.Args, process.Env, process.Cwd, process.User
	cfg.Entrypoint, cfg.Cmd = process.Entrypoint, process.Cmd

	if err := vmm.CheckDevices(cfg, len(plan.volumes)); err != nil {
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
		v, created, err := volumes.EnsureOpts(u.name, volume.Options{Size: size, Labels: u.labels, Shared: u.shared})
		if err != nil {
			return nil, err
		}
		if created {
			logf("created volume %s", v.Name)
		}
		cfg.Volumes = append(cfg.Volumes, container.Volume{Name: v.Name, Size: size, Exclusive: s.exclusiveVolumes})
	}

	// The kernel is fetched now, if it has to be, so that booting later
	// needs no network. Only a kernel named explicitly is recorded.
	if cfg.Kernel = vmm.ExplicitKernel(s.kernel); cfg.Kernel != "" {
		if cfg.Kernel, err = filepath.Abs(cfg.Kernel); err != nil {
			return nil, err
		}
	} else if _, err := vmm.ResolveKernel(ctx, root, "", policy); err != nil {
		return nil, err
	}
	if s.init != "" {
		if cfg.Init, err = filepath.Abs(s.init); err != nil {
			return nil, err
		}
	}
	for _, p := range s.modules {
		if cfg.Modules, err = appendAbs(cfg.Modules, p); err != nil {
			return nil, err
		}
	}

	if cfg.Hostname = s.hostname; cfg.Hostname == "" {
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

// parseMemory reads a size the way docker run -m does (b, k, m or g suffix,
// bytes without one) and rounds it up to 2 MiB. "" means defaultMemory.
//
// A guest takes from Windows only the memory it touches, a 2 MiB chunk at
// a time, and gives back what it frees, so a large default costs little:
// the guest kernel's page descriptors, 1/64 of its RAM, and about 8 ms of
// start-up per GiB, which it spends describing them.
func parseMemory(s string) (uint64, error) {
	if s == "" {
		return vmm.DefaultMemory(), nil
	}
	n, err := parseSize(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	size := (n + (2<<20 - 1)) &^ (2<<20 - 1)
	if maxMem := vm.MaxMemory(); size < 128<<20 || size > maxMem {
		return 0, fmt.Errorf("memory must be between 128m and %dm, this machine's memory", maxMem>>20)
	}
	return size, nil
}

// parseCPUs reads --cpus: a whole number of vCPUs, from 1 to the host's
// logical processors. Docker's --cpus is a CPU-time quota and takes
// fractions; runwhp's counts processors, so a fraction is refused rather
// than quietly meaning something else. Empty means the host's count, up to
// defaultCPUs.
func parseCPUs(s string, host int) (int, error) {
	most := min(host, vm.MaxCPUs)
	s = strings.TrimSpace(s)
	if s == "" {
		return min(most, vmm.DefaultCPUs), nil
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
