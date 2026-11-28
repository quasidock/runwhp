package ocispec

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
)

// The specs in testdata are what nerdctl 2.4.2 sent a shim on Windows, for
// alpine:3.20 and redis:7-alpine, with this machine's paths made neutral:
//
//	plain.json         nerdctl run alpine echo hi
//	detached.json      nerdctl run -d -p 8080:80 -p 127.0.0.1:8081:81/tcp -e FOO=bar -w /tmp
//	                     --hostname h1 --cpus 2 --memory 512m -u 1000:1000 alpine sleep 100
//	mounts.json        nerdctl run -v C:\bind:C:\data -v C:\bind:C:\ro:ro -v vol1:C:\vol alpine echo v
//	image-volume.json  nerdctl run -w /tmp redis:7-alpine echo iv (VOLUME /data)
//
// All were run with --network none, --runtime and --platform linux/amd64.

// loadSpec reads a spec from testdata, with the files it names moved to
// dir: its state directory, holding network-config.json, and each mount's
// source, made as a directory.
func loadSpec(t *testing.T, name string, portMappings string) *specs.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var s specs.Spec
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if portMappings != "" {
		if err := os.WriteFile(filepath.Join(dir, "network-config.json"), []byte(portMappings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.Annotations[stateDirAnnotation] = dir
	for i := range s.Mounts {
		src := filepath.Join(dir, "src", filepath.Base(s.Mounts[i].Source))
		os.MkdirAll(src, 0o755)
		s.Mounts[i].Source = src
	}
	return &s
}

var testRootfs = []Mount{{Type: LayerMountType, Source: `C:\snap\3`,
	Options: []string{"rw", `parentLayerPaths=["C:\\snap\\2","C:\\snap\\1"]`}}}

func TestPlain(t *testing.T) {
	cfg, err := Config(Task{Namespace: "default", ID: "f42fb0e7", Spec: loadSpec(t, "plain.json", ""), Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Args, []string{"echo", "hi"}) {
		t.Errorf("args %q", cfg.Args)
	}
	if !reflect.DeepEqual(cfg.Env, []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}) {
		t.Errorf("env %q: the empty HOSTNAME is to go", cfg.Env)
	}
	if cfg.Cwd != "/" || cfg.User != "" || cfg.Hostname != "" || cfg.TTY || cfg.Stdin {
		t.Errorf("cwd %q user %q hostname %q tty %v stdin %v", cfg.Cwd, cfg.User, cfg.Hostname, cfg.TTY, cfg.Stdin)
	}
	if !cfg.Network || len(cfg.Publish) != 0 || cfg.Memory != 0 || cfg.CPUs != 0 {
		t.Errorf("network %v publish %v memory %d cpus %d", cfg.Network, cfg.Publish, cfg.Memory, cfg.CPUs)
	}
	if want := []string{`C:\snap\1\layer.vhd`, `C:\snap\2\layer.vhd`}; !reflect.DeepEqual(cfg.LayerFiles, want) {
		t.Errorf("layers %q, want bottom first %q", cfg.LayerFiles, want)
	}
	if cfg.RWPath != `C:\snap\3\rw.ext4` {
		t.Errorf("writable layer %q", cfg.RWPath)
	}
	if cfg.LogDriver != "none" || cfg.Containerd == nil || cfg.Containerd.Namespace != "default" || cfg.Containerd.ID != "f42fb0e7" {
		t.Errorf("log driver %q, containerd %+v", cfg.LogDriver, cfg.Containerd)
	}
	if len(cfg.Shares) != 0 || len(cfg.Mounts) != 0 {
		t.Errorf("shares %+v mounts %+v", cfg.Shares, cfg.Mounts)
	}
}

func TestDetached(t *testing.T) {
	ports := `{"portMappings":[{"HostPort":8080,"ContainerPort":80,"Protocol":"tcp","HostIP":"0.0.0.0"},` +
		`{"HostPort":8081,"ContainerPort":81,"Protocol":"tcp","HostIP":"127.0.0.1"},` +
		`{"HostPort":5353,"ContainerPort":53,"Protocol":"udp","HostIP":"0.0.0.0"}]}`
	cfg, err := Config(Task{Spec: loadSpec(t, "detached.json", ports), Rootfs: testRootfs, Stdin: true, Terminal: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "1000:1000" || cfg.Cwd != "/tmp" || cfg.Hostname != "h1" {
		t.Errorf("user %q cwd %q hostname %q", cfg.User, cfg.Cwd, cfg.Hostname)
	}
	if cfg.Memory != 512<<20 || cfg.CPUs != 2 {
		t.Errorf("memory %d cpus %d", cfg.Memory, cfg.CPUs)
	}
	if !strings.Contains(strings.Join(cfg.Env, " "), "FOO=bar") || !strings.Contains(strings.Join(cfg.Env, " "), "HOSTNAME=h1") {
		t.Errorf("env %q", cfg.Env)
	}
	if !cfg.Stdin || !cfg.StdinOnce || !cfg.TTY {
		t.Errorf("stdin %v once %v tty %v", cfg.Stdin, cfg.StdinOnce, cfg.TTY)
	}
	want := []netstack.Port{
		{Host: netip.MustParseAddrPort("0.0.0.0:8080"), Guest: 80},
		{Host: netip.MustParseAddrPort("127.0.0.1:8081"), Guest: 81},
		{Host: netip.MustParseAddrPort("0.0.0.0:5353"), Guest: 53, Proto: "udp"},
	}
	if !reflect.DeepEqual(cfg.Publish, want) {
		t.Errorf("publish %v", cfg.Publish)
	}
}

func TestMounts(t *testing.T) {
	s := loadSpec(t, "mounts.json", "")
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	type m struct {
		target         string
		readOnly, copy bool
	}
	var got []m
	for _, gm := range cfg.Mounts {
		if gm.Kind != guestspec.MountBind || cfg.Shares[gm.Share].Target != gm.Target || cfg.Shares[gm.Share].ReadOnly != gm.ReadOnly {
			t.Errorf("mount %+v, share %+v", gm, cfg.Shares[gm.Share])
		}
		got = append(got, m{gm.Target, gm.ReadOnly, gm.CopyUp})
	}
	want := []m{{"/data", false, false}, {"/ro", true, false}, {"/vol", false, true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts %+v, want %+v", got, want)
	}
	if cfg.Shares[2].Path != s.Mounts[2].Source {
		t.Errorf("volume share %q, want %q", cfg.Shares[2].Path, s.Mounts[2].Source)
	}
}

func TestImageVolume(t *testing.T) {
	cfg, err := Config(Task{Spec: loadSpec(t, "image-volume.json", ""), Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].Target != "/data" || !cfg.Mounts[0].CopyUp || cfg.Mounts[0].ReadOnly {
		t.Errorf("mounts %+v", cfg.Mounts)
	}
	if cfg.Args[0] != "docker-entrypoint.sh" || cfg.Cwd != "/tmp" {
		t.Errorf("args %q cwd %q", cfg.Args, cfg.Cwd)
	}
}

func TestNetworkNone(t *testing.T) {
	s := loadSpec(t, "plain.json", "")
	s.Annotations[NetworkAnnotation] = "none"
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil || cfg.Network {
		t.Errorf("network %v, %v", cfg != nil && cfg.Network, err)
	}
	s = loadSpec(t, "plain.json", `{"portMappings":[{"HostPort":8080,"ContainerPort":80,"Protocol":"tcp"}]}`)
	s.Annotations[NetworkAnnotation] = "none"
	if _, err := Config(Task{Spec: s, Rootfs: testRootfs}); err == nil {
		t.Error("none with a published port was taken")
	}
	s.Annotations[NetworkAnnotation] = "bridge"
	if _, err := Config(Task{Spec: s, Rootfs: testRootfs}); err == nil {
		t.Error("network bridge was taken")
	}
}

func TestPortErrors(t *testing.T) {
	for _, ports := range []string{
		`{"portMappings":[{"HostPort":132,"ContainerPort":132,"Protocol":"sctp"}]}`,
		`{"portMappings":[{"HostPort":-1,"ContainerPort":80,"Protocol":"tcp"}]}`,
		`{"portMappings":[{"HostPort":80,"ContainerPort":80,"Protocol":"tcp","HostIP":"nowhere"}]}`,
		`{"portMappings":`,
	} {
		if _, err := Config(Task{Spec: loadSpec(t, "plain.json", ports), Rootfs: testRootfs}); err == nil {
			t.Errorf("%s was taken", ports)
		}
	}
}

func TestExtraHosts(t *testing.T) {
	s := loadSpec(t, "plain.json", "")
	s.Annotations[extraHostsAnnotation] = `["db:10.1.2.3","host:host-gateway"]`
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Hosts, []string{"10.1.2.3\tdb", netstack.Gateway.String() + "\thost"}) || !cfg.HostGateway {
		t.Errorf("hosts %q gateway %v", cfg.Hosts, cfg.HostGateway)
	}
}

func TestRootfsErrors(t *testing.T) {
	s := loadSpec(t, "plain.json", "")
	for _, rootfs := range [][]Mount{
		nil,
		{{Type: "windows-layer", Source: `C:\x`}},
		{{Type: LayerMountType, Source: `C:\x`, Options: []string{"rw"}}},
		{{Type: LayerMountType, Source: `C:\x`, Options: []string{"rw", "parentLayerPaths=["}}},
		append(testRootfs, testRootfs...),
	} {
		if _, err := Config(Task{Spec: s, Rootfs: rootfs}); err == nil {
			t.Errorf("rootfs %+v was taken", rootfs)
		}
	}
	_, err := Config(Task{Spec: s, Rootfs: []Mount{{Type: "windows-layer"}}})
	if err == nil || !strings.Contains(err.Error(), "--snapshotter runwhp") {
		t.Errorf("the error for another snapshotter's mount does not say which to use: %v", err)
	}
}

func TestLinuxSpec(t *testing.T) {
	limit, quota, period := int64(256<<20), int64(150000), uint64(100000)
	s := &specs.Spec{
		Process:  &specs.Process{Args: []string{"sh"}, Cwd: "/work", Terminal: true},
		Hostname: "box",
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs"},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm"},
			{Destination: "/scratch", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "size=64m", "strictatime"}},
		},
		Linux: &specs.Linux{Resources: &specs.LinuxResources{
			Memory: &specs.LinuxMemory{Limit: &limit},
			CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period},
		}},
	}
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "box" || cfg.Cwd != "/work" || !cfg.TTY || cfg.Memory != 256<<20 || cfg.CPUs != 2 {
		t.Errorf("hostname %q cwd %q tty %v memory %d cpus %d", cfg.Hostname, cfg.Cwd, cfg.TTY, cfg.Memory, cfg.CPUs)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].Kind != guestspec.MountTmpfs || cfg.Mounts[0].Target != "/scratch" || cfg.Mounts[0].Options != "nosuid,size=64m" {
		t.Errorf("mounts %+v", cfg.Mounts)
	}
}

func TestLinuxPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\data`: "/data", `c:\a\b\`: "/a/b", `\data`: "/data", "/data": "/data", `C:/x/../y`: "/y", `D:\`: "/",
	} {
		if got, err := linuxPath(in); err != nil || got != want {
			t.Errorf("linuxPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"data", `C:data`, ""} {
		if got, err := linuxPath(in); err == nil {
			t.Errorf("linuxPath(%q) = %q", in, got)
		}
	}
}

func TestUser(t *testing.T) {
	for _, c := range []struct {
		u    specs.User
		want string
	}{
		{specs.User{}, ""},
		{specs.User{Username: "nobody"}, "nobody"},
		{specs.User{UID: 1000, GID: 1000}, "1000:1000"},
		{specs.User{UID: 1000}, "1000:0"},
		{specs.User{GID: 5}, "0:5"},
	} {
		if got := user(c.u); got != c.want {
			t.Errorf("user(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}

func TestNoProcess(t *testing.T) {
	for _, s := range []*specs.Spec{nil, {}, {Process: &specs.Process{}}} {
		if _, err := Config(Task{Spec: s, Rootfs: testRootfs}); err == nil {
			t.Errorf("spec %+v was taken", s)
		}
	}
	cfg, err := Config(Task{Spec: &specs.Spec{Process: &specs.Process{CommandLine: "cmd /c dir"}}, Rootfs: testRootfs})
	if err != nil || !reflect.DeepEqual(cfg.Args, []string{"cmd", "/c", "dir"}) {
		t.Errorf("command line: %q, %v", cfg.Args, err)
	}
}

func TestExec(t *testing.T) {
	// What nerdctl exec sent: the container's process with new args.
	p := &specs.Process{Args: []string{"echo", "e1"}, Cwd: "/tmp", User: specs.User{UID: 1000, GID: 1000},
		Env: []string{"PATH=/usr/bin", "FOO=bar", "HOSTNAME="}}
	ex, err := Exec(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ex.Args, []string{"echo", "e1"}) || ex.Cwd != "/tmp" || ex.User != "1000:1000" || !ex.TTY {
		t.Errorf("exec %+v", ex)
	}
	if !reflect.DeepEqual(ex.Env, []string{"PATH=/usr/bin", "FOO=bar"}) {
		t.Errorf("env %q", ex.Env)
	}
	if _, err := Exec(&specs.Process{}, false); err == nil {
		t.Error("an exec without args was taken")
	}
}

func TestUserMountOverImageVolume(t *testing.T) {
	s := loadSpec(t, "image-volume.json", "")
	user := filepath.Join(t.TempDir(), "named")
	os.MkdirAll(user, 0o755)
	// As nerdctl run -v named:C:\data redis:7-alpine asks: the user's
	// volume, and the image's VOLUME /data again, as \data.
	s.Mounts = append([]specs.Mount{{Destination: `C:\data`, Source: user, Options: []string{"rbind"}}}, s.Mounts...)
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Shares) != 1 || cfg.Shares[0].Path != user || len(cfg.Mounts) != 1 || cfg.Mounts[0].Target != "/data" {
		t.Errorf("shares %+v mounts %+v", cfg.Shares, cfg.Mounts)
	}
	// The other way round too.
	s.Mounts = append(s.Mounts[1:], s.Mounts[0])
	if cfg, err = Config(Task{Spec: s, Rootfs: testRootfs}); err != nil || cfg.Shares[0].Path != user {
		t.Errorf("image volume first: %+v, %v", cfg, err)
	}
	// Two of -v's on one target are refused.
	s.Mounts = append(s.Mounts, specs.Mount{Destination: `D:\data`, Source: user})
	if _, err := Config(Task{Spec: s, Rootfs: testRootfs}); err == nil {
		t.Error("two user mounts on /data were taken")
	}
}

func TestNerdctlHostGateway(t *testing.T) {
	// nerdctl.toml's host_gateway_ip is runwhp's gateway: --add-host
	// NAME:host-gateway reaches nerdctl's spec as that address.
	s := loadSpec(t, "plain.json", "")
	s.Annotations[extraHostsAnnotation] = `["reg:` + netstack.Gateway.String() + `"]`
	cfg, err := Config(Task{Spec: s, Rootfs: testRootfs})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HostGateway || len(cfg.Hosts) != 1 {
		t.Errorf("hosts %q gateway %v", cfg.Hosts, cfg.HostGateway)
	}
}
