//go:build windows && compose

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/graph"
	"github.com/compose-spec/compose-go/v2/types"
	"golang.org/x/sync/errgroup"

	"github.com/quasidock/runwhp/internal/compose"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/volume"
)

const composeUpUsage = `Usage: runwhp compose up [flags] [SERVICE...]

Create and start the services' containers, and what they depend on, and
follow their output until they stop, or Ctrl+C stops them. A container
whose service has not changed is left as it is; one whose service or
image has is made again.

  -d, --detach              start them in the background and return
      --build               build images before starting
      --no-build            build no image, though one is missing
      --pull POLICY         always, missing or never pull images
      --quiet-pull          pull without progress
      --force-recreate      make every container again
      --no-recreate         leave containers that exist as they are
      --no-deps             start the services named, not what they depend on
      --no-start            create the containers, without starting them
      --remove-orphans      remove containers of services the project no longer has
      --scale SERVICE=N     run N containers of a service (repeatable)
  -t, --timeout SECONDS     how long a container has to stop when it is replaced
      --wait                wait until the containers run, healthy if they have a
                            healthcheck; implies -d
      --wait-timeout SECONDS  give up waiting after this long
      --abort-on-container-exit  stop every container once one stops
      --exit-code-from SERVICE   exit with this service's exit code; implies
                            --abort-on-container-exit
      --no-log-prefix       do not prefix each line with its container's name
      --no-color            do not colour the prefixes
      --timestamps          print the time each line was written
`

// upOptions are up's and create's flags.
type upOptions struct {
	detach, build, noBuild, forceRecreate, noRecreate, noDeps, noStart bool
	removeOrphans, wait, abort, noLogPrefix, timestamps, quietPull     bool
	pull, exitCodeFrom                                                 string
	scale                                                              map[string]int
	timeout                                                            int
	waitTimeout                                                        time.Duration
	services                                                           []string
}

