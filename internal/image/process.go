package image

import (
	"errors"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Overrides are the command-line settings that replace parts of the image
// configuration, as docker run applies them.
type Overrides struct {
	// Entrypoint replaces the image's entrypoint when EntrypointSet; an
	// empty value removes it. Setting it also drops the image's Cmd.
	Entrypoint    string
	EntrypointSet bool
	Cmd           []string // replaces the image's Cmd when non-empty
	Env           []string // KEY=VALUE; a bare KEY takes the value from LookupEnv
	Workdir       string
	User          string
	// LookupEnv resolves bare -e KEY entries, normally os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Process is what the container runs.
type Process struct {
	Args []string
	Env  []string
	Cwd  string
	User string
}

// ErrNoCommand means neither the image nor the command line gave a command.
var ErrNoCommand = errors.New("no command specified")

// BuildProcess merges the image configuration and the overrides.
func BuildProcess(cfg v1.Config, o Overrides) (Process, error) {
	p := Process{Cwd: cfg.WorkingDir, User: cfg.User}

	entrypoint, cmd := cfg.Entrypoint, cfg.Cmd
	if o.EntrypointSet {
		entrypoint, cmd = nil, nil
		if o.Entrypoint != "" {
			entrypoint = []string{o.Entrypoint}
		}
	}
	if len(o.Cmd) > 0 {
		cmd = o.Cmd
	}
	p.Args = append(append([]string(nil), entrypoint...), cmd...)
	if len(p.Args) == 0 {
		return p, ErrNoCommand
	}

	p.Env = mergeEnv(cfg.Env, o.Env, o.LookupEnv)
	if o.Workdir != "" {
		p.Cwd = o.Workdir
	}
	if p.Cwd == "" {
		p.Cwd = "/"
	}
	if o.User != "" {
		p.User = o.User
	}
	return p, nil
}

func mergeEnv(base, overrides []string, lookup func(string) (string, bool)) []string {
	out := append([]string(nil), base...)
	index := map[string]int{}
	for i, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		index[k] = i
	}
	for _, kv := range overrides {
		k, _, hasValue := strings.Cut(kv, "=")
		if !hasValue {
			if lookup == nil {
				continue
			}
			v, ok := lookup(k)
			if !ok {
				continue
			}
			kv = k + "=" + v
		}
		if i, ok := index[k]; ok {
			out[i] = kv
		} else {
			index[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}
