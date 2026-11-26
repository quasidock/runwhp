//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	dcontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dnetwork "github.com/moby/moby/api/types/network"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/rootfs"
	"github.com/quasidock/runwhp/internal/vmm"
	"github.com/quasidock/runwhp/internal/volume"
)

func (s *apiServer) containerRoutes() {
	s.handle("POST /containers/create", s.postContainerCreate)
	s.handle("GET /containers/json", s.getContainers)
	s.handle("POST /containers/prune", s.postContainersPrune)
	s.handle("GET /containers/{id}/json", s.getContainer)
	s.handle("POST /containers/{id}/start", s.postContainerStart)
	s.handle("POST /containers/{id}/stop", s.postContainerStop)
	s.handle("POST /containers/{id}/restart", s.postContainerRestart)
	s.handle("POST /containers/{id}/kill", s.postContainerKill)
	s.handle("POST /containers/{id}/pause", s.postContainerPause)
	s.handle("POST /containers/{id}/unpause", s.postContainerPause)
	s.handle("POST /containers/{id}/rename", s.postContainerRename)
	s.handle("POST /containers/{id}/wait", s.postContainerWait)
	s.handle("POST /containers/{id}/resize", s.postContainerResize)
	s.handle("POST /containers/{id}/attach", s.postContainerAttach)
	s.handle("POST /containers/{id}/update", s.postContainerUpdate)
	s.handle("DELETE /containers/{id}", s.deleteContainer)
	s.handle("GET /containers/{id}/logs", s.getContainerLogs)
	s.handle("GET /containers/{id}/stats", s.getContainerStats)
	s.handle("GET /containers/{id}/top", s.getContainerTop)
	s.handle("GET /containers/{id}/changes", s.getContainerChanges)
	s.handle("GET /containers/{id}/export", s.getContainerExport)
	s.handle("HEAD /containers/{id}/archive", s.headContainerArchive)
	s.handle("GET /containers/{id}/archive", s.getContainerArchive)
	s.handle("PUT /containers/{id}/archive", s.putContainerArchive)
	s.handle("GET /containers/{id}/attach/ws", func(http.ResponseWriter, *http.Request) error {
		return notImplemented("runwhp does not serve attaches over websockets")
	})
}

// resolve finds the container named ref, words its absence as Docker's
// daemon does.
func (s *apiServer) resolve(ref string) (*container.Container, error) {
	c, err := s.containers.Resolve(ref)
	switch {
	case errors.Is(err, container.ErrNotFound):
		return nil, notFound("No such container: %s", ref)
	case errors.Is(err, container.ErrAmbiguous):
		return nil, badRequest("multiple IDs found with provided prefix: %s", ref)
	}
	return c, err
}

