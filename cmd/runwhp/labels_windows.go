//go:build windows

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/volume"
)

// parseLabels reads --label-file files, then --label flags, into labels, as
// docker run does: a later label replaces an earlier one with its key.
func parseLabels(flags, files []string) (map[string]string, error) {
	labels := map[string]string{}
	for _, f := range files {
		kvs, err := readLabelFile(f, os.LookupEnv)
		if err != nil {
			return nil, err
		}
		for _, kv := range kvs {
			k, v, _ := strings.Cut(kv, "=")
			labels[k] = v
		}
	}
	for _, l := range flags {
		k, v, _ := strings.Cut(l, "=")
		if k == "" {
			return nil, fmt.Errorf("invalid label '%s': empty name", l)
		}
		labels[k] = v
	}
	if len(labels) == 0 {
		return nil, nil
	}
	return labels, nil
}

// readLabelFile reads a --label-file as Docker's client reads one, in its
// env-file format: KEY=VALUE lines, the value kept as it is, blank lines and
// lines starting with # skipped; a KEY alone takes its value from lookup,
// and is left out if lookup has none.
func readLabelFile(path string, lookup func(string) (string, bool)) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = fmt.Errorf("open %s: %w", path, pe.Err)
		}
		return nil, fmt.Errorf("--label-file: %w", err)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("--label-file: invalid env file (%s): "+format, append([]any{path}, a...)...)
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 {
			line = strings.TrimPrefix(line, string(rune(0xFEFF)))
		}
		if !utf8.ValidString(line) {
			return nil, bad("invalid utf8 bytes at line %d: %q", n, line)
		}
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, hasValue := strings.Cut(line, "=")
		if k == "" {
			return nil, bad("no variable name on line '%s'", line)
		}
		if strings.ContainsFunc(k, unicode.IsSpace) {
			return nil, bad("variable '%s' contains whitespaces", k)
		}
		if !hasValue {
			var ok bool
			if v, ok = lookup(k); !ok {
				continue
			}
		}
		out = append(out, k+"="+v)
	}
	return out, sc.Err()
}

// withLabels returns base with over on top, or nil if both are empty.
func withLabels(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, over)
	return out
}

// filters are Docker's --filter KEY=VALUE, repeatable: a key's values are
// alternatives, except label's, which must all match, and every key given
// must match.
type filters map[string][]string

// parseFilters reads --filter flags, refusing keys not in valid.
func parseFilters(args []string, valid ...string) (filters, error) {
	f := filters{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, errors.New("bad format of filter (expected name=value)")
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if !slices.Contains(valid, k) {
			return nil, fmt.Errorf("invalid filter '%s'", k)
		}
		f[k] = append(f[k], v)
	}
	return f, nil
}

// match reports whether one of key's values passes ok, or key is not
// filtered on.
func (f filters) match(key string, ok func(string) bool) bool {
	vs, filtered := f[key]
	if !filtered {
		return true
	}
	return slices.ContainsFunc(vs, ok)
}

// matchLabels reports whether labels have every label the label filters
// name: KEY, or KEY=VALUE.
func (f filters) matchLabels(labels map[string]string) bool {
	for _, want := range f["label"] {
		k, v, hasValue := strings.Cut(want, "=")
		got, ok := labels[k]
		if !ok || hasValue && got != v {
			return false
		}
	}
	return true
}

// matchName matches a name as Docker's name filters do: as a regular
// expression, or as a substring if it is not one.
func matchName(name string) func(string) bool {
	return func(pattern string) bool {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return strings.Contains(name, pattern)
		}
		return re.MatchString(name)
	}
}

// containerStates are the words a status filter takes, as Docker's.
var containerStates = []string{"created", "running", "paused", "restarting", "removing", "exited", "dead"}

// containerFilterKeys are the keys list --filter takes.
var containerFilterKeys = []string{"id", "name", "label", "status", "health", "network", "ancestor", "exited", "volume"}

// healthStates are the words a health filter takes.
var healthStates = []string{container.HealthStarting, container.HealthHealthy, container.HealthUnhealthy, container.HealthNone}

