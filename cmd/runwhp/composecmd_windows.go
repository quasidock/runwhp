//go:build windows && compose

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/quasidock/runwhp/internal/compose"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/vmm"
)

const composeUsage = `Usage: runwhp compose [-f FILE]... [-p NAME] [--project-directory DIR]
                      [--profile PROFILE]... [--env-file FILE]... COMMAND [ARGS...]

Run a compose.yaml's services as Docker Compose runs them: each container
in a runwhp of its own, in the background, on the project's networks,
named and labelled as Docker Compose names and labels its own.

Commands:
  up [flags] [SERVICE...]          create and start the services' containers, and follow
                                   their output unless -d
  down [flags]                     stop and remove the project's containers and networks
  create [flags] [SERVICE...]      create the containers, ready to start
  start | stop | restart | kill | pause | unpause [SERVICE...]
  rm [-f] [-s] [SERVICE...]        remove stopped containers
  ps [flags] [SERVICE...]          list the project's containers
  logs [flags] [SERVICE...]        print the containers' output
  exec [flags] SERVICE COMMAND...  run a command in a service's container
  run [flags] SERVICE [COMMAND...] run a one-off container of a service
  build [flags] [SERVICE...]       build the services' images
  pull [SERVICE...]                pull the services' images
  push [SERVICE...]                push the images built for services
  config [flags]                   print the project as it is read
  images [SERVICE...]              list the images the containers use
  port [--index N] SERVICE PORT    print the host address of a published port
  ls [-a] [-q] [--format FMT]      list projects
  version                          print the version

Run 'runwhp compose COMMAND -h' for a command's flags. The project is
the compose.yaml (or compose.yml, docker-compose.yaml, docker-compose.yml)
in this directory or the nearest above it, or the files -f or COMPOSE_FILE
name; its name is -p's, COMPOSE_PROJECT_NAME's, the files', or the
directory's.
`

func numCPU() int { return runtime.NumCPU() }

// composeBuild says this is runwhp-compose.exe, which runs compose for
// runwhp.exe, apart from it: compose-go's dependencies take some 8 ms to
// start, which no other runwhp process should pay.
const composeBuild = true

// composeCtx is a compose command's project and where it writes.
type composeCtx struct {
	root           string
	load           compose.LoadOptions
	stdin          io.Reader
	stdout, stderr io.Writer
	store          *container.Store
	// project is nil when the command found no compose file and works on
	// the containers labelled with name alone.
	project *types.Project
	name    string
	colour  bool
	mu      sync.Mutex // orders progress lines
}

// cmdCompose runs runwhp compose. It returns 1 when a command fails, 2 for
// a usage error, and a container's exit code where Docker Compose does.
func cmdCompose(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("compose", composeUsage)
	var files, profiles, envFiles listFlag
	var name, dir, ansi, progress string
	fs.Var(&files, "f", "")
	fs.Var(&files, "file", "")
	fs.StringVar(&name, "p", "", "")
	fs.StringVar(&name, "project-name", "", "")
	fs.StringVar(&dir, "project-directory", "", "")
	fs.Var(&profiles, "profile", "")
	fs.Var(&envFiles, "env-file", "")
	fs.StringVar(&ansi, "ansi", "auto", "")
	fs.StringVar(&progress, "progress", "", "")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, composeUsage)
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	c := &composeCtx{root: root, stdin: stdin, stdout: stdout, stderr: stderr, store: container.Open(root),
		load: compose.LoadOptions{Files: files, ProjectDir: dir, Name: name, Profiles: profiles, EnvFiles: envFiles, Warnings: stderr}}
	switch ansi {
	case "always":
		c.colour = true
	case "never":
	default:
		c.colour = vmm.IsTerminal(os.Stdout)
	}
	sub, rest := fs.Arg(0), fs.Args()[1:]
	switch sub {
	case "up":
		return c.cmdUp(rest, false)
	case "create":
		return c.cmdUp(rest, true)
	case "down":
		return c.cmdDown(rest)
	case "start", "stop", "restart", "kill", "pause", "unpause":
		return c.cmdLifecycle(sub, rest)
	case "rm":
		return c.cmdRm(rest)
	case "ps":
		return c.cmdPs(rest)
	case "logs":
		return c.cmdLogs(rest)
	case "exec":
		return c.cmdExec(rest)
	case "run":
		return c.cmdRun(rest)
	case "build":
		return c.cmdBuild(rest)
	case "pull":
		return c.cmdPull(rest)
	case "push":
		return c.cmdPush(rest)
	case "config":
		return c.cmdConfig(rest)
	case "images":
		return c.cmdImages(rest)
	case "port":
		return c.cmdPort(rest)
	case "ls":
		return c.cmdLs(rest)
	case "version":
		fmt.Fprintf(stdout, "runwhp compose version %s (compose-go)\n", compose.Version())
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, composeUsage)
		return 0
	}
	fmt.Fprintf(stderr, "runwhp: unknown compose command %q\n\n%s", sub, composeUsage)
	return 2
}