func (c *composeCtx) cmdUp(args []string, create bool) int {
	usage := composeUpUsage
	if create {
		usage = "Usage: runwhp compose create [--build] [--no-build] [--pull POLICY] [--force-recreate] [--no-recreate] [--remove-orphans] [--scale S=N] [SERVICE...]\n\nCreate the services' containers, ready to start.\n"
	}
	fs := composeFlags("up", usage)
	var o upOptions
	var scales listFlag
	var waitTimeout int
	fs.BoolVar(&o.detach, "d", false, "")
	fs.BoolVar(&o.detach, "detach", false, "")
	fs.BoolVar(&o.build, "build", false, "")
	fs.BoolVar(&o.noBuild, "no-build", false, "")
	fs.StringVar(&o.pull, "pull", "", "")
	fs.BoolVar(&o.quietPull, "quiet-pull", false, "")
	fs.BoolVar(&o.forceRecreate, "force-recreate", false, "")
	fs.BoolVar(&o.noRecreate, "no-recreate", false, "")
	fs.BoolVar(&o.noDeps, "no-deps", false, "")
	fs.BoolVar(&o.noStart, "no-start", false, "")
	fs.BoolVar(&o.removeOrphans, "remove-orphans", false, "")
	fs.Var(&scales, "scale", "")
	fs.IntVar(&o.timeout, "t", -1, "")
	fs.IntVar(&o.timeout, "timeout", -1, "")
	fs.BoolVar(&o.wait, "wait", false, "")
	fs.IntVar(&waitTimeout, "wait-timeout", 0, "")
	fs.BoolVar(&o.abort, "abort-on-container-exit", false, "")
	fs.StringVar(&o.exitCodeFrom, "exit-code-from", "", "")
	fs.BoolVar(&o.noLogPrefix, "no-log-prefix", false, "")
	noColor := fs.Bool("no-color", false, "")
	fs.BoolVar(&o.timestamps, "timestamps", false, "")
	fs.Bool("attach-dependencies", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	if *noColor {
		c.colour = false
	}
	o.services, o.noStart = fs.Args(), o.noStart || create
	o.waitTimeout = time.Duration(waitTimeout) * time.Second
	switch {
	case o.build && o.noBuild:
		errorf("--build and --no-build are incompatible")
		return 2
	case o.forceRecreate && o.noRecreate:
		errorf("--force-recreate and --no-recreate are incompatible")
		return 2
	case o.pull != "" && o.pull != "always" && o.pull != "missing" && o.pull != "never" && o.pull != "policy":
		errorf("--pull %s: want always, missing, never or policy", o.pull)
		return 2
	}
	if o.exitCodeFrom != "" {
		o.abort = true
	}
	if o.wait {
		o.detach = true
	}
	if o.abort && o.detach {
		errorf("--abort-on-container-exit and --detach are incompatible")
		return 2
	}
	o.scale = map[string]int{}
	for _, s := range scales {
		name, n, ok := strings.Cut(s, "=")
		count, err := strconv.Atoi(n)
		if !ok || err != nil || count < 0 {
			errorf("--scale %s: want SERVICE=NUMBER", s)
			return 2
		}
		o.scale[name] = count
	}
	ctx, second, stop := interruptibleTwice()
	defer stop()
	if err := c.open(ctx, true); err != nil {
		errorf("%v", err)
		return 1
	}
	return c.up(ctx, second, o)
}

// upPlan is what up makes of each service it runs.
type upPlan struct {
	project  *types.Project // the services up runs, and what they need
	names    []string
	specs    map[string]runSpec
	plans    map[string]compose.Container
	builds   map[string]*compose.Build
	imageIDs map[string]string
}

func (c *composeCtx) up(ctx context.Context, second <-chan struct{}, o upOptions) int {
	started := time.Now()
	plan, err := c.plan(o.services, o.noDeps, o.scale)
	if err != nil {
		printErr(err)
		return 1
	}
	if err := c.prepare(ctx, plan, o.build, o.noBuild, o.pull, o.quietPull); err != nil {
		return c.failed(ctx, err)
	}
	if err := c.orphans(ctx, o.removeOrphans, o.timeout); err != nil {
		return c.failed(ctx, err)
	}
	if err := c.converge(ctx, plan, o); err != nil {
		return c.failed(ctx, err)
	}
	if o.noStart {
		return 0
	}
	if err := c.startServices(ctx, plan.project, plan.names); err != nil {
		return c.failed(ctx, err)
	}
	if o.wait {
		if err := c.waitHealthy(ctx, plan.names, o.waitTimeout); err != nil {
			return c.failed(ctx, err)
		}
	}
	if o.detach {
		return 0
	}
	return c.attached(ctx, second, plan, o, started)
}

// failed reports an error from up, and gives its exit code.
func (c *composeCtx) failed(ctx context.Context, err error) int {
	if ctx.Err() != nil {
		errorf("interrupted")
		return 130
	}
	printErr(err)
	return 1
}

// plan checks the services up runs, all before anything is made: what
// runwhp cannot do as they ask, and how each would be run.
func (c *composeCtx) plan(services []string, noDeps bool, scale map[string]int) (*upPlan, error) {
	p := c.project
	if err := c.checkServices(services); err != nil {
		return nil, err
	}
	for name := range scale {
		if _, ok := p.Services[name]; !ok {
			return nil, fmt.Errorf("no such service: %s", name)
		}
	}
	sel := p
	if len(services) > 0 {
		var opts []types.DependencyOption
		if noDeps {
			opts = append(opts, types.IgnoreDependencies)
		}
		var err error
		if sel, err = p.WithSelectedServices(services, opts...); err != nil {
			return nil, err
		}
	}
	for name, n := range scale {
		if s, ok := sel.Services[name]; ok {
			s.SetScale(n)
			sel.Services[name] = s
		}
	}
	plan := &upPlan{project: sel, names: compose.ServicesOf(sel, nil), specs: map[string]runSpec{},
		plans: map[string]compose.Container{}, builds: map[string]*compose.Build{}, imageIDs: map[string]string{}}
	var pr compose.Problems
	compose.Check(sel, plan.names, &pr)
	for _, n := range plan.names {
		s := sel.Services[n]
		plan.plans[n] = compose.Translate(p, s, &pr)
		plan.builds[n] = compose.TranslateBuild(p, s, &pr)
	}
	for _, w := range pr.Warnings {
		c.warn("%s", w)
	}
	if err := pr.Err(); err != nil {
		return nil, err
	}
	for _, n := range plan.names {
		spec, err := specOf(plan.plans[n], nil)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", n, err)
		}
		plan.specs[n] = spec
	}
	return plan, nil
}

// prepare builds and pulls the images the services need, and makes their
// networks and volumes.
func (c *composeCtx) prepare(ctx context.Context, plan *upPlan, build, noBuild bool, pull string, quiet bool) error {
	is := image.Open(c.root)
	for _, n := range dependencyOrder(plan.project) {
		b := plan.builds[n]
		if b == nil || noBuild {
			continue
		}
		ref := plan.plans[n].Image
		if _, err := is.Get(ref); err == nil && !build && plan.plans[n].PullPolicy != "build" {
			continue
		}
		if err := c.buildService(ctx, ref, b, false, false); err != nil {
			return err
		}
	}
	var toPull []string
	for _, n := range plan.names {
		if plan.builds[n] != nil {
			continue
		}
		policy := plan.plans[n].PullPolicy
		if pull != "" && pull != "policy" {
			policy = pull
		}
		ref := plan.plans[n].Image
		if policy == "never" || slices.Contains(toPull, ref) {
			continue
		}
		if _, err := is.Get(ref); err == nil && policy != "always" {
			continue
		}
		toPull = append(toPull, ref)
	}
	if err := c.pullImages(ctx, toPull, true); err != nil {
		return err
	}
	for _, n := range plan.names {
		if img, err := is.Get(plan.plans[n].Image); err == nil {
			plan.imageIDs[n] = img.ID()
		}
	}
	for _, key := range c.usedNetworks(plan) {
		if err := c.ensureNetwork(key, c.project.Networks[key]); err != nil {
			return err
		}
	}
	for _, key := range c.usedVolumes(plan) {
		if err := c.ensureVolume(key, c.project.Volumes[key]); err != nil {
			return err
		}
	}
	return nil
}

