package build

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"

	"github.com/quasidock/runwhp/internal/rootfs"
)

// Hostname is a RUN step's, as BuildKit's.
const Hostname = "buildkitsandbox"

// pipesDir is where a heredoc with a shebang is run from, as BuildKit
// mounts it: below /dev, a tmpfs, so it is no part of the step's layer.
const pipesDir = "/dev/pipes"

// dispatchRun runs a RUN step, as BuildKit's dispatchRun.
func (b *builder) dispatchRun(ctx context.Context, s *stage, c *instructions.RunCommand, onBuild bool) error {
	args := c.CmdLine
	name := c.String()
	var files []inlineFile
	if len(c.Files) > 0 {
		if len(args) != 1 || !c.PrependShell {
			return fmt.Errorf("parsing produced an invalid run command: %v", args)
		}
		if heredoc := parser.MustParseHeredoc(args[0]); heredoc != nil {
			data := c.Files[0].Data
			if c.Files[0].Chomp {
				data = parser.ChompHeredocContent(data)
			}
			if strings.HasPrefix(data, "#!") {
				// A single heredoc with a shebang: a file to run.
				p := path.Join(pipesDir, c.Files[0].Name)
				files = append(files, inlineFile{path: p, data: data, mode: 0o755})
				args = []string{p}
			} else {
				// A single heredoc: its contents run in the shell.
				args = []string{data}
			}
			name += fmt.Sprintf(" (%s)", summarizeHeredoc(c.Files[0].Data))
		} else {
			// Several: put back together, for the shell to read.
			var sb strings.Builder
			sb.WriteString(args[0])
			for _, f := range c.Files {
				sb.WriteByte('\n')
				sb.WriteString(f.Data)
				sb.WriteString(f.Name)
			}
			args = []string{sb.String()}
		}
	}
	if c.PrependShell {
		args = withShell(s.img, args)
	}

	network := !b.o.NoNetwork
	switch instructions.GetNetwork(c) {
	case instructions.NetworkNone:
		network = false
	case instructions.NetworkDefault, instructions.NetworkHost:
	default:
		return fmt.Errorf("RUN --network=%s is not supported by runwhp build", instructions.GetNetwork(c))
	}
	if sec := instructions.GetSecurity(c); sec != "" && sec != instructions.SecuritySandbox && sec != instructions.SecurityInsecure {
		return fmt.Errorf("RUN --security=%s is not supported", sec)
	}
	if slices.Contains(c.FlagsUsed, "device") {
		return errors.New("RUN --device is not supported by runwhp build")
	}

	env := s.env.list()
	key := []string{"run", "user=" + s.img.Config.User, "cwd=" + workdirOf(s.img), "network=" + strconv.FormatBool(network)}
	key = append(key, env...)
	for _, h := range b.o.Hosts {
		key = append(key, "host="+h)
	}
	for _, f := range files {
		key = append(key, "file", f.path, f.data)
	}
	mounts, mkey, extraEnv, err := b.runMounts(s, c)
	if err != nil {
		return err
	}
	key = append(key, mkey...)
	// The proxy arguments reach the process, but neither its history nor
	// its key, as with BuildKit.
	for _, kv := range b.proxy {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, k+"=") }) {
			env = append(env, kv)
		}
	}
	env = append(env, extraEnv...)

	msg := "RUN " + runCommandString(args, s.args, s.env) + " # buildkit"
	v := b.progress.start(b.prefix(s, onBuildPrefix(onBuild)+name))
	step := &Step{
		Fill: func(ctx context.Context, r Root) error {
			for _, f := range files {
				if err := r.Put(ctx, path.Dir(f.path), fileTar(path.Base(f.path), f.data, f.mode), rootfs.PutOptions{MakeDir: true}); err != nil {
					return err
				}
			}
			return nil
		},
		Run: &Process{
			Args: args, Env: env, Cwd: workdirOf(s.img), User: s.img.Config.User, Hostname: Hostname,
			Network: network, Hosts: b.o.Hosts, Mounts: mounts, Output: v,
		},
	}
	cached, err := b.layerStep(ctx, s, msg, key, step)
	var ee *ExitError
	if errors.As(err, &ee) {
		err = fmt.Errorf("process %q did not complete successfully: %w", strings.Join(args, " "), err)
	}
	v.end(err, cached)
	return err
}

