//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/registry"

	rbuild "github.com/quasidock/runwhp/internal/build"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/image"
)

// checkLogin logs in to the registry server with ac's credentials, as
// docker login asks the daemon to, and fails if they are refused.
func checkLogin(ctx context.Context, server string, ac registry.AuthConfig) error {
	reg, err := name.NewRegistry(server)
	if err != nil {
		return err
	}
	auth := authn.FromConfig(authn.AuthConfig{Username: ac.Username, Password: ac.Password, Auth: ac.Auth,
		IdentityToken: ac.IdentityToken, RegistryToken: ac.RegistryToken})
	tr, err := transport.NewWithContext(ctx, reg, auth, http.DefaultTransport, []string{reg.Scope(transport.PullScope)})
	if err != nil {
		return fmt.Errorf("login to %s failed: %v", server, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Scheme()+"://"+reg.RegistryStr()+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return fmt.Errorf("login to %s failed: %v", server, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("login to %s failed: unauthorized: incorrect username or password", server)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("login to %s failed: %s", server, resp.Status)
	}
	return nil
}

func init() { apiJobs["build"] = jobBuild }

// buildArgs is what a build request asks, for its job.
type buildArgs struct {
	Dockerfile string            `json:"dockerfile"`
	Tags       []string          `json:"tags,omitempty"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Target     string            `json:"target,omitempty"`
	NoCache    bool              `json:"noCache,omitempty"`
	Pull       bool              `json:"pull,omitempty"`
	NoNetwork  bool              `json:"noNetwork,omitempty"`
	Hosts      []string          `json:"hosts,omitempty"`
	Memory     uint64            `json:"memory,omitempty"`
	CPUs       int               `json:"cpus,omitempty"`
	Quiet      bool              `json:"quiet,omitempty"`
}

// postBuild builds an image from the tar of a context, as Docker's
// classic builder's endpoint does, in a job, writing its progress as JSON
// messages.
func (s *apiServer) postBuild(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	if q.Get("remote") != "" {
		return badRequest("runwhp builds from a context the client sends, not from %s", q.Get("remote"))
	}
	if v := q.Get("version"); v == "2" {
		return badRequest("runwhp has no BuildKit session: build with the classic builder (DOCKER_BUILDKIT=0)")
	}
	if p := q.Get("platform"); p != "" && p != "linux/amd64" && p != "linux" {
		return badRequest("runwhp builds linux/amd64 only, not %s", p)
	}
	for _, unsupported := range []string{"cachefrom", "outputs", "squash"} {
		if v := q.Get(unsupported); v != "" && v != "[]" && v != "null" && v != "0" && v != "false" {
			return badRequest("%s is not supported by runwhp's builder", unsupported)
		}
	}
	a := buildArgs{Dockerfile: q.Get("dockerfile"), Target: q.Get("target"), NoCache: boolParam(r, "nocache"),
		Pull: boolParam(r, "pull"), Quiet: boolParam(r, "q"), Tags: q["t"]}
	if a.Dockerfile == "" {
		a.Dockerfile = "Dockerfile"
	}
	for _, p := range []struct {
		key string
		v   *map[string]string
	}{{"buildargs", &a.BuildArgs}, {"labels", &a.Labels}} {
		if raw := q.Get(p.key); raw != "" {
			var m map[string]*string
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				return badRequest("invalid %s: %v", p.key, err)
			}
			*p.v = map[string]string{}
			for k, v := range m {
				if v != nil {
					(*p.v)[k] = *v
				}
			}
		}
	}
	switch q.Get("networkmode") {
	case "", "default", "bridge", "host":
	case "none":
		a.NoNetwork = true
	default:
		return badRequest("--network %s: runwhp's builder takes default, host or none", q.Get("networkmode"))
	}
	if raw := q.Get("extrahosts"); raw != "" {
		// A list, or as older clients send it, a string.
		if err := json.Unmarshal([]byte(raw), &a.Hosts); err != nil {
			a.Hosts = strings.Split(raw, ",")
		}
	}
	var err error
	memory := ""
	if m := q.Get("memory"); m != "" && m != "0" {
		memory = m
	}
	if a.Memory, err = parseMemory(memory); err != nil {
		return badRequest("%v", err)
	}
	cpus := ""
	if cq, _ := strconv.ParseInt(q.Get("cpuquota"), 10, 64); cq > 0 {
		period, _ := strconv.ParseInt(q.Get("cpuperiod"), 10, 64)
		if period <= 0 {
			period = 100000
		}
		cpus = strconv.FormatInt((cq+period-1)/period, 10)
	}
	if a.CPUs, err = parseCPUs(cpus, runtime.NumCPU()); err != nil {
		return badRequest("%v", err)
	}
	for _, t := range a.Tags {
		if _, err := image.Normalize(t); err != nil || strings.Contains(t, "@") {
			return badRequest("invalid reference format: %q", t)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	stream := dockerapi.NewStream(w, func() {
		if flusher != nil {
			flusher.Flush()
		}
	})
	res, err := s.jobs.run(r.Context(), "build", a, r.Body, nil, func(line []byte) error {
		_, err := w.Write(append(line, '\r', '\n'))
		if flusher != nil {
			flusher.Flush()
		}
		return err
	})
	if err != nil {
		if r.Context().Err() == nil {
			stream.Error(err)
		}
		return nil
	}
	var out struct{ ID string }
	json.Unmarshal(res, &out)
	s.emit(events.ImageEventType, "build", out.ID, nil)
	return nil
}

// jobBuild extracts the context the request's body holds, and builds it.
func jobBuild(ctx context.Context, root string, args json.RawMessage, in io.Reader, out jobSink) (any, error) {
	var a buildArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	stream := messageStream(out)
	release, err := gc.Hold(root)
	if err != nil {
		return nil, err
	}
	defer release()
	sweepStore(root, func(string, ...any) {})

	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(tmp, "build-context-*")
	if err != nil {
		return nil, err
	}
	// Removed once the job has answered: removing files is slow on Windows.
	afterJob(func() { os.RemoveAll(dir) })
	tr, err := image.Decompress(in)
	if err != nil {
		return nil, badRequest("reading the build context: %v", err)
	}
	if err := archive.Untar(tr, dir, &archive.TarOptions{NoLchown: true}); err != nil {
		return nil, badRequest("reading the build context: %v", err)
	}
	tr.Close()

	opts := rbuild.Options{Target: a.Target, NoCache: a.NoCache, NoNetwork: a.NoNetwork, Hosts: a.Hosts,
		BuildArgs: map[string]string{}, Labels: map[string]string{}, Secrets: map[string]rbuild.Secret{}}
	for k, v := range a.BuildArgs {
		opts.BuildArgs[k] = v
	}
	for k, v := range a.Labels {
		opts.Labels[k] = v
	}
	// The Dockerfile's path is in the context, as Docker's client sends it;
	// one from outside the context it sends inside, with a name of its own.
	df := filepath.Join(dir, filepath.FromSlash(a.Dockerfile))
	rel, err := filepath.Rel(dir, df)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, badRequest("the Dockerfile %s is outside the build context", a.Dockerfile)
	}
	data, err := os.ReadFile(df)
	if err != nil {
		return nil, badRequest("Cannot locate specified Dockerfile: %s", a.Dockerfile)
	}
	if err := openBuildContext(&opts, dir, df, data); err != nil {
		return nil, err
	}
	opts.DockerfileName = filepath.Base(df)
	var refs []string
	for _, t := range a.Tags {
		n, err := image.Normalize(t)
		if err != nil {
			return nil, badRequest("invalid reference format: %q", t)
		}
		refs = append(refs, n)
	}
	var progress io.Writer
	if !a.Quiet {
		progress = stream.Writer()
	}
	res, err := buildImage(ctx, root, buildRequest{opts: opts, refs: refs, pull: a.Pull, memory: a.Memory, cpus: a.CPUs}, progress)
	if err != nil {
		var le *rbuild.LocatedError
		if errors.As(err, &le) && !a.Quiet {
			stream.Text(le.Excerpt() + "\n")
		}
		var re recordError
		if errors.As(err, &re) {
			return nil, re.err
		}
		return nil, badRequest("failed to solve: %v", err)
	}
	for _, warning := range res.Warnings {
		if !a.Quiet {
			stream.Text(" - " + warning + "\n")
		}
	}
	stream.Aux(build.Result{ID: res.ID})
	if a.Quiet {
		// What docker build -q prints.
		stream.Text(res.ID + "\n")
	}
	if !a.Quiet {
		stream.Text("Successfully built " + shortID(res.ID) + "\n")
		for _, t := range a.Tags {
			stream.Text("Successfully tagged " + familiarRef(t) + "\n")
		}
	}
	return map[string]string{"ID": res.ID}, nil
}

func (s *apiServer) postBuildPrune(w http.ResponseWriter, r *http.Request) error {
	m, err := dockerapi.DecodeFilters(r.URL.Query().Get("filters"))
	if err != nil {
		return badRequest("%v", err)
	}
	if len(m) > 0 {
		return badRequest("runwhp's build cache prune takes no filters")
	}
	release, err := gc.Exclusive(s.root, func() {})
	if err != nil {
		return err
	}
	defer release()
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	rep := u.Apply(u.BuildCache(boolParam(r, "all")))
	resp := build.CachePruneReport{CachesDeleted: rep.Cache, SpaceReclaimed: uint64(max(rep.Freed, 0))}
	if resp.CachesDeleted == nil {
		resp.CachesDeleted = []string{}
	}
	writeJSON(w, resp)
	return nil
}
