package build

import (
	"archive/tar"
	"bytes"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// envList is an environment in order, as BuildKit's llb.EnvList: a later
// value of a key replaces the earlier one in place.
type envList struct {
	keys   []string
	values map[string]string
}

// envOf makes an envList of KEY=VALUE entries.
func envOf(env []string) *envList {
	e := &envList{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		e.set(k, v)
	}
	return e
}

func (e *envList) set(k, v string) {
	if e.values == nil {
		e.values = map[string]string{}
	}
	for _, have := range e.keys {
		if shell.EqualEnvKeys(have, k) {
			e.values[have] = v
			return
		}
	}
	e.keys = append(e.keys, k)
	e.values[k] = v
}

func (e *envList) del(k string) {
	e.keys = slices.DeleteFunc(e.keys, func(have string) bool {
		if shell.EqualEnvKeys(have, k) {
			delete(e.values, have)
			return true
		}
		return false
	})
}

// Get is shell.EnvGetter's.
func (e *envList) Get(k string) (string, bool) {
	for _, have := range e.keys {
		if shell.EqualEnvKeys(have, k) {
			return e.values[have], true
		}
	}
	return "", false
}

// Keys is shell.EnvGetter's.
func (e *envList) Keys() []string { return slices.Clone(e.keys) }

// list is the environment as KEY=VALUE entries.
func (e *envList) list() []string {
	out := make([]string, len(e.keys))
	for i, k := range e.keys {
		out[i] = k + "=" + e.values[k]
	}
	return out
}

// addEnv sets k in an image's environment, as BuildKit's addEnv.
func addEnv(env []string, k, v string) []string {
	for i, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if shell.EqualEnvKeys(key, k) {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

// delEnv removes k from an image's environment.
func delEnv(env []string, k string) []string {
	return slices.DeleteFunc(env, func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return shell.EqualEnvKeys(key, k)
	})
}

// digestOf is a diff ID as a digest.
func digestOf(diffID string) digest.Digest { return digest.Digest(diffID) }

// emptyTar is a tar of no entries.
func emptyTar() io.Reader {
	var buf bytes.Buffer
	tar.NewWriter(&buf).Close()
	return &buf
}

// normalizeHistory gives every history entry a time, as BuildKit's image
// exporter does: an entry before the first new layer takes that layer's
// time, any other the time of the layer before it. With epoch, entries
// after the base image's are clamped to it.
func normalizeHistory(history []ocispecs.History, base int, epoch *time.Time) []ocispecs.History {
	history = slices.Clone(history)
	var created *time.Time
	seenEmpty := false
	for _, h := range history {
		if h.Created != nil {
			created = h.Created
			if seenEmpty {
				break
			}
		} else {
			seenEmpty = true
		}
	}
	seenEmpty = false
	for i, h := range history {
		if h.Created != nil {
			if seenEmpty {
				created = h.Created
			}
		} else {
			seenEmpty = true
			if created != nil {
				t := *created
				h.Created = &t
			}
		}
		history[i] = h
	}
	if epoch != nil {
		for i := base; i < len(history); i++ {
			if history[i].Created == nil || history[i].Created.After(*epoch) {
				t := *epoch
				history[i].Created = &t
			}
		}
	}
	return history
}