// runMounts turns a RUN step's --mount flags into the worker's mounts, the
// parts of its cache key they add, and the environment its secrets give.
func (b *builder) runMounts(s *stage, c *instructions.RunCommand) ([]Mount, []string, []string, error) {
	var out []Mount
	var key, env []string
	for _, m := range instructions.GetMounts(c) {
		target := m.Target
		if target != "" && !path.IsAbs(target) {
			target = path.Join("/", workdirOf(s.img), target)
		}
		switch m.Type {
		case instructions.MountTypeCache:
			if target == "" {
				return nil, nil, nil, errors.New("mount type cache requires a target")
			}
			id := m.CacheID
			if id == "" {
				id = path.Clean(target)
			}
			out = append(out, Mount{Kind: "cache", Target: target, ID: CacheVolume(id), ReadOnly: m.ReadOnly, UID: m.UID, GID: m.GID, Mode: m.Mode})
			key = append(key, "cache", id, target, fmt.Sprint(m.ReadOnly))
		case instructions.MountTypeTmpfs:
			if target == "" {
				return nil, nil, nil, errors.New("mount type tmpfs requires a target")
			}
			out = append(out, Mount{Kind: "tmpfs", Target: target})
			key = append(key, "tmpfs", target)
		case instructions.MountTypeBind:
			if !m.ReadOnly {
				return nil, nil, nil, errors.New("RUN --mount=type=bind,rw is not supported by runwhp build: binds are read-only")
			}
			if b.o.Context == nil {
				return nil, nil, nil, errors.New("RUN --mount=type=bind needs a build context")
			}
			if target == "" {
				return nil, nil, nil, errors.New("mount type bind requires a target")
			}
			src, err := b.o.Context.hostPath(m.Source)
			if err != nil {
				return nil, nil, nil, err
			}
			sum, err := b.o.Context.hash(path.Clean("/" + m.Source))
			if err != nil {
				return nil, nil, nil, err
			}
			out = append(out, Mount{Kind: "bind", Target: target, Source: src, ReadOnly: true})
			key = append(key, "bind", m.Source, target, sum)
		case instructions.MountTypeSecret:
			id := m.CacheID
			if id == "" && target != "" {
				id = path.Base(target)
			}
			if id == "" {
				return nil, nil, nil, errors.New("one of source, target required")
			}
			sec, ok := b.o.Secrets[id]
			if !ok {
				if m.Required {
					return nil, nil, nil, fmt.Errorf("secret %s: not found", id)
				}
				continue
			}
			key = append(key, "secret", id, target)
			if m.Env != nil {
				name := *m.Env
				if name == "" {
					name = id
				}
				data, err := os.ReadFile(sec.Path)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("secret %s: %w", id, err)
				}
				env = append(env, name+"="+string(data))
				key = append(key, "env="+name)
				if target == "" {
					continue
				}
			}
			if target == "" {
				target = "/run/secrets/" + id
			}
			out = append(out, Mount{Kind: "secret", Target: target, Source: sec.Path, ReadOnly: true, UID: m.UID, GID: m.GID, Mode: m.Mode})
		case instructions.MountTypeSSH:
			return nil, nil, nil, errors.New("RUN --mount=type=ssh is not supported by runwhp build")
		default:
			return nil, nil, nil, fmt.Errorf("RUN --mount=type=%s is not supported by runwhp build", m.Type)
		}
	}
	return out, key, env, nil
}

// CacheVolume names the volume of a cache mount's id.
func CacheVolume(id string) string { return "cache-" + digestString(id)[:24] }

// inlineFile is a heredoc to put in place before a step runs.
type inlineFile struct {
	path, data string
	mode       int64
}

// fileTar is a tar of one file.
func fileTar(name, data string, mode int64) *bytes.Buffer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))})
	tw.Write([]byte(data))
	tw.Close()
	return &buf
}

// runCommandString is BuildKit's: the arguments in scope, then args.
func runCommandString(args []string, buildArgs []instructions.KeyValuePairOptional, env shell.EnvGetter) string {
	var tmp []string
	idx := map[string]int{}
	for _, arg := range buildArgs {
		v, ok := env.Get(arg.Key)
		if !ok {
			v = arg.ValueString()
		}
		if i, ok := idx[arg.Key]; ok {
			tmp[i] = arg.Key + "=" + v
		} else {
			idx[arg.Key] = len(tmp)
			tmp = append(tmp, arg.Key+"="+v)
		}
	}
	if len(tmp) > 0 {
		tmp = append([]string{fmt.Sprintf("|%d", len(tmp))}, tmp...)
	}
	return strings.Join(append(tmp, args...), " ")
}

// summarizeHeredoc is BuildKit's: a heredoc's first line.
func summarizeHeredoc(doc string) string {
	doc = strings.TrimSpace(doc)
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	summary := lines[0]
	if len(lines) > 1 {
		summary += "..."
	}
	return summary
}

// workdirOf is an image's working directory, / if it has none.
func workdirOf(img dockerspec.DockerOCIImage) string {
	if wd := img.Config.WorkingDir; wd != "" {
		return wd
	}
	return "/"
}

func onBuildPrefix(onBuild bool) string {
	if onBuild {
		return "ONBUILD "
	}
	return ""
}
