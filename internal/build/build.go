// Package build is runwhp's Dockerfile builder: it runs a Dockerfile as
// Docker 29's default builder, BuildKit, does, as far as the images it
// makes go, but runs each step that changes files in a guest of its own,
// as Docker's classic builder ran each in a container.
//
// The Dockerfile is parsed by BuildKit's own parser, and dispatched as
// BuildKit's dockerfile2llb dispatches it (github.com/moby/buildkit
// v0.35.0, Apache-2.0, The BuildKit Authors): the same stages, argument
// scopes, configuration changes and history. A step that changes files,
// RUN, COPY or ADD, goes to a Worker, which boots a guest on the step's
// layers, puts files into it or runs the command, and reads the changes
// back as a layer. Everything else changes the configuration only.
//
// It departs from BuildKit in two ways. WORKDIR makes no layer of its
// own: the directory is made by the stage's next step that boots a guest,
// or by one more step at the stage's end, and its history entry is empty,
// as the classic builder's was. And the cache is a chain of keys, each
// step's from its parent's and what it was given, kept in
// internal/buildcache, rather than BuildKit's content-addressed graph.
package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/rootfs"
)

// historyComment marks history entries as BuildKit's Dockerfile frontend
// marks them.
const historyComment = "buildkit.dockerfile.v0"

// Platform is the only platform runwhp builds for.
var Platform = ocispecs.Platform{OS: "linux", Architecture: "amd64"}

// DefaultPath is the PATH an image without one gets, as BuildKit gives it.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Options say what to build.
type Options struct {
	Dockerfile []byte
	// DockerfileName names the Dockerfile in messages.
	DockerfileName string
	// Context is the build context; nil for a build without one.
	Context *Context
	// BuildArgs are --build-arg's values.
	BuildArgs map[string]string
	Target    string
	NoCache   bool
	// Labels are --label's, set on the image built.
	Labels map[string]string
	// NoNetwork leaves RUN steps without a network, as --network none.
	NoNetwork bool
	// Hosts are --add-host's NAME:IP entries.
	Hosts []string
	// Secrets are --secret's, by id.
	Secrets map[string]Secret
	// Progress, if set, prints BuildKit's plain progress.
	Progress *Progress
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// TempDir is where a build spools files; the system's if empty.
	TempDir string
}

// Secret is a RUN --mount=type=secret's source: a file on the host.
type Secret struct {
	Path string
}

// Result is the image built.
type Result struct {
	// Config is its configuration blob, and ID its digest.
	Config []byte
	ID     string
	// Warnings are the linter's, as Docker prints them.
	Warnings []string
}

// Layer is what a step left: a layer by diff ID, or nothing.
type Layer struct {
	DiffID string
	Empty  bool
}

// Root is a guest's root, as init's file server serves it.
type Root interface {
	Stat(ctx context.Context, p string) (*guestspec.PathStat, error)
	Glob(ctx context.Context, pattern string) ([]string, error)
	Get(ctx context.Context, p string) (io.ReadCloser, *guestspec.PathStat, error)
	Put(ctx context.Context, dir string, content io.Reader, o rootfs.PutOptions) error
}

// SourceRoot is a root opened to copy files from.
type SourceRoot interface {
	Root
	Close() error
}

// Step is a step that boots a guest on Layers: Fill puts files into its
// root, and Run, if set, then runs its process. The step's layer is what
// they changed.
type Step struct {
	Layers []string
	Fill   func(ctx context.Context, r Root) error
	Run    *Process
	// Build, if set, makes the layer on the host instead, with no guest:
	// COPY --link's, which does not depend on the layers below it.
	Build func(ctx context.Context) (Layer, error)
	// Prepare, if set, runs first, before the step's guest boots: a
	// process runs one guest at a time, so COPY --from reads its source's
	// guest here.
	Prepare func(ctx context.Context) error
}

// Process is a RUN step's.
type Process struct {
	Args, Env []string
	Cwd, User string
	Hostname  string
	Network   bool
	Hosts     []string
	Mounts    []Mount
	Output    io.Writer
}

// Mount is one of a RUN step's mounts.
type Mount struct {
	// Kind is "cache", "tmpfs", "bind" or "secret".
	Kind   string
	Target string
	// Source is a bind's or a secret's file or directory on the host.
	Source string
	// ID is a cache mount's volume.
	ID       string
	ReadOnly bool
	// UID, GID and Mode set a new cache volume's owner and mode, or a
	// secret's.
	UID, GID *uint64
	Mode     *uint64
}

// ExitError is a RUN step's command exiting with a code other than 0.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit code: %d", e.Code) }

