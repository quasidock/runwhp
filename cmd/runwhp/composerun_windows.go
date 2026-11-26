//go:build windows && compose

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"golang.org/x/sync/errgroup"

	"github.com/quasidock/runwhp/internal/compose"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/vmm"
)

// confirmPrompt asks a yes or no question on out, as compose rm asks it.
func confirmPrompt(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// follower follows containers' logs to one place, each line prefixed.
type follower struct {
	c       *composeCtx
	prefix  *compose.Prefixer
	filter  container.LogFilter
	follow  bool
	mu      sync.Mutex
	started map[string]bool
	wg      sync.WaitGroup
}

func (c *composeCtx) newFollower(names []string, prefix bool, filter container.LogFilter, follow bool) *follower {
	return &follower{c: c, prefix: compose.NewPrefixer(compose.Width(names), c.colour, prefix), filter: filter,
		follow: follow, started: map[string]bool{}}
}

// add follows a container's log, once, until ctx ends or, unless
// following for good, the container stops.
func (f *follower) add(ctx context.Context, ct *container.Container, forGood bool) {
	f.mu.Lock()
	if f.started[ct.ID] {
		f.mu.Unlock()
		return
	}
	f.started[ct.ID] = true
	f.mu.Unlock()
	name := f.c.shortName(ct)
	out, errs := f.prefix.Writer(name, f.c.stdout), f.prefix.Writer(name, f.c.stderr)
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer out.Close()
		defer errs.Close()
		var p *container.LogPrinter
		for p == nil {
			var err error
			if p, err = container.NewLogPrinter(ct.Dir(), out, errs, f.filter); err != nil {
				if !f.follow || ctx.Err() != nil {
					return
				}
				p = nil
				time.Sleep(100 * time.Millisecond)
			}
		}
		defer p.Close()
		for {
			if _, err := p.Copy(); err != nil {
				errorf("container %s: %v", ct.Short(), err)
				return
			}
			if !f.follow || p.Done {
				return
			}
			select {
			case <-ctx.Done():
				p.Copy()
				return
			case <-time.After(100 * time.Millisecond):
			}
			if !forGood && !ct.Locked() {
				p.Copy()
				return
			}
		}
	}()
}

// wait waits for every log followed to be done.
func (f *follower) wait() { f.wg.Wait() }

const composeLogsUsage = `Usage: runwhp compose logs [flags] [SERVICE...]

Print the output of the services' containers, each line after its
container's name.

  -f, --follow          follow the output until Ctrl+C
  -t, --timestamps      print the time each line was written
  -n, --tail N          print only the last N lines of each (default all)
      --since TIME      print lines written since TIME, a timestamp or a duration
      --until TIME      print lines written until TIME
      --index N         only container N of the service
      --no-color        do not colour the names
      --no-log-prefix   do not print the names
`

func (c *composeCtx) cmdLogs(args []string) int {
	fs := composeFlags("logs", composeLogsUsage)
	follow := fs.Bool("f", false, "")
	fs.BoolVar(follow, "follow", false, "")
	stamps := fs.Bool("t", false, "")
	fs.BoolVar(stamps, "timestamps", false, "")
	tail := fs.String("n", "all", "")
	fs.StringVar(tail, "tail", "all", "")
	since := fs.String("since", "", "")
	until := fs.String("until", "", "")
	index := fs.Int("index", 0, "")
	noColor := fs.Bool("no-color", false, "")
	noPrefix := fs.Bool("no-log-prefix", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	if *noColor {
		c.colour = false
	}
	filter := container.LogFilter{Tail: -1, Timestamps: *stamps}
	var err error
	if filter.Tail, err = parseTail(*tail); err != nil {
		errorf("%v", err)
		return 2
	}
	now := time.Now()
	if *since != "" {
		if filter.Since, err = parseLogTime(*since, now); err != nil {
			errorf("%v", err)
			return 2
		}
	}
	if *until != "" {
		if filter.Until, err = parseLogTime(*until, now); err != nil {
			errorf("%v", err)
			return 2
		}
	}
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, false); err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.checkServices(fs.Args()); err != nil {
		errorf("%v", err)
		return 1
	}
	cs, err := c.serviceContainers(fs.Args(), false)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if *index > 0 {
		cs = slices.DeleteFunc(cs, func(ct *container.Container) bool { return numberOf(ct) != *index })
	}
	var names []string
	for _, ct := range cs {
		names = append(names, c.shortName(ct))
	}
	f := c.newFollower(names, !*noPrefix, filter, *follow)
	for _, ct := range cs {
		f.add(ctx, ct, true)
		if !*follow {
			f.wait() // one container after another, in name order
		}
	}
	f.wait()
	if ctx.Err() != nil {
		return 130
	}
	return 0
}