func (s *apiServer) postContainerCreate(w http.ResponseWriter, r *http.Request) error {
	var req dcontainer.CreateRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	q := r.URL.Query()
	if p := q.Get("platform"); p != "" && p != "linux" && p != "linux/amd64" {
		return badRequest("runwhp runs linux/amd64 containers only, not %s", p)
	}
	name := strings.TrimPrefix(q.Get("name"), "/")
	if name != "" {
		if err := container.CheckName(name); err != nil {
			return badRequest("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name)
		}
	}
	if s.debug {
		data, _ := json.Marshal(req)
		s.logf("create %s: %s", name, data)
	}
	spec, warnings, err := specFromCreate(req)
	if err != nil {
		return err
	}
	// The image must be here already, the client pulls it; runwhp's kernel
	// is fetched if it has to be, as run fetches it.
	if _, err := vmm.ResolveKernel(r.Context(), s.root, "", image.PullMissing); err != nil {
		return err
	}
	var diag bytes.Buffer
	stdinOnce := req.Config.StdinOnce
	rec, _, err := createContainer(r.Context(), s.root, &spec, createOptions{
		name: name, policy: image.PullNever, stdinOnce: stdinOnce, diag: &diag,
	})
	switch {
	case errors.Is(err, image.ErrNotPresent):
		return notFound("No such image: %s", familiarRef(spec.image))
	case errors.Is(err, container.ErrNameInUse):
		other := ""
		if c, rerr := s.containers.Resolve(name); rerr == nil {
			other = c.ID
		}
		return conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, other)
	case err != nil:
		if diag.Len() > 0 {
			s.logf("create %s failed; the kernel's last lines:\n%s", name, diag.String())
		}
		if httpStatus(err) == http.StatusInternalServerError {
			// What the spec asked that the store or the image refused.
			return badRequest("%v", err)
		}
		return err
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSONStatus(w, http.StatusCreated, dcontainer.CreateResponse{ID: rec.ID, Warnings: warnings})
	return nil
}

// familiarRef is an image reference as the docker CLI shows it.
func familiarRef(ref string) string {
	if n, err := image.Normalize(ref); err == nil {
		return image.Familiar(n)
	}
	return ref
}

// containerFiltersFrom checks the filters of a container list as list
// --filter does.
func containerFiltersFrom(raw string) (filters, error) {
	m, err := dockerapi.DecodeFilters(raw)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	var args []string
	for k, vs := range m {
		for _, v := range vs {
			args = append(args, k+"="+v)
		}
	}
	sort.Strings(args)
	f, err := parseContainerFilters(args)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return f, nil
}

func (s *apiServer) getContainers(w http.ResponseWriter, r *http.Request) error {
	f, err := containerFiltersFrom(r.URL.Query().Get("filters"))
	if err != nil {
		return err
	}
	all := boolParam(r, "all")
	limit, err := intParam(r, "limit", -1)
	if err != nil {
		return err
	}
	cs, err := s.containers.List()
	if err != nil {
		return err
	}
	out := []dcontainer.Summary{}
	for _, c := range cs {
		if limit > 0 && len(out) == limit {
			break
		}
		word := stateWord(c.State)
		if !all && limit <= 0 && word != "running" && word != "paused" && word != "restarting" {
			continue
		}
		if !f.matchContainer(c) {
			continue
		}
		out = append(out, s.summaryOf(c, boolParam(r, "size")))
	}
	writeJSON(w, out)
	return nil
}

// dockerTime is a time as Docker's inspect writes it; the zero time is
// Go's zero, as Docker's is.
func dockerTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// containerName is a container's name as Docker's API gives it, with a
// slash; one runwhp made without a name goes by its short ID.
func containerName(c *container.Container) string {
	if c.Name != "" {
		return "/" + c.Name
	}
	return "/" + c.Short()
}

func (s *apiServer) summaryOf(c *container.Container, size bool) dcontainer.Summary {
	cfg := c.Config
	sum := dcontainer.Summary{
		ID: c.ID, Names: []string{containerName(c)}, Image: imageOf(cfg), ImageID: cfg.ImageID,
		Command: strings.Join(cfg.Args, " "), Created: cfg.Created.Unix(),
		Ports: []dcontainer.PortSummary{}, Labels: cfg.Labels,
		State:  dcontainer.ContainerState(stateWord(c.State)),
		Status: statusText(c.State, time.Now()),
		Mounts: mountPointsOf(s.root, c),
	}
	if sum.Labels == nil {
		sum.Labels = map[string]string{}
	}
	if c.State.Active() {
		published := map[string]bool{}
		for _, p := range container.Published(cfg, c.State) {
			published[fmt.Sprintf("%d/tcp", p.Guest)] = true
			sum.Ports = append(sum.Ports, dcontainer.PortSummary{IP: p.Host.Addr(), PrivatePort: p.Guest, PublicPort: p.Host.Port(), Type: "tcp"})
		}
		// Docker lists the ports exposed and not published too, without
		// a public port.
		for _, e := range exposedPorts(nil, cfg.Expose, nil) {
			if port, err := dnetwork.ParsePort(e); err == nil && !published[e] {
				sum.Ports = append(sum.Ports, dcontainer.PortSummary{PrivatePort: port.Num(), Type: string(port.Proto())})
			}
		}
	}
	sum.HostConfig.NetworkMode = dockerNetworkMode(cfg)
	sum.NetworkSettings = &dcontainer.NetworkSettingsSummary{Networks: s.endpointsOf(c)}
	if cfg.Healthcheck != nil {
		sum.Health = &dcontainer.HealthSummary{Status: dcontainer.HealthStatus(c.State.HealthStatus())}
		if c.State.Health != nil {
			sum.Health.FailingStreak = c.State.Health.FailingStreak
		}
	}
	if size {
		if st, err := allocatedSize(c.RW()); err == nil {
			sum.SizeRw = st
		}
	}
	return sum
}

// portMaps are a container's ports as Docker's inspect gives them: the
// bindings asked for, a random host port's as "", and, while it has a VM,
// the ports bound, with each port exposed and not published as null.
func portMaps(c *container.Container) (bindings, ports dnetwork.PortMap) {
	bindings, ports = dnetwork.PortMap{}, dnetwork.PortMap{}
	for _, p := range c.Config.Publish {
		port, _ := dnetwork.PortFrom(p.Guest, dnetwork.TCP)
		hp := ""
		if !p.Random() {
			hp = strconv.Itoa(int(p.Host.Port()))
		}
		bindings[port] = append(bindings[port], dnetwork.PortBinding{HostIP: p.Host.Addr(), HostPort: hp})
	}
	if !c.State.Active() {
		return bindings, ports
	}
	for _, e := range c.Config.Expose {
		if port, err := dnetwork.ParsePort(e); err == nil {
			ports[port] = nil
		}
	}
	for _, p := range container.Published(c.Config, c.State) {
		port, _ := dnetwork.PortFrom(p.Guest, dnetwork.TCP)
		ports[port] = append(ports[port], dnetwork.PortBinding{HostIP: p.Host.Addr(), HostPort: strconv.Itoa(int(p.Host.Port()))})
	}
	return bindings, ports
}

// dockerNetworkMode is a container's network mode as Docker names it:
// runwhp's default network is Docker's bridge.
func dockerNetworkMode(cfg *container.Config) string {
	switch m := cfg.Mode(); m {
	case network.User, "default":
		return "bridge"
	default:
		return m
	}
}

// endpointsOf is a container's networks as Docker's inspect lists them.
func (s *apiServer) endpointsOf(c *container.Container) map[string]*dnetwork.EndpointSettings {
	out := map[string]*dnetwork.EndpointSettings{}
	cfg := c.Config
	if len(cfg.Networks) == 0 {
		if !cfg.Network {
			out["none"] = &dnetwork.EndpointSettings{NetworkID: "none"}
			return out
		}
		e := &dnetwork.EndpointSettings{NetworkID: network.User}
		if c.State.Active() {
			e.Gateway, e.IPAddress, e.IPPrefixLen = netstack.Gateway, netstack.Guest, netstack.Subnet.Bits()
		}
		out["bridge"] = e
		return out
	}
	st := withNetworks(s.root, containerState{}, c)
	for name, ns := range st.Networks {
		e := &dnetwork.EndpointSettings{NetworkID: ns.NetworkID, EndpointID: ns.EndpointID, Aliases: ns.Aliases, DNSNames: ns.DNSNames,
			IPPrefixLen: ns.IPPrefixLen}
		e.Gateway, _ = netip.ParseAddr(ns.Gateway)
		e.IPAddress, _ = netip.ParseAddr(ns.IPAddress)
		if mac, err := parseMAC(ns.MacAddress); err == nil {
			e.MacAddress = mac
		}
		for _, a := range cfg.Networks {
			if ip, err := netip.ParseAddr(a.IPv4); a.Name == name && err == nil {
				e.IPAMConfig = &dnetwork.EndpointIPAMConfig{IPv4Address: ip}
			}
		}
		out[name] = e
	}
	return out
}

func parseMAC(s string) (dnetwork.HardwareAddr, error) {
	var mac dnetwork.HardwareAddr
	if s == "" {
		return mac, errors.New("no MAC")
	}
	err := mac.UnmarshalText([]byte(s))
	return mac, err
}

// mountPointsOf is a container's mounts as Docker's inspect lists them.
func mountPointsOf(root string, c *container.Container) []dcontainer.MountPoint {
	cfg := c.Config
	out := []dcontainer.MountPoint{}
	for _, m := range cfg.Mounts {
		mp := dcontainer.MountPoint{Destination: m.Target, RW: !m.ReadOnly, Mode: "", Propagation: ""}
		switch m.Kind {
		case guestspec.MountBind:
			mp.Type, mp.Propagation = mount.TypeBind, mount.PropagationRPrivate
			for _, sh := range cfg.Shares {
				if sh.Target == m.Target {
					mp.Source = sh.Path
				}
			}
		case guestspec.MountVolume:
			mp.Type, mp.Name, mp.Driver, mp.Mode = mount.TypeVolume, m.Name, "local", "z"
			if v, err := volume.Open(root).Get(m.Name); err == nil {
				mp.Source = v.Disk()
			}
		case guestspec.MountAnon:
			mp.Type, mp.Driver = mount.TypeVolume, "local"
			mp.Source = c.RW()
		case guestspec.MountTmpfs:
			mp.Type = mount.TypeTmpfs
		default:
			continue
		}
		if !mp.RW && mp.Mode == "" {
			mp.Mode = "ro"
		}
		out = append(out, mp)
	}
	return out
}

func (s *apiServer) getContainer(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, s.inspectOf(c, boolParam(r, "size")))
	return nil
}