// Worker does what needs a guest or the image store.
type Worker interface {
	// Image returns the ID and configuration blob of the image ref names,
	// pulling it as the build's pull policy says.
	Image(ctx context.Context, ref string) (id string, config []byte, err error)
	// HasLayer reports whether the store holds the layer diffID.
	HasLayer(diffID string) bool
	// Step runs s, and returns the layer it made.
	Step(ctx context.Context, s *Step) (Layer, error)
	// Open boots a root of layers, read-only, to copy files from.
	Open(ctx context.Context, layers []string) (SourceRoot, error)
	// Import stores a layer's tar the build made itself.
	Import(r io.Reader) (Layer, error)
}

// Cache is where a build finds and keeps its steps' results.
type Cache interface {
	Get(key string) (buildcache.Entry, bool)
	Put(e buildcache.Entry) error
}

// builder is one build's state.
type builder struct {
	o        Options
	w        Worker
	cache    Cache
	now      func() time.Time
	shlex    *shell.Lex
	lint     *linter.Linter
	warnings []string
	global   *envList
	stages   []*stage
	progress *Progress
	epoch    *time.Time
	proxy    []string // the proxy arguments' environment, kept out of history
	// usedArgs are the build arguments some ARG declared.
	usedArgs map[string]bool
}

// stage is one stage's state as the build goes.
type stage struct {
	inst  instructions.Stage
	index int
	name  string // as progress shows it; empty in a one-stage build
	base  *stage // the stage FROM names, if any
	img   dockerspec.DockerOCIImage
	// baseHistory is how much of img's history is its base image's.
	baseHistory int
	key         string   // the state's cache key
	env         *envList // the state's environment: the image's and ARGs'
	args        []instructions.KeyValuePairOptional
	commands    []command
	workdirSet  bool
	cmdSet      bool
	pending     []pendingDir // WORKDIRs not made yet
	dispatched  bool
	reachable   bool
	cmdIndex    int
	cmdTotal    int
	// imageID is the base image's ID, for a stage FROM an image.
	imageID string
}

// command is an instruction to dispatch, with the stage it copies from.
type command struct {
	instructions.Command
	from      *stage
	fromImage string // an image COPY --from names
	onBuild   bool
}

// pendingDir is a WORKDIR still to make, as the user that set it.
type pendingDir struct {
	path, user string
}

// Build builds the image o describes.
func Build(ctx context.Context, w Worker, cache Cache, o Options) (*Result, error) {
	b := &builder{o: o, w: w, cache: cache, now: o.Now, usedArgs: map[string]bool{}}
	if b.now == nil {
		b.now = time.Now
	}
	b.progress = o.Progress
	if b.progress == nil {
		b.progress = NewProgress(nil, b.now)
	}
	if len(o.Dockerfile) == 0 {
		return nil, errors.New("the Dockerfile cannot be empty")
	}
	b.lint = linter.New(&linter.Config{Warn: func(rule, description, url, msg string, loc []parser.Range) {
		line := 0
		if len(loc) > 0 {
			line = loc[0].Start.Line
		}
		b.warnings = append(b.warnings, fmt.Sprintf("%s: %s (line %d)", rule, msg, line))
	}})
	res, err := parser.Parse(bytes.NewReader(o.Dockerfile))
	if err != nil {
		return nil, b.located(err)
	}
	b.shlex = shell.NewLex(res.EscapeToken)
	stages, metaArgs, err := instructions.Parse(res.AST, b.lint)
	if err != nil {
		return nil, b.located(err)
	}
	if len(stages) == 0 {
		return nil, errors.New("dockerfile contains no stages to build")
	}
	if err := b.args(metaArgs); err != nil {
		return nil, b.located(err)
	}
	target, err := b.plan(stages)
	if err != nil {
		return nil, b.located(err)
	}
	if err := b.resolve(ctx, target); err != nil {
		return nil, b.located(err)
	}
	for _, s := range b.stages {
		if !s.reachable {
			continue
		}
		if err := b.dispatchStage(ctx, s); err != nil {
			return nil, b.located(err)
		}
	}
	raw, err := b.finish(target)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &Result{Config: raw, ID: "sha256:" + hex.EncodeToString(sum[:]), Warnings: b.warnings}, nil
}

// located words err with the Dockerfile's lines, as Docker does.
func (b *builder) located(err error) error {
	var le *parser.LocationError
	if !errors.As(err, &le) || len(le.Locations) == 0 || len(le.Locations[0]) == 0 {
		return err
	}
	name := b.o.DockerfileName
	if name == "" {
		name = "Dockerfile"
	}
	r := le.Locations[0][0]
	return &LocatedError{Err: err, File: name, Line: r.Start.Line, Source: b.o.Dockerfile}
}

// LocatedError is an error at a line of the Dockerfile.
type LocatedError struct {
	Err    error
	File   string
	Line   int
	Source []byte
}

