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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/boot"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
)

const (
	defaultKernel = `C:\Program Files\WSL\tools\kernel`
	baseCmdline   = "console=ttyS0 nokaslr panic=-1 reboot=acpi"
	consoleTail   = 200 // kernel console lines kept for error reports
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

type runOptions struct {
	entrypoint string
	env        listFlag
	workdir    string
	user       string
	memory     string
	hostname   string
	pull       string
	kernel     string
	debug      bool

	// Development flags, left out of the usage text.
	init    string
	layers  listFlag
	modules listFlag
}

func runFlags(o *runOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
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
	fs.StringVar(&o.kernel, "kernel", defaultKernel, "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.StringVar(&o.init, "init", "", "")
	fs.Var(&o.layers, "layer", "")
	fs.Var(&o.modules, "module", "")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: runwhp run [flags] IMAGE [COMMAND [ARG...]]

Flags:
  --entrypoint CMD       override the image's entrypoint ("" removes it)
  -e, --env KEY[=VALUE]  set an environment variable (repeatable); KEY alone copies it from here
  -w, --workdir DIR      working directory inside the container
  -u, --user USER[:GROUP]  user to run as: name or uid, optionally with a group
  -m, --memory SIZE      guest memory, such as 512m or 2g (default 512m)
      --hostname NAME    container hostname (default: random)
      --pull POLICY      missing, always or never (default missing)
      --kernel PATH      Linux bzImage to boot (default: the WSL kernel)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, &o, entrypointSet, fs.Args())
	if err != nil {
		if ctx.Err() != nil {
			errorf("interrupted")
			return 130
		}
		errorf("%v", err)
	}
	return code
}

func run(ctx context.Context, o *runOptions, entrypointSet bool, args []string) (int, error) {
	const fail = guestspec.ExitRuntime
	memory, err := parseMemory(o.memory)
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
		policy, err := image.ParsePolicy(o.pull)
		if err != nil {
			return fail, err
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
	if 1+len(layerFiles) > vm.MaxDevices {
		return fail, fmt.Errorf("the image has %d layers; runwhp supports at most %d", len(layerFiles), vm.MaxDevices-1)
	}
	logf("%4d ms  image ready: %d layers", time.Since(start).Milliseconds(), len(layerFiles))

	kernelImage, err := os.ReadFile(o.kernel)
	if err != nil {
		return fail, fmt.Errorf("reading the kernel: %w", err)
	}
	kernel, err := boot.ParseKernel(kernelImage)
	if err != nil {
		return fail, fmt.Errorf("%s: %w", o.kernel, err)
	}
	modules, err := kernelModules(root, kernel.Version, o.modules)
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
		Hostname: hostname, Layers: len(layerFiles), Debug: o.debug,
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
		{Name: "modules", Mode: boot.ModeDir | 0o755},
	}
	for _, m := range modules {
		files = append(files, boot.File{Name: "modules/" + m.name, Mode: boot.ModeFile | 0o644, Data: m.data})
	}

	// Devices: the console first, then one disk per layer, bottom first.
	status := &statusWriter{}
	console := &virtio.Console{
		Ports: []virtio.Port{
			{Name: guestspec.PortStdout, Out: os.Stdout},
			{Name: guestspec.PortStderr, Out: os.Stderr},
			{Name: guestspec.PortControl, Out: status},
		},
		Logf: logf,
	}
	devices := []virtio.Device{console}
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

	res, err := vm.Run(ctx, vm.Config{
		Kernel:      kernel,
		Initrd:      boot.CPIO(files),
		Cmdline:     cmdline,
		Memory:      memory,
		Devices:     devices,
		ConsoleLine: consoleLine,
		Logf:        logf,
	})
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

type module struct {
	name string
	data []byte
}

// kernelModules returns the modules to load in the guest: those given with
// --module, or every .ko in the store's directory for this kernel version,
// which must include virtio_blk.
func kernelModules(root, version string, explicit []string) ([]module, error) {
	paths := explicit
	if len(paths) == 0 {
		if version == "" {
			return nil, fmt.Errorf("the kernel image has no version string, so its modules cannot be found; pass --module")
		}
		dir := filepath.Join(root, "modules", version)
		found, _ := filepath.Glob(filepath.Join(dir, "*.ko"))
		slices.Sort(found)
		if !slices.ContainsFunc(found, func(p string) bool { return filepath.Base(p) == "virtio_blk.ko" }) {
			return nil, fmt.Errorf("kernel %s needs virtio_blk.ko in %s.\n"+
				"Copy it out of WSL once, from a runwhp checkout, in PowerShell:\n"+
				"  wsl -e sh hack/wsl-modules.sh \"%s\"", version, dir, filepath.Join(root, "modules"))
		}
		paths = found
	}
	var out []module
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, module{filepath.Base(p), data})
	}
	return out, nil
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
