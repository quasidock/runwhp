//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/build"
	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/vmm"
)

const buildUsage = `Usage: runwhp build [flags] PATH | -

Build an image from a Dockerfile, as docker build does, running each RUN,
COPY and ADD in a guest of its own. PATH is the build context, a directory;
- reads the Dockerfile from stdin, with no context.

  -t, --tag NAME         name the image (repeatable); untagged without one
  -f, --file FILE        the Dockerfile; PATH\Dockerfile by default, - for stdin
      --build-arg K[=V]  set a build argument; K alone takes it from the
                         environment (repeatable)
      --target STAGE     build this stage, and the stages it needs
      --no-cache         run every step, though the cache has it
      --pull             pull every FROM image, though it is in the store
  -q, --quiet            print only the image ID
      --iidfile FILE     write the image ID to FILE
      --network MODE     RUN steps' network: default (runwhp's user
                         network), host (the same) or none
      --add-host NAME:IP add an /etc/hosts entry to RUN steps (repeatable)
      --label K=V        set a label on the image (repeatable)
      --secret id=ID[,src=FILE|,env=VAR]
                         a secret for RUN --mount=type=secret (repeatable)
      --platform P       linux/amd64, the only one
  -m, --memory SIZE      each RUN step's memory; half this machine's by default
      --cpus N           each RUN step's vCPUs; the host's, up to 4, by default
      --progress MODE    plain (the default), auto, or quiet, as -q
      --debug            the guests' kernel consoles and runwhp's steps
`

func cmdBuild(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("build", buildUsage)
	var tags, buildArgs, hosts, labels, secrets listFlag
	fs.Var(&tags, "t", "")
	fs.Var(&tags, "tag", "")
	file := fs.String("f", "", "")
	fs.StringVar(file, "file", "", "")
	fs.Var(&buildArgs, "build-arg", "")
	target := fs.String("target", "", "")
	noCache := fs.Bool("no-cache", false, "")
	pull := fs.Bool("pull", false, "")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	iidfile := fs.String("iidfile", "", "")
	network := fs.String("network", "default", "")
	fs.Var(&hosts, "add-host", "")
	fs.Var(&labels, "label", "")
	fs.Var(&secrets, "secret", "")
	platform := fs.String("platform", "", "")
	memory := fs.String("m", "", "")
	fs.StringVar(memory, "memory", "", "")
	cpus := fs.String("cpus", "", "")
	progressMode := fs.String("progress", "plain", "")
	debug := fs.Bool("debug", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		errorf("build takes exactly one PATH, or -")
		fs.Usage()
		return 2
	}
	switch *progressMode {
	case "plain", "auto", "tty":
	case "quiet":
		*quiet = true
	default:
		errorf("--progress %s: want plain, auto or quiet", *progressMode)
		return 2
	}
	if p := strings.ToLower(*platform); p != "" && p != "linux/amd64" && p != "linux/x86_64" {
		errorf("--platform %s: runwhp builds linux/amd64 only", *platform)
		return 2
	}
	var noNetwork bool
	switch *network {
	case "default", "user", "host", "":
	case "none":
		noNetwork = true
	default:
		errorf("--network %s: want default, host or none", *network)
		return 2
	}
	opts := build.Options{Target: *target, NoCache: *noCache, NoNetwork: noNetwork, Hosts: hosts,
		BuildArgs: map[string]string{}, Labels: map[string]string{}, Secrets: map[string]build.Secret{}}
	for _, a := range buildArgs {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			if v, ok = os.LookupEnv(k); !ok {
				continue // as Docker's client: unset, so not passed
			}
		}
		opts.BuildArgs[k] = v
	}
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		opts.Labels[k] = v
	}
	var refs []string
	for _, t := range tags {
		n, err := image.Normalize(t)
		if err != nil {
			errorf("invalid argument %q for \"-t, --tag\" flag: %v", t, err)
			return 2
		}
		if strings.Contains(t, "@") {
			errorf("invalid argument %q for \"-t, --tag\" flag: a tag cannot name a digest", t)
			return 2
		}
		refs = append(refs, n)
	}
	mem, err := parseMemory(*memory)
	if err != nil {
		errorf("%v", err)
		return 2
	}
	ncpu, err := parseCPUs(*cpus, runtime.NumCPU())
	if err != nil {
		errorf("%v", err)
		return 2
	}

	// The Dockerfile and the context.
	path := fs.Arg(0)
	switch {
	case path == "-":
		if *file != "" && *file != "-" {
			errorf("a Dockerfile from stdin takes no -f")
			return 2
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			errorf("reading the Dockerfile from stdin: %v", err)
			return 1
		}
		opts.Dockerfile, opts.DockerfileName = data, "Dockerfile"
	default:
		dockerfile := *file
		switch {
		case dockerfile == "-":
			data, err := io.ReadAll(stdin)
			if err != nil {
				errorf("reading the Dockerfile from stdin: %v", err)
				return 1
			}
			opts.Dockerfile, dockerfile = data, ""
		case dockerfile == "":
			dockerfile = filepath.Join(path, "Dockerfile")
		}
		if dockerfile != "" {
			data, err := os.ReadFile(dockerfile)
			if err != nil {
				errorf("failed to read dockerfile: %v", err)
				return 1
			}
			opts.Dockerfile = data
		}
		opts.DockerfileName = cmpOr(filepath.Base(dockerfile), "Dockerfile")
		ignore, err := build.ReadIgnore(path, dockerfile)
		if err != nil {
			errorf("reading .dockerignore: %v", err)
			return 1
		}
		if opts.Context, err = build.NewContext(path, ignore); err != nil {
			errorf("%v", err)
			return 1
		}
	}

	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	sweepStore(root, func(string, ...any) {})
	ctx, stop := interruptible()
	defer stop()

	for _, s := range secrets {
		sec, cleanup, err := parseSecret(root, s)
		if err != nil {
			errorf("%v", err)
			return 2
		}
		defer cleanup()
		opts.Secrets[sec.id] = build.Secret{Path: sec.path}
	}

	policy := image.PullMissing
	if *pull {
		policy = image.PullAlways
	}
	kernel := vmm.ExplicitKernel("")
	if _, err := vmm.ResolveKernel(ctx, root, kernel, image.PullMissing); err != nil {
		errorf("%v", err)
		return 1
	}
	out := io.Writer(stderr)
	if *quiet {
		out = nil
	}
	progress := build.NewProgress(out, time.Now)
	opts.Progress = progress
	opts.TempDir = filepath.Join(root, "tmp")
	if err := os.MkdirAll(opts.TempDir, 0o755); err != nil {
		errorf("%v", err)
		return 1
	}
	is := image.Open(root)
	worker := &build.VM{Root: root, Images: is, Policy: policy, Memory: mem, CPUs: ncpu,
		StorageSize: defaultDiskSize, Kernel: kernel, Debug: *debug}
	res, err := build.Build(ctx, worker, buildcache.Open(root), opts)
	if err != nil {
		if ctx.Err() != nil {
			errorf("build cancelled")
			return 130
		}
		var le *build.LocatedError
		if errors.As(err, &le) {
			fmt.Fprintln(stderr, le.Excerpt())
		}
		fmt.Fprintf(stderr, "ERROR: failed to build: failed to solve: %v\n", err)
		return 1
	}

	v := progress.Vertex("exporting to image")
	v.Log("exporting layers done")
	if len(refs) == 0 {
		if _, err := is.Record("", res.Config); err != nil {
			v.End(err)
			errorf("%v", err)
			return 1
		}
		v.Log("writing image %s done", res.ID)
	} else {
		v.Log("writing image %s done", res.ID)
		for _, r := range refs {
			if _, err := is.Record(r, res.Config); err != nil {
				v.End(err)
				errorf("%v", err)
				return 1
			}
			v.Log("naming to %s done", r)
		}
	}
	v.End(nil)
	if *iidfile != "" {
		if err := writeFileAtomically(*iidfile, func(w io.Writer) error {
			_, err := io.WriteString(w, res.ID)
			return err
		}); err != nil {
			errorf("writing %s: %v", *iidfile, err)
			return 1
		}
	}
	if len(res.Warnings) > 0 && !*quiet {
		fmt.Fprintf(stderr, "\n %d warning%s found:\n", len(res.Warnings), plural(len(res.Warnings)))
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, " - %s\n", w)
		}
	}
	if *quiet {
		fmt.Fprintln(stdout, res.ID)
	}
	return 0
}