// open loads the project. Without a compose file, a command that may work
// on labels alone does, if it has a project name.
func (c *composeCtx) open(ctx context.Context, needFile bool) error {
	p, err := compose.Load(ctx, c.load)
	if err == nil {
		c.project, c.name = p, p.Name
		return nil
	}
	if !needFile && strings.Contains(err.Error(), "no configuration file provided") {
		name := c.load.Name
		if name == "" {
			name = os.Getenv("COMPOSE_PROJECT_NAME")
		}
		if name != "" {
			c.name = name
			return nil
		}
	}
	return err
}

// printErr reports an error, each of several joined on a line of its own.
func printErr(err error) {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			printErr(e)
		}
		return
	}
	errorf("%v", err)
}

// event writes a progress line, as Docker Compose writes them when its
// output is not a console: " Container app-1 Started ".
func (c *composeCtx) event(kind, name, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.stderr, " %s %s %s \n", kind, name, status)
}

// warn writes a warning, as compose-go's are written.
func (c *composeCtx) warn(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.stderr, "runwhp: compose: warning: "+format+"\n", a...)
}

// projectContainers are the project's containers, found by their labels,
// one-offs too if oneoff, by name.
func (c *composeCtx) projectContainers(oneoff bool) ([]*container.Container, error) {
	all, err := c.store.List()
	if err != nil {
		return nil, err
	}
	var out []*container.Container
	for _, ct := range all {
		l := ct.Config.Labels
		if l[compose.LabelProject] != c.name || !oneoff && l[compose.LabelOneoff] == "True" {
			continue
		}
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// serviceContainers are the containers of services, all if none is named,
// by name.
func (c *composeCtx) serviceContainers(services []string, oneoff bool) ([]*container.Container, error) {
	cs, err := c.projectContainers(oneoff)
	if err != nil || len(services) == 0 {
		return cs, err
	}
	var out []*container.Container
	for _, ct := range cs {
		if slices.Contains(services, serviceOf(ct)) {
			out = append(out, ct)
		}
	}
	return out, nil
}

// checkServices refuses services the project does not have.
func (c *composeCtx) checkServices(names []string) error {
	if c.project == nil {
		return nil
	}
	for _, n := range names {
		if _, ok := c.project.Services[n]; !ok {
			return fmt.Errorf("no such service: %s", n)
		}
	}
	return nil
}

func serviceOf(ct *container.Container) string { return ct.Config.Labels[compose.LabelService] }

func numberOf(ct *container.Container) int {
	n, _ := strconv.Atoi(ct.Config.Labels[compose.LabelNumber])
	return n
}

// shortName is a container's name less its project's, as Docker Compose
// prefixes its output with it: web-1.
func (c *composeCtx) shortName(ct *container.Container) string {
	return strings.TrimPrefix(ct.Name, c.name+"-")
}

// composeFlags makes a compose command's flag set.
func composeFlags(name, usage string) *flag.FlagSet { return newFlags("compose "+name, usage) }

// interruptibleTwice returns a context that ends on Ctrl+C, a channel that
// is closed on a second, and the stop.
func interruptibleTwice() (context.Context, <-chan struct{}, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
			return
		}
		select {
		case <-sigs:
			close(second)
		case <-time.After(24 * time.Hour):
		}
	}()
	return ctx, second, func() { signal.Stop(sigs); cancel() }
}

func (c *composeCtx) cmdPs(args []string) int {
	fs := composeFlags("ps", `Usage: runwhp compose ps [-a] [-q] [--services] [--status STATUS]... [--format table|json] [SERVICE...]

List the project's containers: running ones, or all with -a.
`)
	all := fs.Bool("a", false, "")
	fs.BoolVar(all, "all", false, "")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	services := fs.Bool("services", false, "")
	var statuses listFlag
	fs.Var(&statuses, "status", "")
	format := fs.String("format", "table", "")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx := context.Background()
	if err := c.open(ctx, false); err != nil {
		errorf("%v", err)
		return 1
	}
	if err := c.checkServices(fs.Args()); err != nil {
		errorf("%v", err)
		return 1
	}
	cs, err := c.serviceContainers(fs.Args(), true)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	var shown []*container.Container
	for _, ct := range cs {
		word := stateWord(ct.State)
		switch {
		case len(statuses) > 0 && !slices.Contains(statuses, word):
		case len(statuses) == 0 && !*all && word != "running" && word != "paused" && word != "restarting":
		default:
			shown = append(shown, ct)
		}
	}
	now := time.Now()
	switch {
	case *services:
		var names []string
		for _, ct := range shown {
			if s := serviceOf(ct); !slices.Contains(names, s) {
				names = append(names, s)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintln(c.stdout, n)
		}
	case *quiet:
		for _, ct := range shown {
			fmt.Fprintln(c.stdout, ct.ID)
		}
	case *format == "json":
		enc := json.NewEncoder(c.stdout)
		enc.SetEscapeHTML(false)
		for _, ct := range shown {
			enc.Encode(c.psJSON(ct, now))
		}
	default:
		tw := tabwriter.NewWriter(c.stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "NAME\tIMAGE\tCOMMAND\tSERVICE\tCREATED\tSTATUS\tPORTS")
		for _, ct := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ct.Name, imageOf(ct.Config), commandOf(ct.Config, 20), serviceOf(ct),
				ago(now, ct.Config.Created), statusText(ct.State, now), portsOf(ct))
		}
		tw.Flush()
	}
	return 0
}