func (e *LocatedError) Error() string { return e.Err.Error() }
func (e *LocatedError) Unwrap() error { return e.Err }

// Excerpt is the Dockerfile around the error's line, as Docker prints it.
func (e *LocatedError) Excerpt() string {
	lines := strings.Split(strings.ReplaceAll(string(e.Source), "\r\n", "\n"), "\n")
	var out strings.Builder
	fmt.Fprintf(&out, "%s:%d\n--------------------\n", e.File, e.Line)
	for i := max(e.Line-3, 1); i <= min(e.Line+2, len(lines)); i++ {
		mark := "   "
		if i == e.Line {
			mark = ">>>"
		}
		fmt.Fprintf(&out, " %3d | %s %s\n", i, mark, lines[i-1])
	}
	out.WriteString("--------------------")
	return out.String()
}

// args sets the global arguments: the platform's, then the ARGs before the
// first FROM, with --build-arg's values.
func (b *builder) args(metaArgs []instructions.ArgCommand) error {
	g := &envList{}
	for _, kv := range [][2]string{
		{"BUILDPLATFORM", "linux/amd64"}, {"BUILDOS", "linux"}, {"BUILDOSVERSION", ""}, {"BUILDARCH", "amd64"}, {"BUILDVARIANT", ""},
		{"TARGETPLATFORM", "linux/amd64"}, {"TARGETOS", "linux"}, {"TARGETOSVERSION", ""}, {"TARGETARCH", "amd64"}, {"TARGETVARIANT", ""},
		{"TARGETSTAGE", cmpOr(b.o.Target, "default")},
	} {
		v := kv[1]
		if ov, ok := b.o.BuildArgs[kv[0]]; ok {
			v = ov
		}
		g.set(kv[0], v)
	}
	for _, cmd := range metaArgs {
		for _, kp := range cmd.Args {
			if v, ok := b.o.BuildArgs[kp.Key]; ok {
				g.set(kp.Key, v)
				b.usedArgs[kp.Key] = true
				continue
			}
			if kp.Value != nil {
				v, _, err := b.shlex.ProcessWord(*kp.Value, g)
				if err != nil {
					return parser.WithLocation(err, cmd.Location())
				}
				g.set(kp.Key, v)
			}
		}
	}
	b.global = g
	if v, ok := b.buildArg("SOURCE_DATE_EPOCH"); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid SOURCE_DATE_EPOCH: %s", v)
		}
		t := time.Unix(n, 0).UTC()
		b.epoch = &t
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY"} {
		for ak, v := range b.o.BuildArgs {
			if strings.EqualFold(ak, k) {
				b.proxy = append(b.proxy, k+"="+v, strings.ToLower(k)+"="+v)
			}
		}
	}
	slices.Sort(b.proxy)
	return nil
}

// buildArg is a build argument's value: --build-arg's, else a global ARG's.
func (b *builder) buildArg(key string) (string, bool) {
	if v, ok := b.o.BuildArgs[key]; ok {
		return v, true
	}
	v, ok := b.global.Get(key)
	return v, ok && v != ""
}

// plan makes a stage of each FROM, finds the target and each COPY --from's
// source, and marks the stages the target needs.
func (b *builder) plan(stages []instructions.Stage) (*stage, error) {
	for i, st := range stages {
		name, err := b.expand(st.BaseName, b.global)
		if err != nil {
			return nil, parser.WithLocation(err, st.Location)
		}
		if name == "" {
			return nil, parser.WithLocation(fmt.Errorf("base name (%s) should not be blank", st.BaseName), st.Location)
		}
		if st.Platform != "" {
			p, err := b.expand(st.Platform, b.global)
			if err != nil {
				return nil, parser.WithLocation(err, st.Location)
			}
			if p != "linux/amd64" && p != "linux/x86_64" {
				return nil, parser.WithLocation(fmt.Errorf("platform %s is not supported: runwhp builds linux/amd64 only", p), st.Location)
			}
		}
		st.BaseName = name
		s := &stage{inst: st, index: i, name: st.Name}
		if st.Name == "" {
			s.name = fmt.Sprintf("stage-%d", i)
		}
		if base, ok := b.stageNamed(name); ok {
			s.base = base
		}
		b.stages = append(b.stages, s)
	}
	if len(b.stages) == 1 {
		b.stages[0].name = ""
	}
	for _, s := range b.stages {
		for _, ic := range s.inst.Commands {
			cmd, err := b.command(ic, s)
			if err != nil {
				return nil, parser.WithLocation(err, ic.Location())
			}
			s.commands = append(s.commands, cmd)
		}
	}
	target := b.stages[len(b.stages)-1]
	if b.o.Target != "" {
		t, ok := b.stageNamed(b.o.Target)
		if !ok {
			return nil, fmt.Errorf("target stage %q could not be found", b.o.Target)
		}
		target = t
	}
	b.mark(target)
	return target, nil
}