// dockerState is a container's state as Docker's inspect gives it.
func dockerState(c *container.Container) *dcontainer.State {
	st := c.State
	word := stateWord(st)
	ds := &dcontainer.State{
		Status: dcontainer.ContainerState(word), Running: word == "running" || word == "paused",
		Paused: word == "paused", Restarting: word == "restarting",
		Pid: st.Pid, ExitCode: st.ExitCode, Error: st.Error,
		StartedAt: dockerTime(st.StartedAt), FinishedAt: dockerTime(st.FinishedAt),
	}
	if !ds.Running {
		ds.Pid = 0
	}
	if h := st.Health; h != nil {
		ds.Health = &dcontainer.Health{Status: dcontainer.HealthStatus(h.Status), FailingStreak: h.FailingStreak, Log: []*dcontainer.HealthcheckResult{}}
		for _, l := range h.Log {
			ds.Health.Log = append(ds.Health.Log, &dcontainer.HealthcheckResult{Start: l.Start, End: l.End, ExitCode: l.ExitCode, Output: l.Output})
		}
	}
	return ds
}

func (s *apiServer) inspectOf(c *container.Container, size bool) dcontainer.InspectResponse {
	cfg := c.Config
	resp := dcontainer.InspectResponse{
		ID: c.ID, Created: dockerTime(cfg.Created), State: dockerState(c), Image: cfg.ImageID,
		LogPath: c.LogPath(), Name: containerName(c), RestartCount: c.State.RestartCount,
		Driver: "runwhp", Platform: "linux", ExecIDs: s.execIDsOf(c.ID),
		Mounts: mountPointsOf(s.root, c),
	}
	if len(cfg.Args) > 0 {
		resp.Path, resp.Args = cfg.Args[0], cfg.Args[1:]
	}
	if resp.Args == nil {
		resp.Args = []string{}
	}
	dcfg := &dcontainer.Config{
		Hostname: cfg.Hostname, User: cfg.User, Env: cfg.Env, Cmd: cfg.Cmd, Entrypoint: cfg.Entrypoint,
		Image: imageOf(cfg), WorkingDir: cfg.Cwd, Labels: cfg.Labels, Tty: cfg.TTY,
		OpenStdin: cfg.Stdin, StdinOnce: cfg.StdinOnce, AttachStdout: true, AttachStderr: true, AttachStdin: cfg.Stdin,
		StopSignal: cfg.StopSignal, StopTimeout: cfg.StopTimeout, NetworkDisabled: !cfg.Network,
	}
	if dcfg.Labels == nil {
		dcfg.Labels = map[string]string{}
	}
	if hc := cfg.Healthcheck; hc != nil {
		dcfg.Healthcheck = &dcontainer.HealthConfig{Test: hc.Test, Interval: hc.Interval, Timeout: hc.Timeout,
			StartPeriod: hc.StartPeriod, StartInterval: hc.StartInterval, Retries: hc.Retries}
	}
	bindings, ports := portMaps(c)
	for _, e := range exposedPorts(nil, cfg.Expose, cfg.Publish) {
		if port, err := dnetwork.ParsePort(e); err == nil {
			if dcfg.ExposedPorts == nil {
				dcfg.ExposedPorts = dnetwork.PortSet{}
			}
			dcfg.ExposedPorts[port] = struct{}{}
		}
	}
	resp.Config = dcfg

	hc := &dcontainer.HostConfig{
		NetworkMode: dcontainer.NetworkMode(dockerNetworkMode(cfg)), PortBindings: bindings,
		RestartPolicy: dcontainer.RestartPolicy{Name: dcontainer.RestartPolicyMode(cfg.Restart.Name), MaximumRetryCount: cfg.Restart.MaxRetries},
		AutoRemove:    cfg.AutoRemove,
		LogConfig:     dcontainer.LogConfig{Type: cfg.LogDriver, Config: map[string]string{}},
		Runtime:       "runwhp",
		Isolation:     "",
	}
	if hc.RestartPolicy.Name == "" {
		hc.RestartPolicy.Name = "no"
	}
	if hc.LogConfig.Type == "" {
		hc.LogConfig.Type = container.LogDriverLocal
	}
	hc.Memory = int64(cfg.Memory)
	hc.NanoCPUs = int64(cfg.CPUs) * 1e9
	for _, sh := range cfg.Shares {
		b := sh.Path + ":" + sh.Target
		if sh.ReadOnly {
			b += ":ro"
		}
		hc.Binds = append(hc.Binds, b)
	}
	for _, m := range cfg.Mounts {
		if m.Kind == guestspec.MountVolume {
			b := m.Name + ":" + m.Target
			if m.ReadOnly {
				b += ":ro"
			}
			hc.Binds = append(hc.Binds, b)
		}
		if m.Kind == guestspec.MountTmpfs {
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs[m.Target] = m.Options
		}
	}
	for _, h := range cfg.Hosts {
		if f := strings.Fields(h); len(f) >= 2 {
			for _, name := range f[1:] {
				hc.ExtraHosts = append(hc.ExtraHosts, name+":"+f[0])
			}
		}
	}
	resp.HostConfig = hc
	resp.NetworkSettings = &dcontainer.NetworkSettings{Ports: ports, Networks: s.endpointsOf(c)}
	if size {
		if n, err := allocatedSize(c.RW()); err == nil {
			resp.SizeRw = &n
		}
	}
	return resp
}