// usedNetworks are the project's networks the services join.
func (c *composeCtx) usedNetworks(plan *upPlan) []string {
	used := map[string]bool{}
	for _, n := range plan.names {
		s := plan.project.Services[n]
		if pl := plan.plans[n]; pl.NoNetwork || pl.DefaultNetwork {
			continue
		}
		for key := range s.Networks {
			used[key] = true
		}
	}
	return slices.Sorted(maps.Keys(used))
}

// usedVolumes are the project's volumes the services mount.
func (c *composeCtx) usedVolumes(plan *upPlan) []string {
	used := map[string]bool{}
	for _, n := range plan.names {
		for _, v := range plan.project.Services[n].Volumes {
			if v.Type == types.VolumeTypeVolume && v.Source != "" {
				used[v.Source] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(used))
}

// ensureNetwork makes a project's network, labelled as Docker Compose
// labels its own, unless it is there; an external one must be.
func (c *composeCtx) ensureNetwork(key string, n types.NetworkConfig) error {
	ns := network.Open(c.root)
	name := n.Name
	if n.External {
		if _, err := ns.Resolve(name); err != nil {
			return fmt.Errorf("network %s declared as external, but could not be found", name)
		}
		return nil
	}
	if have, err := ns.Resolve(name); err == nil && have.Name == name {
		switch owner := have.Labels[compose.LabelProject]; owner {
		case c.name:
		case "":
			c.warn("a network with name %s exists but was not created by compose.\nSet `external: true` to use an existing network", name)
		default:
			c.warn("a network with name %s exists but was not created for project %q.\nSet `external: true` to use an existing network", name, c.name)
		}
		return nil
	}
	labels := withLabels(n.Labels, map[string]string{compose.LabelProject: c.name, compose.LabelNetwork: key,
		compose.LabelVersion: compose.Version(), compose.LabelConfigHash: compose.ResourceHash(n)})
	c.event("Network", name, "Creating")
	if _, err := ns.CreateWith(name, labels); err != nil && !errors.Is(err, network.ErrExists) {
		return err
	}
	c.event("Network", name, "Created")
	return nil
}

// ensureVolume makes a project's volume, labelled, unless it is there; an
// external one must be.
func (c *composeCtx) ensureVolume(key string, v types.VolumeConfig) error {
	vs := volume.Open(c.root)
	name := v.Name
	have, err := vs.Get(name)
	if v.External {
		if err != nil {
			return fmt.Errorf("external volume %q not found", name)
		}
		return nil
	}
	if err == nil {
		if have.Labels[compose.LabelProject] != c.name {
			c.warn("volume %q already exists but was not created by Docker Compose. Use `external: true` to use an existing volume", name)
		}
		return nil
	}
	labels := withLabels(v.Labels, map[string]string{compose.LabelProject: c.name, compose.LabelVolume: key,
		compose.LabelVersion: compose.Version(), compose.LabelConfigHash: compose.ResourceHash(v)})
	c.event("Volume", name, "Creating")
	if _, _, err := vs.EnsureWith(name, defaultDiskSize, labels); err != nil {
		return err
	}
	c.event("Volume", name, "Created")
	return nil
}

// orphans warns of containers of services the project no longer has, or
// removes them.
func (c *composeCtx) orphans(ctx context.Context, remove bool, timeout int) error {
	cs, err := c.projectContainers(false)
	if err != nil {
		return err
	}
	var orphans []*container.Container
	var names []string
	for _, ct := range cs {
		if _, ok := c.project.Services[serviceOf(ct)]; !ok {
			if _, disabled := c.project.DisabledServices[serviceOf(ct)]; !disabled {
				orphans = append(orphans, ct)
				names = append(names, ct.Name)
			}
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	if !remove {
		c.warn("Found orphan containers (%s) for this project. If you removed or renamed this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.", strings.Join(names, ", "))
		return nil
	}
	return c.removeAll(ctx, orphans, timeout)
}

// converge creates each service's containers, in dependency order, as
// many as its scale, makes again those whose service or image changed,
// and removes those beyond its scale.
func (c *composeCtx) converge(ctx context.Context, plan *upPlan, o upOptions) error {
	existing, err := c.projectContainers(false)
	if err != nil {
		return err
	}
	byService := map[string][]*container.Container{}
	for _, ct := range existing {
		byService[serviceOf(ct)] = append(byService[serviceOf(ct)], ct)
	}
	return graph.InDependencyOrder(ctx, plan.project, func(ctx context.Context, name string, s types.ServiceConfig) error {
		want := s.GetScale()
		hash := compose.ConfigHash(s)
		have := map[int]*container.Container{}
		var extra []*container.Container
		for _, ct := range byService[name] {
			if n := numberOf(ct); n >= 1 && n <= want && have[n] == nil {
				have[n] = ct
			} else {
				extra = append(extra, ct)
			}
		}
		if err := c.removeAll(ctx, extra, o.timeout); err != nil {
			return err
		}
		g, gctx := errgroup.WithContext(ctx)
		for i := 1; i <= want; i++ {
			ct := have[i]
			g.Go(func() error {
				switch {
				case ct == nil:
					return c.createReplica(gctx, plan, name, i, true)
				case o.noRecreate:
					return nil
				case o.forceRecreate || ct.Config.Labels[compose.LabelConfigHash] != hash ||
					plan.imageIDs[name] != "" && ct.Config.Labels[compose.LabelImage] != plan.imageIDs[name]:
					return c.recreate(gctx, plan, name, i, ct, o.timeout)
				}
				return nil
			})
		}
		return g.Wait()
	}, graph.WithMaxConcurrency(numCPU()))
}

// labelsFor are the labels Docker Compose gives a service's container.
func (c *composeCtx) labelsFor(plan *upPlan, name string, number int) map[string]string {
	s := plan.project.Services[name]
	l := map[string]string{
		compose.LabelProject: c.name, compose.LabelService: name, compose.LabelOneoff: "False",
		compose.LabelConfigHash: compose.ConfigHash(s), compose.LabelImage: plan.imageIDs[name],
		compose.LabelDependsOn: compose.DependsOnLabel(s), compose.LabelWorkingDir: c.project.WorkingDir,
		compose.LabelConfigFiles: compose.ConfigFiles(c.project), compose.LabelVersion: compose.Version(),
	}
	if number > 0 {
		l[compose.LabelNumber] = strconv.Itoa(number)
	}
	return l
}

// createReplica creates a service's container number i, ready to start.
func (c *composeCtx) createReplica(ctx context.Context, plan *upPlan, name string, i int, events bool) error {
	spec := plan.specs[name]
	spec.labels = withLabels(spec.labels, c.labelsFor(plan, name, i))
	cname := compose.ContainerName(c.name, plan.project.Services[name], i)
	if events {
		c.event("Container", cname, "Creating")
	}
	_, _, err := createContainer(ctx, c.root, &spec, createOptions{name: cname, policy: image.PullMissing})
	if err != nil {
		if events {
			c.event("Container", cname, "Error "+err.Error())
		}
		return err
	}
	if events {
		c.event("Container", cname, "Created")
	}
	return nil
}

// recreate makes a service's container again: stopped first, as a runwhp
// volume or address is one VM's at a time, then renamed aside, made anew,
// and removed.
func (c *composeCtx) recreate(ctx context.Context, plan *upPlan, name string, i int, old *container.Container, timeout int) error {
	c.event("Container", old.Name, "Recreate")
	if err := stopContainer(c.store, old.ID, stopTimeout(timeout)); err != nil {
		return err
	}
	cname := old.Name
	if err := c.store.Rename(old, old.ID[:12]+"_"+cname); err != nil {
		return err
	}
	if err := c.createReplica(ctx, plan, name, i, false); err != nil {
		if fresh, gerr := c.store.Get(old.ID); gerr == nil {
			c.store.Rename(fresh, cname)
		}
		return err
	}
	if err := deleteContainer(c.store, old.ID, true); err != nil {
		return err
	}
	c.event("Container", cname, "Recreated")
	return nil
}

// stopTimeout is -t's, or the container's own when -1.
func stopTimeout(t int) time.Duration { return time.Duration(t) * time.Second }

// removeAll stops and removes containers.
func (c *composeCtx) removeAll(ctx context.Context, cs []*container.Container, timeout int) error {
	g, _ := errgroup.WithContext(ctx)
	for _, ct := range cs {
		g.Go(func() error {
			if ct.State.Active() {
				c.event("Container", ct.Name, "Stopping")
				if err := stopContainer(c.store, ct.ID, stopTimeout(timeout)); err != nil {
					return err
				}
				c.event("Container", ct.Name, "Stopped")
			}
			c.event("Container", ct.Name, "Removing")
			if err := deleteContainer(c.store, ct.ID, true); err != nil && !errors.Is(err, container.ErrNotFound) {
				return err
			}
			c.event("Container", ct.Name, "Removed")
			return nil
		})
	}
	return g.Wait()
}

// startServices starts services' containers in dependency order, each
// once what it depends on is as its depends_on asks.
func (c *composeCtx) startServices(ctx context.Context, p *types.Project, names []string) error {
	sel := p
	if len(names) != len(p.Services) {
		var err error
		if sel, err = p.WithSelectedServices(names, types.IgnoreDependencies); err != nil {
			return err
		}
	}
	return graph.InDependencyOrder(ctx, sel, func(ctx context.Context, name string, s types.ServiceConfig) error {
		cs, err := c.serviceContainers([]string{name}, false)
		if err != nil {
			return err
		}
		if err := c.waitDependencies(ctx, sel, name, s, cs); err != nil {
			return err
		}
		g, gctx := errgroup.WithContext(ctx)
		for _, ct := range cs {
			g.Go(func() error {
				switch ct.State.Status {
				case container.Running, container.Restarting:
					c.event("Container", ct.Name, "Running")
					return nil
				}
				c.event("Container", ct.Name, "Starting")
				if err := startContainerIO(gctx, c.store, ct.ID, false); err != nil {
					c.event("Container", ct.Name, "Error "+err.Error())
					return err
				}
				c.event("Container", ct.Name, "Started")
				return nil
			})
		}
		return g.Wait()
	}, graph.WithMaxConcurrency(numCPU()))
}

// waitDependencies waits until a service's dependencies are as its
// depends_on asks: started, healthy, or completed successfully.
func (c *composeCtx) waitDependencies(ctx context.Context, p *types.Project, name string, s types.ServiceConfig, own []*container.Container) error {
	notStarted := func(dep, condition string) {
		for _, ct := range own {
			c.event("Container", ct.Name, fmt.Sprintf("not started: condition %q of service %q not met", condition, dep))
		}
	}
	for _, dep := range slices.Sorted(maps.Keys(s.DependsOn)) {
		d := s.DependsOn[dep]
		if _, ok := p.Services[dep]; !ok || d.Condition == types.ServiceConditionStarted || d.Condition == "" {
			continue
		}
		cs, err := c.serviceContainers([]string{dep}, false)
		if err != nil {
			return err
		}
		for _, ct := range cs {
			c.event("Container", ct.Name, "Waiting")
			switch d.Condition {
			case types.ServiceConditionHealthy:
				if err := c.waitHealth(ctx, ct, 0); err != nil {
					c.event("Container", ct.Name, "Error dependency "+dep+" failed to start")
					notStarted(dep, d.Condition)
					return fmt.Errorf("dependency failed to start: %w", err)
				}
				c.event("Container", ct.Name, "Healthy")
			case types.ServiceConditionCompletedSuccessfully:
				code, err := c.waitExit(ctx, ct)
				if err != nil {
					return err
				}
				if code != 0 {
					msg := fmt.Sprintf("service %q didn't complete successfully: exit %d", dep, code)
					c.event("Container", ct.Name, "Error "+msg)
					notStarted(dep, d.Condition)
					return errors.New(msg)
				}
				c.event("Container", ct.Name, "Exited")
			}
		}
	}
	return nil
}

// waitHealth waits for a container to be healthy, or until timeout if not
// zero.
func (c *composeCtx) waitHealth(ctx context.Context, ct *container.Container, timeout time.Duration) error {
	if !ct.Config.Healthcheck.Enabled() {
		return fmt.Errorf("container %s has no healthcheck configured", ct.Name)
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		deadline = time.After(timeout)
	}
	for {
		now, err := c.store.Get(ct.ID)
		if err != nil {
			return err
		}
		switch {
		case now.State.Health != nil && now.State.Health.Status == container.HealthHealthy && now.State.Status == container.Running:
			return nil
		case now.State.Health != nil && now.State.Health.Status == container.HealthUnhealthy:
			return fmt.Errorf("container %s is unhealthy", ct.Name)
		case now.State.Status == container.Stopped && !now.State.FinishedAt.IsZero():
			return fmt.Errorf("container %s exited (%d)", ct.Name, now.State.ExitCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for container %s to be healthy", ct.Name)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitExit waits for a container to stop, and returns its exit code.
func (c *composeCtx) waitExit(ctx context.Context, ct *container.Container) (int, error) {
	for {
		now, err := c.store.Get(ct.ID)
		if err != nil {
			return 0, err
		}
		if now.State.Status == container.Stopped && !now.State.FinishedAt.IsZero() && !now.Locked() {
			return now.State.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitHealthy is up --wait: each service's containers running, and healthy
// if they have a healthcheck.
func (c *composeCtx) waitHealthy(ctx context.Context, names []string, timeout time.Duration) error {
	cs, err := c.serviceContainers(names, false)
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, ct := range cs {
		g.Go(func() error {
			c.event("Container", ct.Name, "Waiting")
			if ct.Config.Healthcheck.Enabled() {
				if err := c.waitHealth(gctx, ct, timeout); err != nil {
					return err
				}
				c.event("Container", ct.Name, "Healthy")
				return nil
			}
			now, err := c.store.Get(ct.ID)
			if err != nil {
				return err
			}
			if now.State.Status != container.Running {
				return fmt.Errorf("container %s is %s", ct.Name, stateWord(now.State))
			}
			c.event("Container", ct.Name, "Started")
			return nil
		})
	}
	return g.Wait()
}

// stopServices stops containers in reverse dependency order, those of a
// service at once.
func (c *composeCtx) stopServices(ctx context.Context, cs []*container.Container, timeout int, then func(*container.Container) error) error {
	byService := map[string][]*container.Container{}
	for _, ct := range cs {
		byService[serviceOf(ct)] = append(byService[serviceOf(ct)], ct)
	}
	stopGroup := func(group []*container.Container) error {
		g, _ := errgroup.WithContext(ctx)
		for _, ct := range group {
			g.Go(func() error {
				if ct.State.Active() {
					c.event("Container", ct.Name, "Stopping")
					if err := stopContainer(c.store, ct.ID, stopTimeout(timeout)); err != nil {
						return err
					}
					c.event("Container", ct.Name, "Stopped")
				}
				if then != nil {
					return then(ct)
				}
				return nil
			})
		}
		return g.Wait()
	}
	var known *types.Project
	if c.project != nil {
		known = c.project
	}
	var rest []*container.Container
	if known != nil {
		var mu sync.Mutex
		err := graph.InDependencyOrder(ctx, known, func(ctx context.Context, name string, _ types.ServiceConfig) error {
			mu.Lock()
			group := byService[name]
			delete(byService, name)
			mu.Unlock()
			return stopGroup(group)
		}, graph.InReverseOrder)
		if err != nil {
			return err
		}
	}
	for _, group := range byService {
		rest = append(rest, group...)
	}
	return stopGroup(rest)
}

const composeDownUsage = `Usage: runwhp compose down [-v] [--rmi local|all] [--remove-orphans] [-t SECONDS]

Stop and remove the project's containers, and the networks it made.

  -v, --volumes          remove the project's volumes too
      --rmi local|all    remove the images built for services (local), or every
                         service's image (all)
      --remove-orphans   remove containers of services the project no longer has
  -t, --timeout SECONDS  how long each container has to stop (default: its own)
`

func (c *composeCtx) cmdDown(args []string) int {
	fs := composeFlags("down", composeDownUsage)
	vols := fs.Bool("v", false, "")
	fs.BoolVar(vols, "volumes", false, "")
	rmi := fs.String("rmi", "", "")
	removeOrphans := fs.Bool("remove-orphans", false, "")
	timeout := fs.Int("t", -1, "")
	fs.IntVar(timeout, "timeout", -1, "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	if *rmi != "" && *rmi != "local" && *rmi != "all" {
		errorf("--rmi %s: want local or all", *rmi)
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	if err := c.open(ctx, false); err != nil {
		errorf("%v", err)
		return 1
	}
	all, err := c.projectContainers(true)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	var cs, orphans []*container.Container
	for _, ct := range all {
		if c.project != nil && ct.Config.Labels[compose.LabelOneoff] != "True" {
			if _, ok := c.project.Services[serviceOf(ct)]; !ok && !*removeOrphans {
				orphans = append(orphans, ct)
				continue
			}
		}
		cs = append(cs, ct)
	}
	if len(orphans) > 0 {
		var names []string
		for _, ct := range orphans {
			names = append(names, ct.Name)
		}
		c.warn("Found orphan containers (%s) for this project. If you removed or renamed this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.", strings.Join(names, ", "))
	}
	found := len(cs) > 0
	err = c.stopServices(ctx, cs, *timeout, func(ct *container.Container) error {
		c.event("Container", ct.Name, "Removing")
		if err := deleteContainer(c.store, ct.ID, true); err != nil && !errors.Is(err, container.ErrNotFound) {
			return err
		}
		c.event("Container", ct.Name, "Removed")
		return nil
	})
	failed := false
	if err != nil {
		errorf("%v", err)
		failed = true
	}
	if *vols {
		f, bad := c.removeVolumes()
		found, failed = found || f, failed || bad
	}
	f, bad := c.removeNetworks()
	found, failed = found || f, failed || bad
	if *rmi != "" {
		f, bad := c.removeImages(*rmi, all)
		found, failed = found || f, failed || bad
	}
	if !found {
		c.warn("Warning: No resource found to remove for project %q.", c.name)
	}
	if failed {
		return 1
	}
	return 0
}

// removeNetworks removes the networks the project made.
func (c *composeCtx) removeNetworks() (found, failed bool) {
	ns := network.Open(c.root)
	// Those the project declares, and any other it made, as its
	// services' networks were.
	var names []string
	if c.project != nil {
		for _, key := range slices.Sorted(maps.Keys(c.project.Networks)) {
			if n := c.project.Networks[key]; !n.External {
				names = append(names, n.Name)
			}
		}
	}
	if list, err := ns.List(); err == nil {
		for _, n := range list {
			if n.Labels[compose.LabelProject] == c.name && !slices.Contains(names, n.Name) {
				names = append(names, n.Name)
			}
		}
	}
	for _, name := range names {
		n, err := ns.Resolve(name)
		if err != nil || n.Name != name || n.Labels[compose.LabelProject] != c.name {
			continue
		}
		found = true
		c.event("Network", name, "Removing")
		if _, err := ns.Remove(n.ID); err != nil {
			errorf("%v", err)
			failed = true
			continue
		}
		c.event("Network", name, "Removed")
	}
	return found, failed
}

// removeVolumes removes the volumes the project made, as down -v does.
func (c *composeCtx) removeVolumes() (found, failed bool) {
	vs := volume.Open(c.root)
	type vol struct{ name string }
	var names []string
	if c.project != nil {
		for _, key := range slices.Sorted(maps.Keys(c.project.Volumes)) {
			if v := c.project.Volumes[key]; !v.External {
				names = append(names, v.Name)
			}
		}
	} else if list, err := vs.List(); err == nil {
		for _, v := range list {
			if v.Labels[compose.LabelProject] == c.name {
				names = append(names, v.Name)
			}
		}
	}
	for _, name := range names {
		v, err := vs.Get(name)
		if err != nil {
			continue
		}
		found = true
		if v.Labels[compose.LabelProject] != c.name {
			c.event("Volume", name, "Skipped Volume was not created by Docker Compose")
			continue
		}
		c.event("Volume", name, "Removing")
		if err := vs.Remove(name); err != nil {
			errorf("%v", err)
			failed = true
			continue
		}
		c.event("Volume", name, "Removed")
	}
	return found, failed
}

// removeImages removes the services' images, as down --rmi does: those
// built for services without an image name, or all.
func (c *composeCtx) removeImages(which string, cs []*container.Container) (found, failed bool) {
	var refs []string
	if c.project != nil {
		for _, n := range slices.Sorted(maps.Keys(c.project.Services)) {
			s := c.project.Services[n]
			if which == "all" || s.Image == "" {
				refs = append(refs, compose.ImageName(c.name, s))
			}
		}
	} else if which == "all" {
		for _, ct := range cs {
			if ct.Config.Image != "" && !slices.Contains(refs, ct.Config.Image) {
				refs = append(refs, ct.Config.Image)
			}
		}
	}
	is := image.Open(c.root)
	release, err := exclusive(c.root)
	if err != nil {
		errorf("%v", err)
		return false, true
	}
	defer release()
	for _, ref := range refs {
		img, err := is.Get(ref)
		if err != nil {
			continue
		}
		found = true
		shown := image.Familiar(img.Ref)
		c.event("Image", shown, "Removing")
		_, u, ok := loadStore()
		if !ok {
			return found, true
		}
		p, errs := u.RemoveImages([]string{ref}, false)
		r := u.Apply(p)
		if len(errs) > 0 || printReport(r) > 0 {
			for _, err := range errs {
				errorf("%v", err)
			}
			failed = true
			continue
		}
		c.event("Image", shown, "Removed")
	}
	return found, failed
}

func (c *composeCtx) cmdLifecycle(cmd string, args []string) int {
	fs := composeFlags(cmd, fmt.Sprintf("Usage: runwhp compose %s [flags] [SERVICE...]\n\n%s the services' containers.\n", cmd, strings.ToUpper(cmd[:1])+cmd[1:]))
	timeout := fs.Int("t", -1, "")
	fs.IntVar(timeout, "timeout", -1, "")
	signal := fs.String("s", "SIGKILL", "")
	fs.StringVar(signal, "signal", "SIGKILL", "")
	if fs.Parse(args) != nil {
		return 2
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
	each := func(f func(*container.Container) error) int {
		g, _ := errgroup.WithContext(ctx)
		for _, ct := range cs {
			g.Go(func() error { return f(ct) })
		}
		if err := g.Wait(); err != nil {
			errorf("%v", err)
			return 1
		}
		return 0
	}
	switch cmd {
	case "start":
		if c.project == nil {
			return each(func(ct *container.Container) error {
				c.event("Container", ct.Name, "Starting")
				if err := startContainerIO(ctx, c.store, ct.ID, false); err != nil {
					return err
				}
				c.event("Container", ct.Name, "Started")
				return nil
			})
		}
		names := fs.Args()
		if len(names) == 0 {
			names = compose.ServicesOf(c.project, nil)
		}
		if err := c.startServices(ctx, c.project, names); err != nil {
			errorf("%v", err)
			return 1
		}
	case "stop":
		if err := c.stopServices(ctx, cs, *timeout, nil); err != nil {
			errorf("%v", err)
			return 1
		}
	case "restart":
		return each(func(ct *container.Container) error {
			c.event("Container", ct.Name, "Restarting")
			if err := stopContainer(c.store, ct.ID, stopTimeout(*timeout)); err != nil {
				return err
			}
			if err := startContainerIO(ctx, c.store, ct.ID, false); err != nil {
				return err
			}
			c.event("Container", ct.Name, "Started")
			return nil
		})
	case "kill":
		sig, err := guestspec.ParseSignal(*signal)
		if err != nil {
			errorf("%v", err)
			return 2
		}
		var running []*container.Container
		for _, ct := range cs {
			if ct.State.Status == container.Running {
				running = append(running, ct)
			}
		}
		if len(running) == 0 {
			fmt.Fprintln(c.stderr, "No container to kill")
			return 0
		}
		cs = running
		return each(func(ct *container.Container) error {
			c.event("Container", ct.Name, "Killing")
			if err := ask(context.Background(), c.store, ct, container.Request{Op: container.OpKill, Signal: sig}, 10*time.Second); err != nil {
				return err
			}
			c.event("Container", ct.Name, "Killed")
			return nil
		})
	case "pause", "unpause":
		op, done := container.OpPause, "Paused"
		if cmd == "unpause" {
			op, done = container.OpUnpause, "Unpaused"
		}
		return each(func(ct *container.Container) error {
			if err := ask(context.Background(), c.store, ct, container.Request{Op: op}, 30*time.Second); err != nil {
				return err
			}
			c.event("Container", ct.Name, done)
			return nil
		})
	}
	return 0
}

func (c *composeCtx) cmdRm(args []string) int {
	fs := composeFlags("rm", `Usage: runwhp compose rm [-f] [-s] [-v] [SERVICE...]

Remove the services' stopped containers.

  -f, --force    do not ask
  -s, --stop     stop running containers first
  -v, --volumes  remove anonymous volumes too, which go with their containers anyway
`)
	force := fs.Bool("f", false, "")
	fs.BoolVar(force, "force", false, "")
	stopFirst := fs.Bool("s", false, "")
	fs.BoolVar(stopFirst, "stop", false, "")
	fs.Bool("v", false, "")
	fs.Bool("volumes", false, "")
	if fs.Parse(args) != nil {
		return 2
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
	if *stopFirst {
		if err := c.stopServices(ctx, cs, -1, nil); err != nil {
			errorf("%v", err)
			return 1
		}
		if cs, err = c.serviceContainers(fs.Args(), false); err != nil {
			errorf("%v", err)
			return 1
		}
	}
	var stopped []*container.Container
	for _, ct := range cs {
		if !ct.State.Active() || ct.State.Status == container.Created {
			stopped = append(stopped, ct)
		}
	}
	if len(stopped) == 0 {
		fmt.Fprintln(c.stderr, "No stopped containers")
		return 0
	}
	var names []string
	for _, ct := range stopped {
		names = append(names, ct.Name)
	}
	fmt.Fprintf(c.stdout, "Going to remove %s\n", strings.Join(names, ", "))
	if !*force && !confirmPrompt(c.stdin, c.stdout, "Are you sure? [yN] ") {
		return 0
	}
	sort.Slice(stopped, func(i, j int) bool { return stopped[i].Name < stopped[j].Name })
	g, _ := errgroup.WithContext(ctx)
	for _, ct := range stopped {
		g.Go(func() error {
			c.event("Container", ct.Name, "Removing")
			if err := deleteContainer(c.store, ct.ID, true); err != nil {
				return err
			}
			c.event("Container", ct.Name, "Removed")
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}

// gcHold holds the store from prunes, as a build does.
func (c *composeCtx) gcHold() (func(), error) { return gc.Hold(c.root) }