// command prepares an instruction of stage s: a COPY --from's source.
func (b *builder) command(ic instructions.Command, s *stage) (command, error) {
	cmd := command{Command: ic}
	if c, ok := ic.(*instructions.CopyCommand); ok && c.From != "" {
		if strings.Contains(c.From, "$") {
			return cmd, errors.New("variable expansion is not supported for --from, define a new stage with FROM using ARG from global scope as a workaround")
		}
		if n, err := strconv.Atoi(c.From); err == nil {
			if n < 0 || n >= len(b.stages) || b.stages[n].index >= s.index {
				return cmd, fmt.Errorf("invalid stage index %d", n)
			}
			cmd.from = b.stages[n]
		} else if from, ok := b.stageNamed(c.From); ok {
			if from.index >= s.index {
				return cmd, fmt.Errorf("cannot copy from stage %q, it needs to be defined before current stage %q", c.From, s.inst.Name)
			}
			cmd.from = from
		} else {
			cmd.fromImage = c.From
		}
	}
	if c, ok := ic.(*instructions.RunCommand); ok {
		for _, m := range instructions.GetMounts(c) {
			if m.From != "" {
				return cmd, errors.New("RUN --mount with from= is not supported by runwhp build")
			}
		}
	}
	return cmd, nil
}

// stageNamed finds a stage by its name, case-insensitive, as BuildKit does.
func (b *builder) stageNamed(name string) (*stage, bool) {
	for _, s := range b.stages {
		if s.inst.Name != "" && strings.EqualFold(s.inst.Name, name) {
			return s, true
		}
	}
	return nil, false
}

// mark marks s, and every stage it needs, reachable.
func (b *builder) mark(s *stage) {
	if s.reachable {
		return
	}
	s.reachable = true
	if s.base != nil {
		b.mark(s.base)
	}
	for _, c := range s.commands {
		if c.from != nil {
			b.mark(c.from)
		}
	}
}

// resolve finds the reachable stages' base images, and the ONBUILD
// triggers they bring.
func (b *builder) resolve(ctx context.Context, target *stage) error {
	for {
		added := false
		for _, s := range b.stages {
			if !s.reachable || s.dispatched || s.imageID != "" || s.base != nil || s.key != "" {
				continue
			}
			if s.inst.BaseName == "scratch" {
				s.img = emptyImage()
				s.key = digestString("scratch")
				continue
			}
			id, raw, err := b.w.Image(ctx, s.inst.BaseName)
			if err != nil {
				return parser.WithLocation(err, s.inst.Location)
			}
			var img dockerspec.DockerOCIImage
			if err := json.Unmarshal(raw, &img); err != nil {
				return parser.WithLocation(fmt.Errorf("the configuration of %s: %w", s.inst.BaseName, err), s.inst.Location)
			}
			img.Created = nil
			s.img, s.imageID, s.key = img, id, digestString("image", id)
			s.baseHistory = len(img.History)
			if len(img.Config.OnBuild) > 0 {
				n, err := b.onBuild(s, img.Config.OnBuild)
				if err != nil {
					return parser.WithLocation(err, s.inst.Location)
				}
				s.img.Config.OnBuild = nil
				added = added || n
			}
		}
		if !added {
			return nil
		}
		// Triggers may copy from stages not reachable before.
		for _, s := range b.stages {
			s.reachable = false
		}
		b.mark(target)
	}
}

// onBuild puts the triggers of s's base image before its commands, and
// reports whether any copies from another stage.
func (b *builder) onBuild(s *stage, triggers []string) (bool, error) {
	var cmds []command
	newDeps := false
	for _, t := range triggers {
		ast, err := parser.Parse(strings.NewReader(t))
		if err != nil {
			return false, err
		}
		if len(ast.AST.Children) != 1 {
			return false, errors.New("onbuild trigger should be a single expression")
		}
		node := ast.AST.Children[0]
		if len(s.inst.Location) > 0 {
			node.StartLine, node.EndLine = s.inst.Location[0].Start.Line, s.inst.Location[0].End.Line
		}
		ic, err := instructions.ParseCommand(node)
		if err != nil {
			return false, err
		}
		cmd, err := b.command(ic, s)
		if err != nil {
			return false, err
		}
		cmd.onBuild = true
		newDeps = newDeps || cmd.from != nil
		cmds = append(cmds, cmd)
	}
	s.commands = append(cmds, s.commands...)
	return newDeps, nil
}