// psJSON is a container as compose ps --format json describes it.
func (c *composeCtx) psJSON(ct *container.Container, now time.Time) map[string]any {
	var labels, networks, mounts []string
	for _, k := range slices.Sorted(maps.Keys(ct.Config.Labels)) {
		labels = append(labels, k+"="+ct.Config.Labels[k])
	}
	for _, a := range ct.Config.Networks {
		networks = append(networks, a.Name)
	}
	for _, v := range ct.Config.Volumes {
		mounts = append(mounts, v.Name)
	}
	publishers := []map[string]any{}
	for _, p := range ct.Config.Publish {
		publishers = append(publishers, map[string]any{"URL": p.Host.Addr().String(), "TargetPort": p.Guest, "PublishedPort": p.Host.Port(), "Protocol": "tcp"})
	}
	health := ""
	if ct.State.Health != nil {
		health = ct.State.Health.Status
	}
	return map[string]any{
		"Command": commandOf(ct.Config, 20), "CreatedAt": ct.Config.Created.Local().Format("2006-01-02 15:04:05 -0700 MST"),
		"ExitCode": ct.State.ExitCode, "Health": health, "ID": ct.Short(), "Image": imageOf(ct.Config),
		"Labels": strings.Join(labels, ","), "LocalVolumes": strconv.Itoa(len(ct.Config.Volumes)), "Mounts": strings.Join(mounts, ","),
		"Name": ct.Name, "Names": ct.Name, "Networks": strings.Join(networks, ","), "Ports": portsOf(ct), "Project": c.name,
		"Publishers": publishers, "RunningFor": ago(now, ct.Config.Created), "Service": serviceOf(ct), "Size": "0B",
		"State": stateWord(ct.State), "Status": statusText(ct.State, now),
	}
}

func (c *composeCtx) cmdLs(args []string) int {
	fs := composeFlags("ls", `Usage: runwhp compose ls [-a] [-q] [--format table|json]

List projects: those with running containers, or all with -a.
`)
	all := fs.Bool("a", false, "")
	fs.BoolVar(all, "all", false, "")
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	format := fs.String("format", "table", "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	cs, err := c.store.List()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	type project struct {
		Name, Status, ConfigFiles string
		counts                    map[string]int
	}
	byName := map[string]*project{}
	for _, ct := range cs {
		name := ct.Config.Labels[compose.LabelProject]
		if name == "" {
			continue
		}
		p := byName[name]
		if p == nil {
			p = &project{Name: name, ConfigFiles: ct.Config.Labels[compose.LabelConfigFiles], counts: map[string]int{}}
			byName[name] = p
		}
		p.counts[stateWord(ct.State)]++
	}
	var list []project
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		p := byName[name]
		if !*all && p.counts["running"] == 0 {
			continue
		}
		var parts []string
		for _, w := range slices.Sorted(maps.Keys(p.counts)) {
			parts = append(parts, fmt.Sprintf("%s(%d)", w, p.counts[w]))
		}
		p.Status = strings.Join(parts, ", ")
		list = append(list, *p)
	}
	switch {
	case *quiet:
		for _, p := range list {
			fmt.Fprintln(c.stdout, p.Name)
		}
	case *format == "json":
		out := []map[string]string{}
		for _, p := range list {
			out = append(out, map[string]string{"Name": p.Name, "Status": p.Status, "ConfigFiles": p.ConfigFiles})
		}
		data, _ := json.Marshal(out)
		fmt.Fprintln(c.stdout, string(data))
	default:
		tw := tabwriter.NewWriter(c.stdout, 20, 1, 3, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATUS\tCONFIG FILES")
		for _, p := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Status, p.ConfigFiles)
		}
		tw.Flush()
	}
	return 0
}