// attached follows the output of up's containers until they stop: by
// themselves, or stopped by Ctrl+C or, with --abort-on-container-exit,
// once one does.
func (c *composeCtx) attached(ctx context.Context, second <-chan struct{}, plan *upPlan, o upOptions, since time.Time) int {
	var services []string
	for _, n := range plan.names {
		if plan.plans[n].Attach || slices.Contains(o.services, n) {
			services = append(services, n)
		}
	}
	cs, err := c.serviceContainers(services, false)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	var names []string
	for _, ct := range cs {
		names = append(names, c.shortName(ct))
	}
	fmt.Fprintf(c.stdout, "Attaching to %s\n", strings.Join(names, ", "))
	logCtx, stopLogs := context.WithCancel(context.Background())
	defer stopLogs()
	f := c.newFollower(names, !o.noLogPrefix, container.LogFilter{Tail: -1, Since: since.Add(-time.Second), Timestamps: o.timestamps}, true)
	for _, ct := range cs {
		f.add(logCtx, ct, true)
	}

	// Watch them stop.
	exited := make(chan *container.Container, len(cs))
	go func() {
		seen := map[string]time.Time{}
		for {
			running := 0
			for _, ct := range cs {
				now, err := c.store.Get(ct.ID)
				if err != nil {
					continue
				}
				fin := now.State.FinishedAt
				if now.State.Status == container.Stopped || now.State.Status == container.Restarting {
					if fin.After(since) && !fin.Equal(seen[ct.ID]) {
						seen[ct.ID] = fin
						time.Sleep(150 * time.Millisecond) // the last of its output
						f.prefix.Exited(c.stdout, c.shortName(ct), now.State.ExitCode)
						exited <- now
					}
				}
				if now.State.Active() {
					running++
				}
			}
			if running == 0 {
				close(exited)
				return
			}
			select {
			case <-logCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()

	code := 0
	// drain waits for the watcher to have seen every container stop, and
	// for the last of their output.
	drain := func() {
		deadline := time.After(10 * time.Second)
		for {
			select {
			case _, ok := <-exited:
				if ok {
					continue
				}
			case <-deadline:
			}
			break
		}
		stopLogs()
		f.wait()
	}
	stopAll := func(why string) {
		c.mu.Lock()
		fmt.Fprintf(c.stderr, " Compose Stopping %s\n", why)
		c.mu.Unlock()
		done := make(chan struct{})
		go func() {
			cs, _ := c.serviceContainers(plan.names, false)
			c.stopServices(context.Background(), cs, o.timeout, nil)
			close(done)
		}()
		select {
		case <-done:
		case <-second:
			cs, _ := c.serviceContainers(plan.names, false)
			for _, ct := range cs {
				killAndWait(c.store, ct)
			}
			<-done
		}
		c.mu.Lock()
		fmt.Fprintln(c.stderr)
		c.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			stopAll("Gracefully Stopping... press Ctrl+C again to force")
			drain()
			return 130
		case ct, ok := <-exited:
			if !ok {
				stopLogs()
				f.wait()
				return code
			}
			if o.exitCodeFrom != "" && serviceOf(ct) == o.exitCodeFrom || o.exitCodeFrom == "" && o.abort {
				code = ct.State.ExitCode
			}
			if o.abort {
				stopAll("Aborting on container exit...")
				drain()
				if o.exitCodeFrom != "" {
					if c, err := c.exitCodeOf(o.exitCodeFrom); err == nil {
						code = c
					}
				}
				return code
			}
		}
	}
}

// exitCodeOf is a service's first container's exit code.
func (c *composeCtx) exitCodeOf(service string) (int, error) {
	cs, err := c.serviceContainers([]string{service}, false)
	if err != nil || len(cs) == 0 {
		return 0, fmt.Errorf("no container of service %s", service)
	}
	return cs[0].State.ExitCode, nil
}

const composeExecUsage = `Usage: runwhp compose exec [flags] SERVICE COMMAND [ARG...]

Run a command in a service's running container.

  -d, --detach            run it in the background
  -T, --no-TTY            give it no terminal, though input is one
  -e, --env KEY[=VALUE]   set an environment variable (repeatable)
  -u, --user USER         user to run as
  -w, --workdir DIR       working directory
      --index N           container N of the service (default 1)
`

func (c *composeCtx) cmdExec(args []string) int {
	fs := composeFlags("exec", composeExecUsage)
	detach := fs.Bool("d", false, "")
	fs.BoolVar(detach, "detach", false, "")
	noTTY := fs.Bool("T", false, "")
	fs.BoolVar(noTTY, "no-TTY", false, "")
	fs.Bool("i", true, "")
	fs.Bool("interactive", true, "")
	var env listFlag
	fs.Var(&env, "e", "")
	fs.Var(&env, "env", "")
	user := fs.String("u", "", "")
	fs.StringVar(user, "user", "", "")
	workdir := fs.String("w", "", "")
	fs.StringVar(workdir, "workdir", "", "")
	index := fs.Int("index", 1, "")
	if fs.Parse(args) != nil || fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	if err := c.open(context.Background(), false); err != nil {
		errorf("%v", err)
		return 1
	}
	ct, err := c.running(fs.Arg(0), *index)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	argv := []string{"-i"}
	if *detach {
		argv = []string{"-d"}
	} else if !*noTTY && vmm.IsTerminal(os.Stdin) {
		argv = append(argv, "-t")
	}
	for _, e := range env {
		argv = append(argv, "-e", e)
	}
	if *user != "" {
		argv = append(argv, "-u", *user)
	}
	if *workdir != "" {
		argv = append(argv, "-w", *workdir)
	}
	return cmdExec(append(append(argv, ct.ID), fs.Args()[1:]...))
}

const composeRunUsage = `Usage: runwhp compose run [flags] SERVICE [COMMAND [ARG...]]

Run a one-off container of a service, after starting what it depends on.

  -d, --detach            run it in the background and print its ID
      --rm                remove it when it stops
      --name NAME         name it
      --entrypoint CMD    override the entrypoint
  -e, --env KEY[=VALUE]   set an environment variable (repeatable)
  -l, --label KEY=VALUE   set a label (repeatable)
  -u, --user USER         user to run as
  -w, --workdir DIR       working directory
  -v, --volume SRC:DST    bind a host directory or a volume (repeatable)
  -p, --publish H:C       publish a port (repeatable)
  -P, --service-ports     publish the service's ports
  -T, --no-TTY            give it no terminal, though input is one
      --no-deps           do not start what it depends on
      --build             build its image first
`

func (c *composeCtx) cmdRun(args []string) int {
	fs := composeFlags("run", composeRunUsage)
	detach := fs.Bool("d", false, "")
	fs.BoolVar(detach, "detach", false, "")
	rm := fs.Bool("rm", false, "")
	name := fs.String("name", "", "")
	entrypoint := fs.String("entrypoint", "", "")
	var env, labels, volumes, publish listFlag
	fs.Var(&env, "e", "")
	fs.Var(&env, "env", "")
	fs.Var(&labels, "l", "")
	fs.Var(&labels, "label", "")
	user := fs.String("u", "", "")
	fs.StringVar(user, "user", "", "")
	workdir := fs.String("w", "", "")
	fs.StringVar(workdir, "workdir", "", "")
	fs.Var(&volumes, "v", "")
	fs.Var(&volumes, "volume", "")
	fs.Var(&publish, "p", "")
	fs.Var(&publish, "publish", "")
	servicePorts := fs.Bool("P", false, "")
	fs.BoolVar(servicePorts, "service-ports", false, "")
	noTTY := fs.Bool("T", false, "")
	fs.BoolVar(noTTY, "no-TTY", false, "")
	fs.Bool("i", true, "")
	fs.Bool("interactive", true, "")
	noDeps := fs.Bool("no-deps", false, "")
	build := fs.Bool("build", false, "")
	fs.Bool("remove-orphans", false, "")
	fs.Bool("quiet-pull", false, "")
	if fs.Parse(args) != nil || fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	entrypointSet := false
	fs.Visit(func(f *flag.Flag) { entrypointSet = entrypointSet || f.Name == "entrypoint" })
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, true); err != nil {
		errorf("%v", err)
		return 1
	}
	service := fs.Arg(0)
	if err := c.checkServices([]string{service}); err != nil {
		errorf("%v", err)
		return 1
	}

	// What it depends on, started as up -d starts it.
	plan, err := c.plan([]string{service}, *noDeps, nil)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.prepare(ctx, plan, *build, false, "", false); err != nil {
		return c.failed(ctx, err)
	}
	var deps []string
	for _, n := range plan.names {
		if n != service {
			deps = append(deps, n)
		}
	}
	if len(deps) > 0 {
		depPlan := *plan
		depPlan.names = deps
		if depPlan.project, err = plan.project.WithSelectedServices(deps, types.IgnoreDependencies); err != nil {
			errorf("%v", err)
			return 1
		}
		if err := c.converge(ctx, &depPlan, upOptions{timeout: -1}); err != nil {
			return c.failed(ctx, err)
		}
		if err := c.startServices(ctx, depPlan.project, deps); err != nil {
			return c.failed(ctx, err)
		}
	}
	s := plan.project.Services[service]
	if err := c.waitDependencies(ctx, plan.project, service, s, nil); err != nil {
		return c.failed(ctx, err)
	}

	// The one-off: the service's container, as run's flags change it.
	ct := plan.plans[service]
	if !*servicePorts {
		ct.Ports = nil
	}
	ct.Restart = "no"
	if fs.NArg() > 1 {
		ct.Command = fs.Args()[1:]
	}
	if entrypointSet {
		ct.EntrypointSet, ct.Entrypoint = true, nil
		if *entrypoint != "" {
			ct.Entrypoint = []string{*entrypoint}
		}
	}
	if *user != "" {
		ct.User = *user
	}
	if *workdir != "" {
		ct.WorkingDir = *workdir
	}
	// A one-off answers to its own name only, as Docker Compose's does.
	for i := range ct.Networks {
		ct.Networks[i].Aliases = nil
	}
	ct.StdinOpen = !*detach
	ct.TTY = !*detach && !*noTTY && vmm.IsTerminal(os.Stdin)
	var slug [32]byte
	rand.Read(slug[:])
	slugHex := hex.EncodeToString(slug[:])
	l := c.labelsFor(plan, service, 0)
	l[compose.LabelOneoff], l[compose.LabelSlug] = "True", slugHex
	spec, err := specOf(ct, l)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	spec.env = append(spec.env, env...)
	for _, lb := range labels {
		k, v, _ := strings.Cut(lb, "=")
		spec.labels[k] = v
	}
	extra, err := parseMountFlags(volumes, nil, nil)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	spec.mounts = append(spec.mounts, extra...)
	for _, p := range publish {
		pt, err := parsePublish(p)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		spec.network.publish = append(spec.network.publish, pt)
	}
	spec.rm = *rm
	cname := *name
	if cname == "" {
		cname = compose.OneoffName(c.name, service, slugHex)
	}
	var creds []byte
	if plan.project.Services[service].UseAPISocket {
		if creds, err = apiSocketCredentials(); err != nil {
			errorf("%v", err)
			return 1
		}
		if !*detach && creds != nil {
			// A foreground run boots its container itself, with no created
			// container to copy them into first.
			c.warn("service %q: use_api_socket: the docker CLI's credentials are given to detached runs only", service)
			creds = nil
		}
		withAPISocketConfig(&spec, creds)
	}
	c.event("Container", cname, "Creating")
	if *detach {
		rec, code, err := createContainer(ctx, c.root, &spec, createOptions{name: cname, policy: image.PullMissing})
		if err == nil {
			if err = putAPISocketConfig(ctx, c.root, rec, creds); err != nil {
				code = 1
			}
		}
		if err != nil {
			errorf("%v", err)
			return code
		}
		c.event("Container", cname, "Created")
		if err := startContainerIO(ctx, c.store, rec.ID, false); err != nil {
			errorf("%v", err)
			return 1
		}
		fmt.Fprintln(c.stdout, rec.ID)
		return 0
	}
	c.event("Container", cname, "Created")
	stop() // runForeground passes Ctrl+C on to the container
	return runForeground(&spec, cname, image.PullMissing)
}

const composeBuildUsage = `Usage: runwhp compose build [--no-cache] [--pull] [-q] [SERVICE...]

Build the images of services with a build section.
`

func (c *composeCtx) cmdBuild(args []string) int {
	fs := composeFlags("build", composeBuildUsage)
	noCache := fs.Bool("no-cache", false, "")
	pull := fs.Bool("pull", false, "")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, true); err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.checkServices(fs.Args()); err != nil {
		errorf("%v", err)
		return 1
	}
	var pr compose.Problems
	for _, n := range dependencyOrder(c.project) {
		if len(fs.Args()) > 0 && !slices.Contains(fs.Args(), n) {
			continue
		}
		s := c.project.Services[n]
		b := compose.TranslateBuild(c.project, s, &pr)
		if err := pr.Err(); err != nil {
			errorf("%v", err)
			return 1
		}
		if b == nil {
			continue
		}
		if *quiet {
			c.stdout = io.Discard
		}
		if err := c.buildService(ctx, compose.ImageName(c.name, s), b, *noCache, *pull); err != nil {
			return c.failed(ctx, err)
		}
	}
	for _, w := range pr.Warnings {
		c.warn("%s", w)
	}
	return 0
}