// emptyImage is scratch's configuration, as BuildKit makes it.
func emptyImage() dockerspec.DockerOCIImage {
	var img dockerspec.DockerOCIImage
	img.OS, img.Architecture = Platform.OS, Platform.Architecture
	img.RootFS.Type = "layers"
	img.Config.WorkingDir = "/"
	img.Config.Env = []string{"PATH=" + DefaultPath}
	return img
}

// expand expands word's variables from env, as BuildKit does.
func (b *builder) expand(word string, env shell.EnvGetter) (string, error) {
	w, _, err := b.shlex.ProcessWord(word, env)
	return w, err
}

// dispatchStage runs the commands of s, once its base is built.
func (b *builder) dispatchStage(ctx context.Context, s *stage) error {
	if s.dispatched {
		return nil
	}
	s.dispatched = true
	if s.base != nil {
		base := s.base
		s.img = cloneImage(base.img)
		s.key = base.key
		s.baseHistory = len(base.img.History)
		s.imageID = base.imageID
	}
	// The steps progress counts: FROM an image, and each step that
	// changes files.
	if s.base == nil && s.inst.BaseName != "scratch" {
		s.cmdTotal = 1
	}
	for _, c := range s.commands {
		switch c.Command.(type) {
		case *instructions.AddCommand, *instructions.CopyCommand, *instructions.RunCommand, *instructions.WorkdirCommand:
			s.cmdTotal++
		}
	}
	if s.base == nil && s.inst.BaseName != "scratch" {
		b.progress.cached(b.prefix(s, "FROM "+s.inst.BaseName))
	}
	if _, ok := envOf(s.img.Config.Env).Get("PATH"); !ok {
		s.img.Config.Env = append(s.img.Config.Env, "PATH="+DefaultPath)
	}
	s.env = envOf(s.img.Config.Env)
	for _, c := range s.commands {
		if err := b.dispatch(ctx, s, c); err != nil {
			return parser.WithLocation(err, c.Location())
		}
	}
	if len(s.pending) > 0 {
		// The last WORKDIRs, with no step after them to make them: the
		// layer is the last one's, whose history entry it takes, every
		// entry after it being empty.
		msg := "WORKDIR " + s.pending[len(s.pending)-1].path
		n := len(s.img.History)
		if _, err := b.layerStep(ctx, s, msg, nil, nil); err != nil {
			return err
		}
		added := s.img.History[len(s.img.History)-1]
		s.img.History = s.img.History[:len(s.img.History)-1]
		if !added.EmptyLayer {
			found := false
			for i := n - 1; i >= 0 && !found; i-- {
				if h := &s.img.History[i]; h.CreatedBy == msg && h.EmptyLayer {
					h.EmptyLayer, h.Created, found = false, added.Created, true
				}
			}
			if !found {
				s.img.History = append(s.img.History, added)
			}
		}
	}
	return nil
}

// prefix is the step's name in progress: [stage i/n] INSTR.
func (b *builder) prefix(s *stage, str string) string {
	if s.cmdTotal == 0 {
		return str
	}
	s.cmdIndex++
	out := "["
	if s.name != "" {
		out += s.name + " "
	}
	width := len(strconv.Itoa(s.cmdTotal))
	return out + fmt.Sprintf("%*d/%d] ", width, s.cmdIndex, s.cmdTotal) + str
}