func (c *composeCtx) cmdConfig(args []string) int {
	fs := composeFlags("config", `Usage: runwhp compose config [-q] [--services] [--volumes] [--networks] [--images] [--format yaml|json]

Print the project as it is read: interpolated, merged and normalized.
`)
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	services := fs.Bool("services", false, "")
	volumes := fs.Bool("volumes", false, "")
	networks := fs.Bool("networks", false, "")
	images := fs.Bool("images", false, "")
	format := fs.String("format", "yaml", "")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := c.open(context.Background(), true); err != nil {
		errorf("%v", err)
		return 1
	}
	p := c.project
	switch {
	case *quiet:
	case *services:
		for _, n := range dependencyOrder(p) {
			fmt.Fprintln(c.stdout, n)
		}
	case *volumes:
		for _, n := range slices.Sorted(maps.Keys(p.Volumes)) {
			fmt.Fprintln(c.stdout, n)
		}
	case *networks:
		for _, n := range slices.Sorted(maps.Keys(p.Networks)) {
			fmt.Fprintln(c.stdout, n)
		}
	case *images:
		for _, n := range dependencyOrder(p) {
			fmt.Fprintln(c.stdout, compose.ImageName(p.Name, p.Services[n]))
		}
	default:
		var data []byte
		var err error
		if *format == "json" {
			data, err = p.MarshalJSON()
		} else {
			data, err = p.MarshalYAML()
		}
		if err != nil {
			errorf("%v", err)
			return 1
		}
		c.stdout.Write(data)
	}
	return 0
}

// dependencyOrder is the project's services, each after what it depends
// on, by name otherwise, as config --services lists them.
func dependencyOrder(p *types.Project) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		s, ok := p.Services[n]
		if !ok {
			return
		}
		for _, d := range slices.Sorted(maps.Keys(s.DependsOn)) {
			visit(d)
		}
		out = append(out, n)
	}
	for _, n := range slices.Sorted(maps.Keys(p.Services)) {
		visit(n)
	}
	return out
}

func (c *composeCtx) cmdImages(args []string) int {
	fs := composeFlags("images", `Usage: runwhp compose images [-q] [SERVICE...]

List the images the project's containers use.
`)
	quiet := fs.Bool("q", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := c.open(context.Background(), false); err != nil {
		errorf("%v", err)
		return 1
	}
	cs, err := c.serviceContainers(fs.Args(), true)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	sizes := map[string]int64{}
	if _, u, ok := loadStore(); ok {
		for _, img := range u.Images {
			sizes[img.ID()] = img.Size
		}
	}
	tw := tabwriter.NewWriter(c.stdout, 20, 1, 3, ' ', 0)
	if !*quiet {
		fmt.Fprintln(tw, "CONTAINER\tREPOSITORY\tTAG\tIMAGE ID\tSIZE")
	}
	for _, ct := range cs {
		id := strings.TrimPrefix(ct.Config.ImageID, "sha256:")
		if *quiet {
			fmt.Fprintln(tw, id)
			continue
		}
		repo, tag := "<none>", "<none>"
		if ct.Config.ImageName != "" {
			repo, tag = image.Display(ct.Config.ImageName)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.12s\t%s\n", ct.Name, repo, tag, id, humanSize(sizes[ct.Config.ImageID]))
	}
	tw.Flush()
	return 0
}

func (c *composeCtx) cmdPort(args []string) int {
	fs := composeFlags("port", `Usage: runwhp compose port [--index N] SERVICE PRIVATE_PORT

Print the host address a container's port is published on.
`)
	index := fs.Int("index", 1, "")
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	if err := c.open(context.Background(), false); err != nil {
		errorf("%v", err)
		return 1
	}
	service, private := fs.Arg(0), fs.Arg(1)
	ct, err := c.running(service, *index)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	port, _, _ := strings.Cut(private, "/")
	var listed []string
	for _, p := range ct.Config.Publish {
		if strconv.Itoa(int(p.Guest)) == port {
			fmt.Fprintln(c.stdout, p.Host.String())
			return 0
		}
		listed = append(listed, fmt.Sprintf("%d/tcp", p.Guest))
	}
	errorf("no port %s/tcp for container %s: %s", port, ct.Name, strings.Join(listed, ", "))
	return 1
}

// running is a service's running container number index.
func (c *composeCtx) running(service string, index int) (*container.Container, error) {
	cs, err := c.serviceContainers([]string{service}, false)
	if err != nil {
		return nil, err
	}
	for _, ct := range cs {
		if numberOf(ct) == index && ct.State.Status == container.Running {
			return ct, nil
		}
	}
	return nil, fmt.Errorf("service %q is not running", service)
}