// buildService builds a service's image, with BuildKit's progress on
// stdout, as Docker Compose shows it.
func (c *composeCtx) buildService(ctx context.Context, ref string, b *compose.Build, noCache, pull bool) error {
	req, err := buildRequestOf(b, noCache, pull)
	if err != nil {
		return err
	}
	release, err := c.gcHold()
	if err != nil {
		return err
	}
	defer release()
	c.event("Image", ref, "Building")
	if _, err := buildImage(ctx, c.root, req, c.stdout); err != nil {
		c.event("Image", ref, "Error "+err.Error())
		return fmt.Errorf("building %s: %w", ref, err)
	}
	c.event("Image", ref, "Built")
	return nil
}

func (c *composeCtx) cmdPull(args []string) int {
	fs := composeFlags("pull", "Usage: runwhp compose pull [-q] [--ignore-buildable] [SERVICE...]\n\nPull the services' images.\n")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	ignoreBuildable := fs.Bool("ignore-buildable", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, true); err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.checkServices(fs.Args()); err != nil {
		errorf("%v", err)
		return 1
	}
	var refs []string
	for _, n := range compose.ServicesOf(c.project, fs.Args()) {
		s := c.project.Services[n]
		if s.Image == "" || s.Build != nil && *ignoreBuildable {
			continue
		}
		if !slices.Contains(refs, s.Image) {
			refs = append(refs, s.Image)
		}
	}
	if err := c.pullImages(ctx, refs, !*quiet); err != nil {
		return c.failed(ctx, err)
	}
	return 0
}