// dispatch runs one command of s.
func (b *builder) dispatch(ctx context.Context, s *stage, cmd command) error {
	if ex, ok := cmd.Command.(instructions.SupportsSingleWordExpansion); ok {
		if _, isArg := cmd.Command.(*instructions.ArgCommand); !isArg {
			if err := ex.Expand(func(word string) (string, error) { return b.expand(word, s.env) }); err != nil {
				return err
			}
		}
	}
	if ex, ok := cmd.Command.(instructions.SupportsSingleWordExpansionRaw); ok {
		lex := shell.NewLex('\\')
		lex.SkipProcessQuotes = true
		if err := ex.ExpandRaw(func(word string) (string, error) {
			w, _, err := lex.ProcessWord(word, s.env)
			return w, err
		}); err != nil {
			return err
		}
	}
	img := &s.img
	switch c := cmd.Command.(type) {
	case *instructions.MaintainerCommand:
		img.Author = c.Maintainer
		b.history(s, "MAINTAINER "+c.Maintainer)
	case *instructions.EnvCommand:
		if c.Unset {
			var keys []string
			for _, e := range c.Env {
				if e.Key == "" {
					return errors.New("ENV --unset names can not be blank")
				}
				s.env.del(e.Key)
				img.Config.Env = delEnv(img.Config.Env, e.Key)
				s.args = slices.DeleteFunc(s.args, func(a instructions.KeyValuePairOptional) bool { return shell.EqualEnvKeys(a.Key, e.Key) })
				keys = append(keys, e.Key)
			}
			b.history(s, "ENV --unset "+strings.Join(keys, " "))
			return nil
		}
		msg := "ENV"
		for _, e := range c.Env {
			msg += " " + e.String()
			s.env.set(e.Key, e.Value)
			img.Config.Env = addEnv(img.Config.Env, e.Key, e.Value)
		}
		b.history(s, msg)
	case *instructions.LabelCommand:
		if c.Unset {
			var keys []string
			for _, l := range c.Labels {
				delete(img.Config.Labels, l.Key)
				keys = append(keys, l.Key)
			}
			b.history(s, "LABEL --unset "+strings.Join(keys, " "))
			return nil
		}
		if img.Config.Labels == nil {
			img.Config.Labels = map[string]string{}
		}
		msg := "LABEL"
		for _, l := range c.Labels {
			img.Config.Labels[l.Key] = l.Value
			msg += " " + l.String()
		}
		b.history(s, msg)
	case *instructions.OnbuildCommand:
		img.Config.OnBuild = append(img.Config.OnBuild, c.Expression)
	case *instructions.CmdCommand:
		args := c.CmdLine
		if c.PrependShell {
			args = withShell(*img, args)
		}
		img.Config.Cmd = args
		img.Config.ArgsEscaped = true //nolint:staticcheck // as BuildKit sets it
		s.cmdSet = true
		b.history(s, fmt.Sprintf("CMD %q", args))
	case *instructions.EntrypointCommand:
		args := c.CmdLine
		if c.PrependShell {
			args = withShell(*img, args)
		}
		img.Config.Entrypoint = args
		if !s.cmdSet {
			img.Config.Cmd = nil
		}
		b.history(s, fmt.Sprintf("ENTRYPOINT %q", args))
	case *instructions.HealthCheckCommand:
		h := *c.Health
		img.Config.Healthcheck = &h
		b.history(s, fmt.Sprintf("HEALTHCHECK %+v", h))
	case *instructions.ExposeCommand:
		var ports []string
		for _, p := range c.Ports {
			words, err := b.shlex.ProcessWords(p, s.env)
			if err != nil {
				return err
			}
			for _, w := range words {
				ps, err := exposed(w)
				if err != nil {
					return err
				}
				ports = append(ports, ps...)
			}
		}
		if img.Config.ExposedPorts == nil {
			img.Config.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range ports {
			img.Config.ExposedPorts[p] = struct{}{}
		}
		b.history(s, "EXPOSE "+strings.Join(ports, " "))
	case *instructions.UserCommand:
		img.Config.User = c.User
		b.history(s, "USER "+c.User)
	case *instructions.VolumeCommand:
		if slices.Contains(c.Volumes, "") {
			return errors.New("VOLUME specified can not be an empty string")
		}
		if c.Unset {
			for _, v := range c.Volumes {
				for k := range img.Config.Volumes {
					if path.Clean(k) == path.Clean(v) {
						delete(img.Config.Volumes, k)
					}
				}
			}
			b.history(s, fmt.Sprintf("VOLUME --unset %v", c.Volumes))
			return nil
		}
		if img.Config.Volumes == nil {
			img.Config.Volumes = map[string]struct{}{}
		}
		for _, v := range c.Volumes {
			img.Config.Volumes[v] = struct{}{}
		}
		b.history(s, fmt.Sprintf("VOLUME %v", c.Volumes))
	case *instructions.StopSignalCommand:
		if _, err := guestspec.ParseSignal(c.Signal); err != nil {
			return err
		}
		img.Config.StopSignal = c.Signal
		b.history(s, "STOPSIGNAL "+c.Signal)
	case *instructions.ShellCommand:
		img.Config.Shell = c.Shell
		b.history(s, fmt.Sprintf("SHELL %v", c.Shell))
	case *instructions.ArgCommand:
		return b.dispatchArg(s, c)
	case *instructions.WorkdirCommand:
		return b.dispatchWorkdir(s, c)
	case *instructions.RunCommand:
		return b.dispatchRun(ctx, s, c, cmd.onBuild)
	case *instructions.CopyCommand:
		return b.dispatchCopy(ctx, s, copyCmd{
			params: c.SourcesAndDest, from: cmd.from, fromImage: cmd.fromImage, chown: c.Chown, chmod: c.Chmod,
			link: c.Link, exclude: c.ExcludePatterns, parents: c.Parents, code: c.String(), onBuild: cmd.onBuild,
		})
	case *instructions.AddCommand:
		if c.KeepGitDir != nil {
			return errors.New("ADD --keep-git-dir is not supported by runwhp build: it fetches no git sources")
		}
		unpack := true
		if c.Unpack != nil {
			unpack = *c.Unpack
		}
		return b.dispatchCopy(ctx, s, copyCmd{
			params: c.SourcesAndDest, add: true, chown: c.Chown, chmod: c.Chmod, link: c.Link,
			exclude: c.ExcludePatterns, checksum: c.Checksum, unpack: unpack, code: c.String(), onBuild: cmd.onBuild,
		})
	default:
		return fmt.Errorf("%s is not supported by runwhp build", strings.ToUpper(cmd.Name()))
	}
	return nil
}

// dispatchArg declares build arguments in s, as BuildKit's dispatchArg.
func (b *builder) dispatchArg(s *stage, c *instructions.ArgCommand) error {
	var commits []string
	for _, arg := range c.Args {
		v, hasValue := b.o.BuildArgs[arg.Key]
		hasDefault := arg.Value != nil
		if !hasDefault && !hasValue {
			if gv, ok := b.global.Get(arg.Key); ok {
				arg.Value = &gv
			}
		}
		if hasValue {
			arg.Value = &v
			b.usedArgs[arg.Key] = true
		} else if hasDefault {
			ev, err := b.expand(*arg.Value, s.env)
			if err != nil {
				return err
			}
			arg.Value = &ev
		}
		if arg.Value != nil {
			s.env.set(arg.Key, *arg.Value)
		}
		s.args = append(s.args, arg)
		commit := arg.Key
		if arg.Value != nil {
			commit += "=" + *arg.Value
		}
		commits = append(commits, commit)
	}
	b.history(s, "ARG "+strings.Join(commits, " "))
	return nil
}

// dispatchWorkdir sets the working directory, and leaves it to be made.
func (b *builder) dispatchWorkdir(s *stage, c *instructions.WorkdirCommand) error {
	wd := c.Path
	if !path.IsAbs(wd) {
		wd = path.Join("/", s.img.Config.WorkingDir, wd)
	}
	wd = path.Clean(wd)
	s.img.Config.WorkingDir = wd
	s.workdirSet = true
	name := b.prefix(s, "WORKDIR "+wd)
	if wd != "/" {
		s.pending = append(s.pending, pendingDir{path: wd, user: s.img.Config.User})
	}
	b.progress.instant(name)
	b.history(s, "WORKDIR "+wd)
	return nil
}

// history adds an empty layer's history entry to s, and keys the state.
func (b *builder) history(s *stage, msg string) {
	s.img.History = append(s.img.History, ocispecs.History{CreatedBy: msg, Comment: historyComment, EmptyLayer: true})
	s.key = digestString(s.key, "meta", msg)
}

// layerStep runs a step that changes files, through the cache: key says
// what the step depends on besides its parent's key, fill and run what it
// does. msg is its history entry.
func (b *builder) layerStep(ctx context.Context, s *stage, msg string, key []string, step *Step) (cached bool, err error) {
	if step == nil {
		step = &Step{}
	}
	pending := s.pending
	s.pending = nil
	parts := append([]string{s.key, "layer", msg}, key...)
	for _, d := range pending {
		parts = append(parts, "mkdir", d.path, d.user)
	}
	stepKey := digestString(parts...)
	entry, hit := b.cache.Get(stepKey)
	if hit && entry.DiffID != "" && !b.w.HasLayer(entry.DiffID) {
		hit = false
	}
	if !hit || b.o.NoCache {
		fill := step.Fill
		step.Layers = slices.Clone(layersOf(s.img))
		step.Fill = func(ctx context.Context, r Root) error {
			for _, d := range pending {
				if err := r.Put(ctx, d.path, emptyTar(), rootfs.PutOptions{MakeDir: true, Chown: d.user, ChownParents: true}); err != nil {
					return fmt.Errorf("making the working directory %s: %w", d.path, err)
				}
			}
			if fill != nil {
				return fill(ctx, r)
			}
			return nil
		}
		if step.Prepare != nil {
			if err := step.Prepare(ctx); err != nil {
				return false, err
			}
		}
		var l Layer
		if step.Build != nil && len(pending) == 0 {
			l, err = step.Build(ctx)
		} else {
			l, err = b.w.Step(ctx, step)
		}
		if err != nil {
			return false, err
		}
		created := b.now().UTC()
		if hit && entry.DiffID == l.DiffID {
			created = entry.Created // the same layer as before keeps its time
		}
		entry = buildcache.Entry{Key: stepKey, CreatedBy: msg, DiffID: l.DiffID, Created: created}
	}
	cached = hit && !b.o.NoCache
	entry.Used = b.now().UTC()
	if err := b.cache.Put(entry); err != nil {
		return false, err
	}
	b.addLayer(s, msg, entry)
	return cached, nil
}

// addLayer adds the layer entry made to s.
func (b *builder) addLayer(s *stage, msg string, e buildcache.Entry) {
	created := e.Created
	h := ocispecs.History{CreatedBy: msg, Comment: historyComment, Created: &created}
	if e.DiffID == "" {
		h.EmptyLayer = true
	} else {
		s.img.RootFS.DiffIDs = append(s.img.RootFS.DiffIDs, digestOf(e.DiffID))
	}
	s.img.History = append(s.img.History, h)
	s.key = digestString(e.Key, e.DiffID)
}

// finish is the target's configuration blob.
func (b *builder) finish(target *stage) ([]byte, error) {
	img := cloneImage(target.img)
	if len(b.o.Labels) > 0 {
		if img.Config.Labels == nil {
			img.Config.Labels = map[string]string{}
		}
		maps.Copy(img.Config.Labels, b.o.Labels)
	}
	img.OS, img.Architecture = Platform.OS, Platform.Architecture
	img.RootFS.Type = "layers"
	img.History = normalizeHistory(img.History, target.baseHistory, b.epoch)
	img.Created = nil
	for _, h := range img.History {
		if h.Created != nil {
			t := *h.Created
			img.Created = &t
		}
	}
	for k := range b.o.BuildArgs {
		if !b.usedArgs[k] && !isPredefinedArg(k) {
			b.warnings = append(b.warnings, fmt.Sprintf("UnusedBuildArg: build argument %q was not consumed", k))
		}
	}
	return json.Marshal(img)
}

// isPredefinedArg reports whether a build argument needs no ARG to be used.
func isPredefinedArg(k string) bool {
	switch strings.ToUpper(k) {
	case "HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY", "SOURCE_DATE_EPOCH",
		"BUILDKIT_INLINE_CACHE", "BUILDKIT_SYNTAX":
		return true
	}
	return strings.HasPrefix(k, "TARGET") || strings.HasPrefix(k, "BUILD")
}

// layersOf lists an image's layers, by diff ID.
func layersOf(img dockerspec.DockerOCIImage) []string {
	out := make([]string, len(img.RootFS.DiffIDs))
	for i, d := range img.RootFS.DiffIDs {
		out[i] = d.String()
	}
	return out
}

// digestString hashes parts into a cache key.
func digestString(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cloneImage copies an image's configuration, as BuildKit's clone.
func cloneImage(src dockerspec.DockerOCIImage) dockerspec.DockerOCIImage {
	img := src
	img.Config.Env = slices.Clone(src.Config.Env)
	img.Config.Cmd = slices.Clone(src.Config.Cmd)
	img.Config.Entrypoint = slices.Clone(src.Config.Entrypoint)
	img.Config.OnBuild = slices.Clone(src.Config.OnBuild)
	img.Config.ExposedPorts = maps.Clone(src.Config.ExposedPorts)
	img.Config.Volumes = maps.Clone(src.Config.Volumes)
	img.Config.Labels = maps.Clone(src.Config.Labels)
	img.Config.Shell = slices.Clone(src.Config.Shell)
	if src.Config.Healthcheck != nil {
		hc := *src.Config.Healthcheck
		hc.Test = slices.Clone(src.Config.Healthcheck.Test)
		img.Config.Healthcheck = &hc
	}
	img.History = slices.Clone(src.History)
	img.RootFS.DiffIDs = slices.Clone(src.RootFS.DiffIDs)
	return img
}

// withShell is BuildKit's: args joined after the image's shell.
func withShell(img dockerspec.DockerOCIImage, args []string) []string {
	sh := slices.Clone(img.Config.Shell)
	if len(sh) == 0 {
		sh = []string{"/bin/sh", "-c"}
	}
	return append(sh, strings.Join(args, " "))
}

// exposed turns an EXPOSE word into port/proto entries.
func exposed(raw string) ([]string, error) {
	p := raw
	if i := strings.LastIndex(p, ":"); i >= 0 {
		p = p[i+1:]
	}
	port, proto, _ := strings.Cut(p, "/")
	if proto == "" {
		proto = "tcp"
	}
	proto = strings.ToLower(proto)
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return nil, fmt.Errorf("invalid port: %q: invalid proto: %s", raw, proto)
	}
	start, end, ok := strings.Cut(port, "-")
	lo, err := strconv.Atoi(start)
	if err != nil || lo < 0 || lo > 65535 {
		return nil, fmt.Errorf("invalid containerPort: %s", port)
	}
	hi := lo
	if ok {
		if hi, err = strconv.Atoi(end); err != nil || hi < lo || hi > 65535 {
			return nil, fmt.Errorf("invalid containerPort: %s", port)
		}
	}
	var out []string
	for n := lo; n <= hi; n++ {
		out = append(out, strconv.Itoa(n)+"/"+proto)
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