// parseContainerFilters reads list's --filter flags.
func parseContainerFilters(args []string) (filters, error) {
	f, err := parseFilters(args, containerFilterKeys...)
	if err != nil {
		return nil, err
	}
	for _, s := range f["status"] {
		if !slices.Contains(containerStates, s) {
			return nil, fmt.Errorf("invalid filter 'status=%s': invalid value for state (%s): must be one of %s", s, s, strings.Join(containerStates, ", "))
		}
	}
	for _, s := range f["health"] {
		if !slices.Contains(healthStates, s) {
			return nil, fmt.Errorf("Unrecognised filter value for health: %s", s)
		}
	}
	for _, s := range f["exited"] {
		if _, err := strconv.Atoi(s); err != nil {
			return nil, fmt.Errorf("invalid filter 'exited=%s'", s)
		}
	}
	return f, nil
}

// stateWord is a container's state as Docker's status filter names it.
func stateWord(st container.State) string {
	switch st.Status {
	case container.Creating, container.Created:
		return "created"
	case container.Running:
		if st.Paused {
			return "paused"
		}
		return "running"
	case container.Restarting:
		return "restarting"
	case container.Stopped:
		if st.FinishedAt.IsZero() {
			return "created"
		}
	}
	return "exited"
}

// matchContainer reports whether a container passes list's filters.
func (f filters) matchContainer(c *container.Container) bool {
	cfg := c.Config
	return f.match("id", func(v string) bool { return v != "" && strings.HasPrefix(c.ID, strings.ToLower(v)) }) &&
		f.match("name", matchName(c.Name)) &&
		f.matchLabels(cfg.Labels) &&
		f.match("status", func(v string) bool { return stateWord(c.State) == v }) &&
		f.match("health", func(v string) bool { return c.State.HealthStatus() == v }) &&
		f.match("exited", func(v string) bool {
			n, _ := strconv.Atoi(v)
			return stateWord(c.State) == "exited" && c.State.ExitCode == n
		}) &&
		f.match("network", func(v string) bool {
			if len(cfg.Networks) == 0 {
				mode := cfg.Mode()
				return strings.EqualFold(v, mode) || mode == "user" && (v == "bridge" || v == "default")
			}
			for _, a := range cfg.Networks {
				if strings.EqualFold(v, a.Name) || len(v) >= 4 && strings.HasPrefix(a.Network, strings.ToLower(v)) {
					return true
				}
			}
			return false
		}) &&
		f.match("ancestor", func(v string) bool { return isAncestor(cfg, v) }) &&
		f.match("volume", func(v string) bool {
			for _, vol := range cfg.Volumes {
				if strings.EqualFold(vol.Name, v) {
					return true
				}
			}
			for _, m := range cfg.Mounts {
				if m.Target == v {
					return true
				}
			}
			return false
		})
}

// isAncestor reports whether ref names the image a container was made
// from: as it was given, by its normalized name, or by its ID or a prefix of
// that.
func isAncestor(cfg *container.Config, ref string) bool {
	if ref == "" {
		return false
	}
	if ref == cfg.Image {
		return true
	}
	if n, err := image.Normalize(ref); err == nil && cfg.ImageName != "" && n == cfg.ImageName {
		return true
	}
	id := strings.TrimPrefix(cfg.ImageID, "sha256:")
	ref = strings.TrimPrefix(ref, "sha256:")
	return id != "" && len(ref) >= 4 && strings.HasPrefix(id, ref)
}

// matchNetwork reports whether a network passes network ls's filters.
func (f filters) matchNetwork(n *network.Network) bool {
	return f.match("name", matchName(n.Name)) &&
		f.match("id", func(v string) bool { return v != "" && strings.HasPrefix(n.ID, strings.ToLower(v)) }) &&
		f.matchLabels(n.Labels) &&
		f.match("driver", func(v string) bool { return v == n.Driver }) &&
		f.match("type", func(v string) bool { return v == "builtin" && n.IsBuiltin() || v == "custom" && !n.IsBuiltin() })
}

// matchVolume reports whether a volume passes volume ls's filters; used
// says whether a container uses it.
func (f filters) matchVolume(v *volume.Volume, used bool) bool {
	return f.match("name", matchName(v.Name)) &&
		f.matchLabels(v.Labels) &&
		f.match("driver", func(d string) bool { return d == "local" }) &&
		f.match("dangling", func(d string) bool {
			b, err := strconv.ParseBool(d)
			return err == nil && b != used
		})
}
