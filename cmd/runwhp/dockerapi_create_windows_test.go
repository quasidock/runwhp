//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	dcontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dnetwork "github.com/moby/moby/api/types/network"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/network"
)

// createRun is what docker run alpine:3.20 true sends, as the server
// read it.
func createRun(t *testing.T) dcontainer.CreateRequest {
	t.Helper()
	data, err := os.ReadFile("testdata/dockerapi/create-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var req dcontainer.CreateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSpecFromCreateDefaults(t *testing.T) {
	req := createRun(t)
	s, warnings, err := specFromCreate(req)
	if err != nil {
		t.Fatalf("docker run's own request refused: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings for docker run's own request: %q", warnings)
	}
	if s.image != "alpine:3.20" || len(s.command) != 1 || s.command[0] != "true" || s.entrypointSet || !s.rm {
		t.Errorf("spec %+v", s)
	}
	if len(s.network.attach) != 0 || len(s.network.publish) != 0 || s.lookupEnv != nil {
		t.Errorf("network %+v, lookupEnv set %v", s.network, s.lookupEnv != nil)
	}
}

func TestSpecFromCreate(t *testing.T) {
	req := createRun(t)
	req.Config.Entrypoint = []string{""}
	req.Config.Env = []string{"A=1", "BARE"}
	req.Config.Healthcheck = &dcontainer.HealthConfig{Test: []string{"CMD", "true"}, Interval: 2 * time.Second, Retries: 2}
	req.Config.Volumes = map[string]struct{}{"/anon": {}, "/data": {}}
	stop := 3
	req.Config.StopTimeout = &stop
	req.Config.StopSignal = "SIGINT"
	hc := req.HostConfig
	hc.AutoRemove = false
	hc.Binds = []string{`C:\src:/src:ro`, "vol:/data"}
	hc.Mounts = []mount.Mount{{Type: mount.TypeTmpfs, Target: "/scratch", TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 1 << 20}}}
	hc.PortBindings = dnetwork.PortMap{
		dnetwork.MustParsePort("80/tcp"): {{HostPort: "8080"}},
		dnetwork.MustParsePort("443"):    {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8443"}},
		// Random host ports, as Docker Compose and testcontainers-go ask.
		dnetwork.MustParsePort("5432/tcp"): {{}},
		dnetwork.MustParsePort("6379/tcp"): {{HostPort: "0"}},
		dnetwork.MustParsePort("5353/udp"): {{HostPort: "53"}},
		// No bindings: only exposed.
		dnetwork.MustParsePort("9000/tcp"): {},
	}
	req.Config.ExposedPorts = dnetwork.PortSet{dnetwork.MustParsePort("53/udp"): {}, dnetwork.MustParsePort("7000/tcp"): {}}
	hc.PublishAllPorts = true
	hc.NetworkMode = "app"
	req.NetworkingConfig = &dnetwork.NetworkingConfig{EndpointsConfig: map[string]*dnetwork.EndpointSettings{
		"app":   {Aliases: []string{"web"}, IPAMConfig: &dnetwork.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("172.30.0.9")}},
		"other": {},
	}}
	hc.NanoCPUs = 1_500_000_000
	hc.Memory = 512 << 20
	hc.RestartPolicy = dcontainer.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}
	hc.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	hc.LogConfig = dcontainer.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m"}}
	hc.Init = new(bool)
	*hc.Init = true

	s, warnings, err := specFromCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	if !s.entrypointSet || s.entrypoint != nil {
		t.Errorf("entrypoint %q set %v: want it reset", s.entrypoint, s.entrypointSet)
	}
	if s.cpus != 2 || s.memory != 512<<20 || s.restart.String() != "on-failure:3" {
		t.Errorf("cpus %d, memory %d, restart %v", s.cpus, s.memory, s.restart)
	}
	if s.stopSignal != "SIGINT" || s.stopTimeout == nil || *s.stopTimeout != 3 {
		t.Errorf("stop %q %v", s.stopSignal, s.stopTimeout)
	}
	if s.health.retries != 2 || s.health.interval != 2*time.Second {
		t.Errorf("health %+v", s.health)
	}
	if len(s.network.attach) != 2 || s.network.attach[0].name != "app" || s.network.attach[0].ip != "172.30.0.9" ||
		len(s.network.attach[0].aliases) != 1 || s.network.attach[1].name != "other" {
		t.Errorf("networks %+v", s.network.attach)
	}
	ports := map[string]bool{}
	for _, p := range s.network.publish {
		ports[p.String()] = true
	}
	if !ports["127.0.0.1:8080->80/tcp"] || !ports["0.0.0.0:8443->443/tcp"] || !ports["127.0.0.1:0->5432/tcp"] ||
		!ports["127.0.0.1:0->6379/tcp"] || !ports["127.0.0.1:53->5353/udp"] || len(ports) != 5 || !s.network.hostGateway {
		t.Errorf("ports %v, host gateway %v", s.network.publish, s.network.hostGateway)
	}
	exposed := exposedPorts(nil, s.network.expose, nil)
	if want := []string{"53/udp", "80/tcp", "443/tcp", "5353/udp", "5432/tcp", "6379/tcp", "7000/tcp", "9000/tcp"}; !reflect.DeepEqual(exposed, want) || !s.network.publishAll {
		t.Errorf("exposed %q, publish all %v; want %q", exposed, s.network.publishAll, want)
	}
	kinds := map[string]string{}
	for _, m := range s.mounts {
		kinds[m.target] = m.kind + ":" + m.source
	}
	want := map[string]string{"/src": guestspec.MountBind + `:C:\src`, "/data": guestspec.MountVolume + ":vol",
		"/scratch": guestspec.MountTmpfs + ":", "/anon": guestspec.MountVolume + ":"}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("mount %s: %q, want %q", k, kinds[k], v)
		}
	}
	if len(s.mounts) != 4 {
		t.Errorf("mounts %+v", s.mounts)
	}
	joined := strings.Join(warnings, "\n")
	if s.logs.driver != "json-file" || s.logs.maxSize != 1<<20 {
		t.Errorf("logs %+v", s.logs)
	}
	for _, w := range []string{"rounded up to 2", "--init"} {
		if !strings.Contains(joined, w) {
			t.Errorf("no warning of %s in %q", w, warnings)
		}
	}
}