func (s *apiServer) postContainerStart(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	if c.State.Status == container.Running || c.State.Status == container.Restarting {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if c.State.Status == container.Stopped && c.Config.AutoRemove {
		return conflict("container %s was run with --rm, and cannot be started again", c.Short())
	}
	if err := startContainerIO(r.Context(), s.containers, c.ID, false); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// stopParams reads stop's and restart's t (or timeout) and signal.
func stopParams(r *http.Request) (time.Duration, int, error) {
	q := r.URL.Query()
	t := q.Get("t")
	if t == "" {
		t = q.Get("timeout")
	}
	timeout := time.Duration(-1)
	if t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < -1 {
			return 0, 0, badRequest("invalid timeout %q", t)
		}
		if n >= 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	sig := 0
	if v := q.Get("signal"); v != "" {
		n, err := guestspec.ParseSignal(v)
		if err != nil {
			return 0, 0, badRequest("%v", err)
		}
		sig = n
	}
	return timeout, sig, nil
}

func (s *apiServer) postContainerStop(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	timeout, sig, err := stopParams(r)
	if err != nil {
		return err
	}
	if !c.State.Active() || c.State.Status == container.Created {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if err := stopContainerWith(r.Context(), s.containers, c.ID, timeout, sig); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postContainerRestart(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	timeout, sig, err := stopParams(r)
	if err != nil {
		return err
	}
	if c.Config.AutoRemove {
		return conflict("container %s was run with --rm, and would be gone once stopped", c.Short())
	}
	if err := stopContainerWith(r.Context(), s.containers, c.ID, timeout, sig); err != nil {
		return err
	}
	if err := startContainerIO(r.Context(), s.containers, c.ID, false); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postContainerKill(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	sig := guestspec.SIGKILL
	sigName := "SIGKILL"
	if v := r.URL.Query().Get("signal"); v != "" {
		if sig, err = guestspec.ParseSignal(v); err != nil {
			return badRequest("%v", err)
		}
		sigName = v
	}
	if !c.State.Active() {
		return conflict("Cannot kill container: %s: container %s is not running", r.PathValue("id"), c.ID)
	}
	if err := ask(r.Context(), s.containers, c, container.Request{Op: container.OpKill, Signal: sig}, 10*time.Second); err != nil {
		if errors.Is(err, container.ErrNotRunning) {
			return conflict("Cannot kill container: %s: container %s is not running", r.PathValue("id"), c.ID)
		}
		return err
	}
	s.emitContainer(c, "kill", map[string]string{"signal": strconv.Itoa(sig), "signalName": sigName})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postContainerPause(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	op := container.OpPause
	if strings.HasSuffix(r.URL.Path, "/unpause") {
		op = container.OpUnpause
	}
	if err := askPause(s.root, c, op); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postContainerRename(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if name == "" {
		return badRequest("Neither old nor new names may be empty")
	}
	if err := s.containers.Rename(c, name); err != nil {
		if errors.Is(err, container.ErrNameInUse) {
			return conflict("Conflict. The container name \"/%s\" is already in use. You have to remove (or rename) that container to be able to reuse that name.", name)
		}
		if httpStatus(err) == http.StatusInternalServerError {
			return badRequest("%v", err)
		}
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) deleteContainer(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("id")
	c, err := s.resolve(ref)
	if err != nil {
		return err
	}
	if boolParam(r, "link") {
		return badRequest("runwhp has no links to remove")
	}
	if err := deleteContainer(s.containers, c.ID, boolParam(r, "force")); err != nil {
		if errors.Is(err, container.ErrRunning) {
			return conflict("cannot remove container \"%s\": container is %s: stop the container before removing or force remove", containerName(c), stateWord(c.State))
		}
		return err
	}
	s.execs.dropContainer(c.ID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postContainerWait(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	cond := r.URL.Query().Get("condition")
	switch cond {
	case "", "not-running", "next-exit", "removed":
	default:
		return badRequest("invalid condition: %q", cond)
	}
	// The headers go now, so that the client knows the wait is in place
	// before it starts the container.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	code, werr := s.waitFor(r.Context(), c, cond)
	resp := dcontainer.WaitResponse{StatusCode: int64(code)}
	if werr != nil {
		if r.Context().Err() != nil {
			return nil
		}
		resp.Error = &dcontainer.WaitExitError{Message: werr.Error()}
	}
	data, _ := json.Marshal(resp)
	w.Write(append(data, '\n'))
	return nil
}

// waitFor waits for c as a wait with condition asks.
func (s *apiServer) waitFor(ctx context.Context, c *container.Container, cond string) (int, error) {
	if cond == "" || cond == "not-running" {
		if !c.State.Active() || c.State.Status == container.Created || c.State.Status == container.Creating {
			return c.State.ExitCode, nil
		}
		return waitContainer(ctx, s.containers, c.ID)
	}
	// next-exit, and removed: the container's next end, even if it is
	// stopped now and someone starts it later.
	for !c.State.Active() {
		if cond == "removed" {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		cur, err := s.containers.Get(c.ID)
		if err != nil {
			return c.State.ExitCode, nil // removed meanwhile
		}
		c = cur
	}
	code := c.State.ExitCode
	if c.State.Active() {
		var err error
		if code, err = waitContainer(ctx, s.containers, c.ID); err != nil {
			if errors.Is(err, container.ErrNotFound) {
				return code, nil
			}
			return code, err
		}
	}
	if cond != "removed" {
		return code, nil
	}
	for {
		if _, err := s.containers.Get(c.ID); errors.Is(err, container.ErrNotFound) {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *apiServer) postContainerResize(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	h, err1 := intParam(r, "h", 0)
	wd, err2 := intParam(r, "w", 0)
	if err1 != nil || err2 != nil || h <= 0 || wd <= 0 || h > 0xffff || wd > 0xffff {
		return badRequest("invalid terminal size")
	}
	ctl := guestspec.Control{Rows: uint16(h), Cols: uint16(wd)}
	if err := ask(r.Context(), s.containers, c, container.Request{Op: container.OpControl, Control: &ctl}, 5*time.Second); err != nil {
		if errors.Is(err, container.ErrNotRunning) {
			return conflict("cannot resize container %s: it is not running", c.Short())
		}
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *apiServer) postContainerUpdate(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	var req dcontainer.UpdateConfig
	if err := readJSON(r, &req); err != nil {
		return err
	}
	same := req.RestartPolicy.Name == "" || string(req.RestartPolicy.Name) == c.Config.Restart.Name ||
		req.RestartPolicy.Name == "no" && !c.Config.Restart.IsSet()
	zero := req.Resources.Memory == 0 && req.Resources.NanoCPUs == 0 && req.Resources.CPUCount == 0 &&
		req.Resources.CPUShares == 0 && req.Resources.PidsLimit == nil && req.Resources.MemorySwap == 0
	if !same || !zero {
		return badRequest("runwhp cannot change a container's configuration once it is made: make it again with what it should have")
	}
	writeJSON(w, dcontainer.UpdateResponse{Warnings: []string{}})
	return nil
}

func (s *apiServer) getContainerLogs(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	q := r.URL.Query()
	stdout, stderr := boolParam(r, "stdout"), boolParam(r, "stderr")
	if !stdout && !stderr {
		return badRequest("Bad parameters: you must choose at least one stream")
	}
	follow := boolParam(r, "follow")
	f := container.LogFilter{Timestamps: boolParam(r, "timestamps"), Tail: -1}
	now := time.Now()
	if t := q.Get("tail"); t != "" && t != "all" {
		if f.Tail, err = parseTail(t); err != nil {
			return badRequest("%v", err)
		}
	}
	if f.Since, err = apiTime(q.Get("since"), now); err != nil {
		return err
	}
	if f.Until, err = apiTime(q.Get("until"), now); err != nil {
		return err
	}
	files, _ := container.LogFiles(c.Dir())
	if len(files) == 0 && !c.Config.KeepsLog() {
		return badRequest("configured logging driver does not support reading")
	}
	tty := c.Config.TTY
	ctype := dockerapiMultiplexed
	if tty {
		ctype = dockerapiRaw
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	out := &lockedWriter{w: w}
	var ow, ew io.Writer = io.Discard, io.Discard
	if stdout {
		ow = streamWriter(out, container.Stdout, tty)
	}
	if stderr {
		ew = streamWriter(out, container.Stderr, tty)
	}
	if len(files) == 0 && !(follow && c.State.Active()) {
		return nil
	}
	p, err := container.NewLogPrinter(c.Dir(), ow, ew, f)
	if err != nil {
		return err
	}
	defer p.Close()
	ctx := r.Context()
	for {
		if _, err := p.Copy(); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !follow || p.Done || (!f.Until.IsZero() && time.Now().After(f.Until)) {
			return nil
		}
		if !c.Locked() {
			_, err := p.Copy()
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// apiTime reads a since or until parameter: Unix seconds, with a fraction,
// or a time as Docker's client sends it.
func apiTime(v string, now time.Time) (time.Time, error) {
	if v == "" || v == "0" {
		return time.Time{}, nil
	}
	if sec, frac, ok := strings.Cut(v, "."); isDigits(sec) && (!ok || isDigits(frac)) {
		n, _ := strconv.ParseInt(sec, 10, 64)
		var ns int64
		if ok {
			frac = (frac + "000000000")[:9]
			ns, _ = strconv.ParseInt(frac, 10, 64)
		}
		return time.Unix(n, ns), nil
	}
	t, err := parseLogTime(v, now)
	if err != nil {
		return t, badRequest("%v", err)
	}
	return t, nil
}

const (
	dockerapiRaw         = "application/vnd.docker.raw-stream"
	dockerapiMultiplexed = "application/vnd.docker.multiplexed-stream"
)

// lockedWriter serialises writes from several streams.
type lockedWriter struct {
	mu chanMutex
	w  io.Writer
}

type chanMutex struct{ ch chan struct{} }

func (m *chanMutex) lock() {
	if m.ch == nil {
		m.ch = make(chan struct{}, 1)
	}
	m.ch <- struct{}{}
}
func (m *chanMutex) unlock() { <-m.ch }

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.lock()
	defer l.mu.unlock()
	return l.w.Write(p)
}

// streamWriter writes one of a container's streams to w: as it is with a
// terminal, and otherwise in Docker's multiplexed framing, which is the
// log's.
func streamWriter(w io.Writer, stream byte, tty bool) io.Writer {
	if tty {
		return w
	}
	return frameWriter{w: w, kind: stream}
}

type frameWriter struct {
	w    io.Writer
	kind byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if _, err := f.w.Write(container.AppendFrame(nil, f.kind, p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *apiServer) postContainersPrune(w http.ResponseWriter, r *http.Request) error {
	m, err := dockerapi.DecodeFilters(r.URL.Query().Get("filters"))
	if err != nil {
		return badRequest("%v", err)
	}
	if err := dockerapi.CheckFilterKeys(m, "label", "label!", "until"); err != nil {
		return badRequest("%v", err)
	}
	if len(m["until"]) > 0 {
		return badRequest("runwhp's prunes take no until filter yet")
	}
	cs, err := s.containers.List()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, c := range cs {
		if !labelFilters(m).matchContainerLabels(c.Config.Labels) {
			keep[c.ID] = true
		}
	}
	ids, freed, errs := pruneContainersExcept(s.root, keep)
	if len(errs) > 0 {
		s.logf("container prune: %v", errors.Join(errs...))
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, dcontainer.PruneReport{ContainersDeleted: ids, SpaceReclaimed: uint64(max(freed, 0))})
	return nil
}

// labelFilters are a prune's label and label! filters.
type labelFilters map[string][]string

func (f labelFilters) matchContainerLabels(labels map[string]string) bool {
	return filters{"label": f["label"]}.matchLabels(labels) && notLabels(f["label!"], labels)
}

// notLabels reports whether labels has none of the KEY or KEY=VALUE given.
func notLabels(not []string, labels map[string]string) bool {
	for _, l := range not {
		k, v, hasValue := strings.Cut(l, "=")
		got, ok := labels[k]
		if ok && (!hasValue || got == v) {
			return false
		}
	}
	return true
}

// The files of a container: in this process, through the runwhp running
// it, when it has a VM; otherwise in a job, which boots a guest with its
// storage.

// errNeedsGuest says a container's files can only be reached by booting
// a guest, which the server leaves to a job.
var errNeedsGuest = errors.New("the container has no VM: its files need a guest of their own")

type fileJobArgs struct {
	Ref                  string `json:"ref"`
	Path                 string `json:"path,omitempty"`
	NoOverwriteDirNonDir bool   `json:"noOverwriteDirNonDir,omitempty"`
	CopyUIDGID           bool   `json:"copyUIDGID,omitempty"`
	// PipeOnly refuses with errNeedsGuest rather than boot a guest.
	PipeOnly bool `json:"pipeOnly,omitempty"`
}

func init() {
	apiJobs["archive-stat"] = jobArchiveStat
	apiJobs["archive-get"] = jobArchiveGet
	apiJobs["archive-put"] = jobArchivePut
	apiJobs["export"] = jobExport
	apiJobs["changes"] = jobChanges
}

// openJobRoot reaches the files of the container a file job names.
func openJobRoot(ctx context.Context, root string, a fileJobArgs) (*rootClient, error) {
	store := container.Open(root)
	c, err := store.Resolve(a.Ref)
	if errors.Is(err, container.ErrNotFound) {
		return nil, notFound("No such container: %s", a.Ref)
	}
	if err != nil {
		return nil, err
	}
	if a.PipeOnly {
		if c.State.Status != container.Created && c.State.Status != container.Running {
			return nil, errNeedsGuest
		}
		return pipeRoot(root, c, a.Ref), nil
	}
	return openRoot(ctx, store, root, a.Ref)
}

func jobArchiveStat(ctx context.Context, root string, args json.RawMessage, _ io.Reader, _ jobSink) (any, error) {
	var a fileJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	rc, err := openJobRoot(ctx, root, a)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	return rc.Stat(ctx, a.Path)
}

func jobArchiveGet(ctx context.Context, root string, args json.RawMessage, _ io.Reader, out jobSink) (any, error) {
	var a fileJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	rc, err := openJobRoot(ctx, root, a)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	tr, st, err := rc.Get(ctx, a.Path)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	data, _ := json.Marshal(st)
	if err := out.Message(data); err != nil {
		return nil, err
	}
	return nil, copyTar(out, tr)
}

func jobArchivePut(ctx context.Context, root string, args json.RawMessage, in io.Reader, _ jobSink) (any, error) {
	var a fileJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	rc, err := openJobRoot(ctx, root, a)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	st, err := rc.Stat(ctx, a.Path)
	if err != nil {
		return nil, err
	}
	if !st.Mode.IsDir() {
		return nil, badRequest("extraction point is not a directory")
	}
	return nil, rc.Put(ctx, a.Path, in, rootfs.PutOptions{NoOverwriteDirNonDir: a.NoOverwriteDirNonDir, CopyUIDGID: a.CopyUIDGID})
}

func jobExport(ctx context.Context, root string, args json.RawMessage, _ io.Reader, out jobSink) (any, error) {
	var a fileJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	rc, err := openJobRoot(ctx, root, a)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	tr, err := rc.Tar(ctx, guestspec.RootExport)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	return nil, copyTar(out, tr)
}

func jobChanges(ctx context.Context, root string, args json.RawMessage, _ io.Reader, _ jobSink) (any, error) {
	var a fileJobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	rc, err := openJobRoot(ctx, root, a)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	cs, err := rc.Changes(ctx)
	if err != nil {
		return nil, err
	}
	out := []dcontainer.FilesystemChange{}
	for _, c := range cs {
		out = append(out, dcontainer.FilesystemChange{Path: c.Path, Kind: dcontainer.ChangeType(c.Kind)})
	}
	return out, nil
}

// localSink is a jobSink for a job done in this process.
type localSink struct {
	data    io.Writer
	message func([]byte) error
}

func (l localSink) Write(p []byte) (int, error) {
	if l.data == nil {
		return len(p), nil
	}
	return l.data.Write(p)
}

func (l localSink) Message(line []byte) error {
	if l.message == nil {
		return nil
	}
	return l.message(line)
}

// fileJob does a file job on a container: here, through its runwhp, if it
// has a VM, and otherwise in a job of its own.
func (s *apiServer) fileJob(ctx context.Context, kind string, a fileJobArgs, body io.Reader, data io.Writer, message func([]byte) error) (json.RawMessage, error) {
	local := a
	local.PipeOnly = true
	argData, _ := json.Marshal(local)
	v, err := apiJobs[kind](ctx, s.root, argData, body, localSink{data: data, message: message})
	if !errors.Is(err, errNeedsGuest) {
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	return s.jobs.run(ctx, kind, a, body, data, message)
}

func (s *apiServer) archiveArgs(r *http.Request) (fileJobArgs, error) {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return fileJobArgs{}, err
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		return fileJobArgs{}, badRequest("path is required")
	}
	return fileJobArgs{Ref: c.ID, Path: p}, nil
}

func pathStatHeader(st *guestspec.PathStat) string {
	data, _ := json.Marshal(st)
	return base64.StdEncoding.EncodeToString(data)
}

func (s *apiServer) headContainerArchive(w http.ResponseWriter, r *http.Request) error {
	a, err := s.archiveArgs(r)
	if err != nil {
		return err
	}
	res, err := s.fileJob(r.Context(), "archive-stat", a, nil, nil, nil)
	if err != nil {
		return err
	}
	var st guestspec.PathStat
	if err := json.Unmarshal(res, &st); err != nil {
		return err
	}
	w.Header().Set("X-Docker-Container-Path-Stat", pathStatHeader(&st))
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *apiServer) getContainerArchive(w http.ResponseWriter, r *http.Request) error {
	a, err := s.archiveArgs(r)
	if err != nil {
		return err
	}
	started := false
	_, err = s.fileJob(r.Context(), "archive-get", a, nil, w, func(line []byte) error {
		var st guestspec.PathStat
		if err := json.Unmarshal(line, &st); err != nil {
			return err
		}
		w.Header().Set("X-Docker-Container-Path-Stat", pathStatHeader(&st))
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		started = true
		return nil
	})
	if err != nil && started {
		s.logf("archive of %s in %s ended early: %v", a.Path, a.Ref, err)
		return nil
	}
	return err
}

func (s *apiServer) putContainerArchive(w http.ResponseWriter, r *http.Request) error {
	a, err := s.archiveArgs(r)
	if err != nil {
		return err
	}
	a.NoOverwriteDirNonDir = boolParam(r, "noOverwriteDirNonDir")
	a.CopyUIDGID = boolParam(r, "copyUIDGID")
	if _, err := s.fileJob(r.Context(), "archive-put", a, r.Body, nil, nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *apiServer) getContainerExport(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	header := false
	_, err = s.fileJob(r.Context(), "export", fileJobArgs{Ref: c.ID}, nil, writerFunc(func(p []byte) (int, error) {
		if !header {
			w.Header().Set("Content-Type", "application/x-tar")
			w.WriteHeader(http.StatusOK)
			header = true
		}
		return w.Write(p)
	}), nil)
	if err != nil && header {
		s.logf("export of %s ended early: %v", c.ID, err)
		return nil
	}
	return err
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// getContainerTop lists a running container's processes, as init sees
// them, in the columns ps -ef or ps aux would give.
func (s *apiServer) getContainerTop(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	procs, err := containerTop(r.Context(), s.root, c)
	if err != nil {
		return err
	}
	titles, rows, err := dockerapi.Top(procs, r.URL.Query().Get("ps_args"), time.Local)
	if err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, dcontainer.TopResponse{Titles: titles, Processes: rows})
	return nil
}

func (s *apiServer) getContainerChanges(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	res, err := s.fileJob(r.Context(), "changes", fileJobArgs{Ref: c.ID}, nil, nil, nil)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(append(res, '\n'))
	return nil
}

var _ = fmt.Sprintf