// pullImages pulls images, four at a time.
func (c *composeCtx) pullImages(ctx context.Context, refs []string, events bool) error {
	if len(refs) == 0 {
		return nil
	}
	release, err := c.gcHold()
	if err != nil {
		return err
	}
	defer release()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, ref := range refs {
		g.Go(func() error {
			if events {
				c.event("Image", ref, "Pulling")
			}
			if _, err := image.Open(c.root).Ensure(gctx, ref, image.PullAlways); err != nil {
				c.event("Image", ref, "Error "+err.Error())
				return err
			}
			if events {
				c.event("Image", ref, "Pulled")
			}
			return nil
		})
	}
	return g.Wait()
}

func (c *composeCtx) cmdPush(args []string) int {
	fs := composeFlags("push", "Usage: runwhp compose push [-q] [SERVICE...]\n\nPush the images of services that have a build section and an image name.\n")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, true); err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.checkServices(fs.Args()); err != nil {
		errorf("%v", err)
		return 1
	}
	release, err := c.gcHold()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer release()
	is := image.Open(c.root)
	is.Progress = c.stderr
	for _, n := range compose.ServicesOf(c.project, fs.Args()) {
		s := c.project.Services[n]
		if s.Image == "" || s.Build == nil {
			continue
		}
		recs, err := pushRecords(is, s.Image, false)
		if err != nil {
			errorf("push: %v", err)
			return 1
		}
		c.event("Image", s.Image, "Pushing")
		out := c.stdout
		if *quiet {
			out = io.Discard
		}
		if err := pushTags(ctx, c.root, is, recs, false, false, out); err != nil {
			return c.failed(ctx, fmt.Errorf("push: %w", err))
		}
		c.event("Image", s.Image, "Pushed")
	}
	return 0
}