func TestSpecFromCreateBridgeAliases(t *testing.T) {
	req := createRun(t)
	req.HostConfig.NetworkMode = "bridge"
	s, _, err := specFromCreate(req)
	if err != nil || len(s.network.attach) != 0 {
		t.Fatalf("bridge: %+v, %v", s.network.attach, err)
	}
	req.NetworkingConfig = &dnetwork.NetworkingConfig{EndpointsConfig: map[string]*dnetwork.EndpointSettings{
		"bridge": {IPAMConfig: &dnetwork.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.0.2.20")}}}}
	s, _, err = specFromCreate(req)
	if err != nil || len(s.network.attach) != 1 || s.network.attach[0].name != network.User {
		t.Fatalf("bridge with an address: %+v, %v", s.network.attach, err)
	}
}

// Docker's socket, as Testcontainers' Ryuk, Docker Compose's
// use_api_socket and Dev Containers' docker-outside-of-docker bind it, and
// named pipes, are sockets.
func TestSpecFromCreateSockets(t *testing.T) {
	req := createRun(t)
	req.HostConfig.Binds = []string{"//./pipe/runwhp_engine-e2e-1:/var/run/docker.sock", "/var/run/docker.sock:/run/d.sock:rw"}
	req.HostConfig.Mounts = []mount.Mount{
		{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker-host.sock"},
		{Type: mount.TypeNamedPipe, Source: `\\.\pipe\openssh-ssh-agent`, Target: "/run/ssh.sock", ReadOnly: true},
	}
	s, _, err := specFromCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range s.mounts {
		if m.kind == guestspec.MountSocket {
			got = append(got, m.source+" "+m.pipe+" "+m.target)
		}
	}
	want := []string{
		`//./pipe/runwhp_engine-e2e-1 \\.\pipe\runwhp_engine-e2e-1 /var/run/docker.sock`,
		`/var/run/docker.sock  /run/d.sock`,
		`/var/run/docker.sock  /var/run/docker-host.sock`,
		`\\.\pipe\openssh-ssh-agent \\.\pipe\openssh-ssh-agent /run/ssh.sock`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("sockets\n got %q\nwant %q", got, want)
	}
	req.HostConfig.Binds, req.HostConfig.Mounts = nil, []mount.Mount{{Type: mount.TypeNamedPipe, Source: `C:\x`, Target: "/x"}}
	if _, _, err := specFromCreate(req); err == nil || !strings.Contains(err.Error(), "not a named pipe") {
		t.Errorf("npipe of a file: %v", err)
	}
	// What docker-outside-of-docker asks besides: SELinux labels, which
	// change nothing.
	req.HostConfig.Mounts, req.HostConfig.SecurityOpt = nil, []string{"label=disable", "label:type:x"}
	if _, _, err := specFromCreate(req); err != nil {
		t.Errorf("label options: %v", err)
	}
}

// A volume a mount makes gets the mount's labels, as Docker gives them:
// Testcontainers' Ryuk finds the volumes of a session by them.
func TestSpecFromCreateVolumeLabels(t *testing.T) {
	req := createRun(t)
	req.HostConfig.Binds = nil
	req.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "tc-data", Target: "/data",
		VolumeOptions: &mount.VolumeOptions{Labels: map[string]string{"org.testcontainers.sessionId": "s1"}}}}
	s, _, err := specFromCreate(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planMounts(s.mounts, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.volumes) != 1 || plan.volumes[0].labels["org.testcontainers.sessionId"] != "s1" {
		t.Errorf("volumes %+v", plan.volumes)
	}
}

func TestSpecFromCreateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*dcontainer.CreateRequest)
		want string
	}{
		{"seccomp profile", func(r *dcontainer.CreateRequest) { r.HostConfig.SecurityOpt = []string{"seccomp=/p.json"} }, "--security-opt seccomp=/p.json"},
		{"cap-add", func(r *dcontainer.CreateRequest) { r.HostConfig.CapAdd = []string{"NET_ADMIN"} }, "--cap-add"},
		{"host network", func(r *dcontainer.CreateRequest) { r.HostConfig.NetworkMode = "host" }, "--network host"},
		{"container network", func(r *dcontainer.CreateRequest) { r.HostConfig.NetworkMode = "container:x" }, "container:x"},
		{"dns", func(r *dcontainer.CreateRequest) { r.HostConfig.DNS = []netip.Addr{netip.MustParseAddr("1.1.1.1")} }, "--dns"},
		{"sctp", func(r *dcontainer.CreateRequest) {
			r.HostConfig.PortBindings = dnetwork.PortMap{dnetwork.MustParsePort("132/sctp"): {{HostPort: "132"}}}
		}, "sctp ports cannot be published"},
		{"relative bind", func(r *dcontainer.CreateRequest) { r.HostConfig.Binds = []string{"./src:/src"} }, "relative"},
		{"rm and restart", func(r *dcontainer.CreateRequest) {
			r.HostConfig.RestartPolicy = dcontainer.RestartPolicy{Name: "always"}
		}, "--restart and --rm"},
		{"no image", func(r *dcontainer.CreateRequest) { r.Config.Image = "" }, "no image"},
		{"unconfined and other", func(r *dcontainer.CreateRequest) {
			r.HostConfig.SecurityOpt = []string{"label=disable", "seccomp=unconfined", "apparmor:unconfined", "systempaths=unconfined"}
		},
			"--security-opt systempaths=unconfined is"},
	} {
		req := createRun(t)
		tc.edit(&req)
		_, _, err := specFromCreate(req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
			continue
		}
		if httpStatus(err) != http.StatusBadRequest {
			t.Errorf("%s: status %d", tc.name, httpStatus(err))
		}
	}
}

func TestPipePath(t *testing.T) {
	for _, in := range []string{"runwhp_engine-x", `\\.\pipe\runwhp_engine-x`, "//./pipe/runwhp_engine-x", `\.\pipe\runwhp_engine-x`, `\\.\PIPE\runwhp_engine-x`} {
		if got := pipePath(in); got != `\\.\pipe\runwhp_engine-x` {
			t.Errorf("pipePath(%q) = %q", in, got)
		}
	}
}