// buildSecret is a --secret: its id, and the file that holds it.
type buildSecret struct{ id, path string }

// parseSecret reads --secret id=ID[,src=FILE|,env=VAR][,type=file|env]. A
// secret from the environment is written to a file in the store's tmp,
// which cleanup removes.
func parseSecret(root, s string) (buildSecret, func(), error) {
	var sec buildSecret
	var env string
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return sec, nil, fmt.Errorf("--secret %s: want KEY=VALUE fields", s)
		}
		switch strings.ToLower(k) {
		case "id":
			sec.id = v
		case "src", "source":
			sec.path = v
		case "env":
			env = v
		case "type":
			if v != "file" && v != "env" {
				return sec, nil, fmt.Errorf("--secret %s: unsupported type %q", s, v)
			}
		default:
			return sec, nil, fmt.Errorf("--secret %s: unexpected key %q", s, k)
		}
	}
	if sec.id == "" {
		return sec, nil, fmt.Errorf("--secret %s: id is required", s)
	}
	if sec.path == "" && env == "" {
		env = sec.id // as Docker's client: the environment variable of that name
		if _, ok := os.LookupEnv(env); !ok {
			sec.path = sec.id
		}
	}
	if sec.path != "" {
		abs, err := filepath.Abs(sec.path)
		if err != nil {
			return sec, nil, err
		}
		sec.path = abs
		if _, err := os.Stat(abs); err != nil {
			return sec, nil, fmt.Errorf("--secret %s: %w", s, err)
		}
		return sec, func() {}, nil
	}
	v, ok := os.LookupEnv(env)
	if !ok {
		return sec, nil, fmt.Errorf("--secret %s: environment variable %s is not set", s, env)
	}
	dir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return sec, nil, err
	}
	f, err := os.CreateTemp(dir, "secret-*")
	if err != nil {
		return sec, nil, err
	}
	_, err = io.Copy(f, bytes.NewBufferString(v))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return sec, nil, err
	}
	sec.path = f.Name()
	return sec, func() { os.Remove(f.Name()) }, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
