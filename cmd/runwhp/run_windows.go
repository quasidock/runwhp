//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
      --hostname NAME    container hostname (default: the start of its ID)
      --network NETWORK  user (default: outbound access through this machine, alone), none,
                         or a network made with runwhp network create, whose containers reach
                         each other by name (repeatable); NAME=N,alias=A,ip=X gives a network
                         its own alias and address
      --network-alias NAME  another name for the container on its first network (repeatable)
      --ip ADDRESS       the container's address on its first network
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
      --log-driver local|none  keep the container's output in its log (local, the default)
                         or not
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyLogf := func(string, ...any) {}
	if o.debug {
		proxyLogf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	proxy := vmm.NewSignalProxy(cancel, proxyLogf)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go proxy.Run(sigs)
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

func run(ctx context.Context, o *runOptions, entrypointSet bool, args []string, proxy *vmm.SignalProxy) (int, error) {
	const fail = guestspec.ExitRuntime
	if o.interactive && o.tty && !vmm.IsTerminal(os.Stdin) {
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
	// A --rm container's writable layer is a scratch disk, made while the
	// image is resolved; a kept one is made in the container's directory
	// once there is one.
	var layer *vmm.PendingLayer
	if o.rm {
		size, err := parseStorageOpts(o.storageOpts)
		if err != nil {
			return fail, err
		}
		layer = vmm.StartLayer(vmm.ScratchPath(root), size, true)
		defer layer.Close()
	}
	// A prune waits until the container is recorded, and keeps its layers.
	release, err := gc.Hold(root)
	if err != nil {
		return fail, err
	}
	cfg, err := resolveConfig(ctx, root, o, entrypointSet, args, policy, logf)
	if err != nil {
		release()
		return fail, err
	}
	cfg.AutoRemove = o.rm
	store := container.Open(root)
	rec, err := store.CreateWith(cfg, o.name, container.State{Pid: os.Getpid(), Output: container.OutputTerminal})
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
	network, err := parseNetworkFlags(root, networkFlags{modes: o.networks, aliases: o.aliases, ip: o.ip, publish: o.publish, addHosts: o.addHosts})
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
	logs, err := parseLogOptions(o.logDriver, o.logOpts)
	if err != nil {
		return nil, err
	}
	restart, err := container.ParseRestartPolicy(o.restart)
	if err != nil {
		return nil, err
	}
	if restart.IsSet() && o.rm {
		return nil, errors.New("conflicting options: cannot specify both --restart and --rm")
	}
	cfg := &container.Config{
		Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(),
		Stdin: o.interactive, TTY: o.tty,
		Hosts: network.hosts, Network: network.enabled, NetworkMode: network.mode, Networks: network.networks, Publish: network.publish, HostGateway: network.hostGateway,
		Shares: plan.shares, Mounts: plan.mounts,
		Memory: memory, CPUs: cpus, StorageSize: storageSize, Debug: o.debug,
		LogDriver: logs.driver, LogMaxSize: logs.maxSize, LogMaxFiles: logs.maxFiles,
		Restart: restart,
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
		cfg.Image, cfg.ImageName, cfg.ImageDigest, cfg.ImageID = args[0], img.Ref, img.Digest, img.ID()
		cfg.StopSignal = cf.Config.StopSignal
	}
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
	if cfg.Kernel = vmm.ExplicitKernel(o.kernel); cfg.Kernel != "" {
		if cfg.Kernel, err = filepath.Abs(cfg.Kernel); err != nil {
			return nil, err
		}
	} else if _, err := vmm.ResolveKernel(ctx, root, "", policy); err != nil {
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
