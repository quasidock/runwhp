package compose

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// project writes files into a directory of their own and loads the
// project there.
func project(t *testing.T, files map[string]string, o LoadOptions) (*types.Project, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "My.App_X")
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o.ProjectDir = dir
	for i, f := range o.Files {
		o.Files[i] = filepath.Join(dir, f)
	}
	p, err := Load(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir
}

const full = `
services:
  web:
    extends:
      file: base.yaml
      service: base
    build:
      context: ./app
      args: {V: "1"}
    command: ["sh", "-c", "echo $${GREETING}"]
    entrypoint: /bin/sh -c
    environment:
      GREETING: ${GREETING:-hello}
      UNSET:
    env_file: [web.env]
    ports: ["8080:80", "127.0.0.2:8443:443/tcp", "9000"]
    expose: ["9100", "9200/udp"]
    extra_hosts: ["h:host-gateway", "x=1.2.3.4"]
    networks:
      front:
        aliases: [www]
      back:
        ipv4_address: 172.30.0.9
    volumes:
      - ./data:/data:ro
      - type: bind
        source: ./cache
        target: /cache
        consistency: delegated
        bind: {create_host_path: false}
      - named:/named:nocopy
      - /anon
      - type: tmpfs
        target: /tmp/t
        tmpfs: {size: 1048576, mode: 0o1777}
    tmpfs: ["/run:size=64m"]
    secrets: [pw, {source: pw, target: other}]
    configs: [conf]
    restart: unless-stopped
    mem_limit: 512m
    cpus: 1.5
    healthcheck:
      test: ["CMD", "wget", "-q", "localhost"]
      interval: 2s
      retries: 4
      start_period: 10s
      start_interval: 1s
    depends_on:
      db: {condition: service_healthy}
    hostname: web-host
    user: "1000:1000"
    working_dir: /app
    tty: true
    stdin_open: true
    stop_signal: SIGUSR1
    stop_grace_period: 1500ms
    labels: {tier: front}
    logging:
      driver: json-file
      options: {max-size: 10m, max-file: "3", compress: "true"}
    pull_policy: always
    platform: linux/amd64
    storage_opt: {size: 8g}
  client:
    image: alpine:3.20
    links: ["web:frontend"]
    networks: [back]
  db:
    image: postgres:17
    networks: [back]
    deploy:
      replicas: 1
      resources:
        limits: {memory: 1g, cpus: "2"}
      restart_policy: {condition: on-failure, max_attempts: 3}
  worker:
    image: alpine:3.20
    network_mode: none
    scale: 3
    attach: false
    healthcheck: {disable: true}
volumes:
  named: {}
networks:
  front: {}
  back: {}
secrets:
  pw: {file: ./pw.txt}
configs:
  conf: {file: ./conf.ini}
`

func TestTranslate(t *testing.T) {
	p, dir := project(t, map[string]string{
		"compose.yaml": full,
		"base.yaml":    "services:\n  base:\n    image: mine/web:1\n    labels: {from: base}\n",
		"web.env":      "FROM_FILE=1\n",
		"pw.txt":       "secret",
		"conf.ini":     "x=1",
	}, LoadOptions{})
	if p.Name != "myapp_x" {
		t.Errorf("project name %q", p.Name)
	}
	var pr Problems
	Check(p, ServicesOf(p, nil), &pr)
	web := Translate(p, p.Services["web"], &pr)
	if err := pr.Err(); err != nil {
		t.Fatal(err)
	}
	want := Container{
		Service: "web", Image: "mine/web:1",
		Entrypoint: []string{"/bin/sh", "-c"}, EntrypointSet: true, Command: []string{"sh", "-c", "echo ${GREETING}"},
		Env:        []string{"FROM_FILE=1", "GREETING=hello"},
		WorkingDir: "/app", User: "1000:1000", Hostname: "web-host", TTY: true, StdinOpen: true,
		Memory: 512 << 20, CPUs: 2,
		Networks: []Attachment{{Name: "myapp_x_back", Aliases: []string{"web", "frontend"}, IPv4: "172.30.0.9"},
			{Name: "myapp_x_front", Aliases: []string{"web", "www", "frontend"}}},
		Ports:      []Port{{HostPort: 8080, Target: 80}, {HostIP: "127.0.0.2", HostPort: 8443, Target: 443}, {Target: 9000}},
		Expose:     []string{"9100", "9200/udp"},
		ExtraHosts: []string{"h:host-gateway", "x:1.2.3.4"},
		Mounts: []Mount{
			{Kind: MountBind, Source: filepath.Join(dir, "data"), Target: "/data", ReadOnly: true, Create: true},
			{Kind: MountBind, Source: filepath.Join(dir, "cache"), Target: "/cache", Delegated: true},
			{Kind: MountVolume, Source: "myapp_x_named", Target: "/named", NoCopy: true},
			{Kind: MountVolume, Target: "/anon"},
			{Kind: MountTmpfs, Target: "/tmp/t", Options: "size=1048576,mode=1777"},
			{Kind: MountTmpfs, Target: "/run", Options: "size=64m"},
			{Kind: MountBind, Source: filepath.Join(dir, "pw.txt"), Target: "/run/secrets/pw", ReadOnly: true},
			{Kind: MountBind, Source: filepath.Join(dir, "pw.txt"), Target: "/run/secrets/other", ReadOnly: true},
			{Kind: MountBind, Source: filepath.Join(dir, "conf.ini"), Target: "/conf", ReadOnly: true},
		},
		Restart:     "unless-stopped",
		Health:      &Health{Test: []string{"CMD", "wget", "-q", "localhost"}, Interval: 2 * time.Second, StartPeriod: 10 * time.Second, StartInterval: time.Second, Retries: 4},
		StopSignal:  "SIGUSR1",
		StopTimeout: ptr(2),
		StorageSize: "8g",
		Labels:      map[string]string{"tier": "front", "from": "base"},
		LogDriver:   "local", LogOpts: []string{"max-file=3", "max-size=10m"},
		PullPolicy: "always", Replicas: 1, Attach: true,
	}
	if !reflect.DeepEqual(web, want) {
		t.Errorf("web:\n got %+v\nwant %+v", web, want)
	}
	if !slices.ContainsFunc(pr.Warnings, func(w string) bool { return strings.Contains(w, "cpus 1.5 is 2 here") }) ||
		!slices.ContainsFunc(pr.Warnings, func(w string) bool { return strings.Contains(w, "log option compress is ignored") }) {
		t.Errorf("warnings: %q", pr.Warnings)
	}

	db := Translate(p, p.Services["db"], &pr)
	if db.Memory != 1<<30 || db.CPUs != 2 || db.Restart != "on-failure:3" || db.Image != "postgres:17" ||
		len(db.Networks) != 1 || !reflect.DeepEqual(db.Networks[0].Aliases, []string{"db"}) {
		t.Errorf("db: %+v", db)
	}
	worker := Translate(p, p.Services["worker"], &pr)
	if !worker.NoNetwork || worker.Replicas != 3 || worker.Attach || !worker.Health.Disable || worker.Restart != "no" {
		t.Errorf("worker: %+v", worker)
	}
	if name := ImageName(p.Name, types.ServiceConfig{Name: "api"}); name != "myapp_x-api" {
		t.Errorf("a built image's name: %s", name)
	}
	if got := DependsOnLabel(p.Services["web"]); got != "db:service_healthy:false" {
		t.Errorf("depends_on label %q", got)
	}
	if ContainerName("p", p.Services["web"], 2) != "p-web-2" || OneoffName("p", "web", "0123456789abcdef") != "p-web-run-0123456789ab" {
		t.Error("names")
	}
}

func ptr[T any](v T) *T { return &v }

func TestRefused(t *testing.T) {
	p, _ := project(t, map[string]string{"compose.yaml": `
services:
  host: {image: a, network_mode: host}
  dev: {image: a, devices: ["/dev/sda:/dev/sda"]}
  gpu: {image: a, gpus: all}
  vf: {image: a, volumes_from: [dev]}
  pid: {image: a, pid: host}
  arm: {image: a, platform: linux/arm64}
  udp: {image: a, ports: ["53:53/udp"]}
  range: {image: a, ports: ["8000-8001:80-81"]}
  ok: {image: a, links: [range]}
  sub: {image: a, volumes: [{type: volume, source: v, target: /v, volume: {subpath: x}}]}
  envsecret: {image: a, secrets: [es]}
  ipv6: {image: a, networks: {n: {ipv6_address: "fd00::9"}}}
volumes:
  v: {}
  dv: {driver: nfs}
networks:
  n: {}
  ov: {driver: overlay}
  ip: {ipam: {config: [{subnet: 10.9.0.0/16}]}}
  in: {internal: true}
secrets:
  es: {environment: HOME}
`}, LoadOptions{})
	var pr Problems
	Check(p, ServicesOf(p, nil), &pr)
	for _, name := range ServicesOf(p, nil) {
		Translate(p, p.Services[name], &pr)
	}
	got := pr.Err().Error()
	for _, want := range []string{
		`service "host": network_mode host is not supported`,
		`service "dev": devices is not supported`,
		`service "gpu": gpus is not supported`,
		`service "vf": volumes_from is not supported`,
		`service "pid": pid host is not supported`,
		`service "arm": platform linux/arm64 is not supported: runwhp runs linux/amd64 only`,
		`service "udp": port 53/udp cannot be published: runwhp publishes TCP ports only`,
		`service "sub": /v: volume subpath is not supported`,
		`service "envsecret": secret es: only secrets from a file are supported`,
		`service "ipv6": network n: IPv6 and link-local addresses are not supported`,
		`volume dv: driver nfs is not supported`,
		`network ov: driver overlay is not supported`,
		`network ip: ipam is not supported`,
		`network in: internal networks are not supported`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no error saying %q in:\n%s", want, got)
		}
	}
	// compose-go makes a range of ports single ones, which runwhp takes.
	r := Translate(p, p.Services["range"], &Problems{})
	if !reflect.DeepEqual(r.Ports, []Port{{HostPort: 8000, Target: 80}, {HostPort: 8001, Target: 81}}) {
		t.Errorf("a range: %+v", r.Ports)
	}
}

func TestIgnored(t *testing.T) {
	p, _ := project(t, map[string]string{"compose.yaml": `
services:
  a:
    image: a
    privileged: true
    cap_add: [NET_ADMIN]
    init: true
    dns: [1.1.1.1]
    read_only: true
    expose: ["80"]
    logging: {driver: syslog}
    deploy: {resources: {reservations: {memory: 1g}}}
`}, LoadOptions{})
	var pr Problems
	Translate(p, p.Services["a"], &pr)
	if err := pr.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(pr.Warnings, "\n")
	for _, want := range []string{"privileged is ignored", "cap_add is ignored", "init is ignored", "dns is ignored", "read_only is ignored",
		"logging driver syslog is runwhp's local log here", "deploy.resources.reservations is ignored"} {
		if !strings.Contains(all, want) {
			t.Errorf("no warning saying %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "expose") {
		t.Errorf("expose warned of: %s", all)
	}
}

func TestSharedVolumes(t *testing.T) {
	p, _ := project(t, map[string]string{"compose.yaml": `
services:
  a: {image: a, volumes: ["data:/d"]}
  b: {image: a, volumes: ["data:/d:ro"]}
  mig: {image: a, volumes: ["seed:/s"]}
  app: {image: a, volumes: ["seed:/s"], depends_on: {mig: {condition: service_completed_successfully}}}
  many: {image: a, volumes: ["solo:/s"], deploy: {replicas: 2}}
  bind: {image: a, volumes: ["./x:/x"]}
  bind2: {image: a, volumes: ["./x:/x"]}
volumes: {data: {}, seed: {}, solo: {}}
`}, LoadOptions{})
	var pr Problems
	Check(p, ServicesOf(p, nil), &pr)
	got := pr.Err().Error()
	for _, want := range []string{
		`services "a" and "b" both mount volume myapp_x_data: a runwhp volume is used by one running container at a time`,
		`service "many" mounts volume myapp_x_solo and has 2 replicas`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no error saying %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "seed") || len(pr.Errors) != 2 {
		t.Errorf("errors: %v", pr.Errors)
	}
	// Running a alone is fine.
	pr = Problems{}
	Check(p, []string{"a", "mig", "app"}, &pr)
	if pr.Err() != nil {
		t.Errorf("a alone: %v", pr.Err())
	}
}

func TestLoad(t *testing.T) {
	files := map[string]string{
		"compose.yaml":  "services:\n  web: {image: alpine, environment: {V: \"${V}\"}}\n  dbg: {image: busybox, profiles: [debug]}\n",
		".env":          "V=from-dotenv\n",
		"other.env":     "V=from-envfile\n",
		"override.yaml": "services:\n  web: {labels: {extra: \"1\"}}\n",
	}
	p, _ := project(t, files, LoadOptions{})
	if v := *p.Services["web"].Environment["V"]; v != "from-dotenv" || len(p.Services) != 1 {
		t.Errorf("defaults: V=%s, services %v", v, p.ServiceNames())
	}
	p, _ = project(t, files, LoadOptions{EnvFiles: nil, Profiles: []string{"debug"}, Name: "named",
		Files: []string{"compose.yaml", "override.yaml"}})
	if p.Name != "named" || len(p.Services) != 2 || p.Services["web"].Labels["extra"] != "1" || len(p.ComposeFiles) != 2 {
		t.Errorf("profiles, name and files: %s %v %v %v", p.Name, p.ServiceNames(), p.Services["web"].Labels, p.ComposeFiles)
	}
	dir := filepath.Join(t.TempDir(), "envfile")
	os.MkdirAll(dir, 0o755)
	for n, c := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
	}
	p, err := Load(context.Background(), LoadOptions{ProjectDir: dir, EnvFiles: []string{filepath.Join(dir, "other.env")}})
	if err != nil || *p.Services["web"].Environment["V"] != "from-envfile" {
		t.Errorf("--env-file: %v", err)
	}
	t.Setenv("COMPOSE_PROFILES", "debug")
	if p, err := Load(context.Background(), LoadOptions{ProjectDir: dir}); err != nil || len(p.Services) != 2 {
		t.Errorf("COMPOSE_PROFILES: %v, %v", err, p.ServiceNames())
	}
	if _, err := Load(context.Background(), LoadOptions{ProjectDir: dir, Name: "Bad Name"}); err == nil ||
		!strings.Contains(err.Error(), `invalid project name "Bad Name"`) {
		t.Errorf("a bad name: %v", err)
	}
	empty := t.TempDir()
	if _, err := Load(context.Background(), LoadOptions{ProjectDir: empty}); err == nil || !strings.Contains(err.Error(), "no configuration file provided: not found") {
		t.Errorf("no file: %v", err)
	}
}

func service(env types.MappingWithEquals) types.ServiceConfig {
	s := types.ServiceConfig{Name: "a"}
	s.Image, s.Environment = "alpine", env
	return s
}

func TestConfigHash(t *testing.T) {
	s := service(types.MappingWithEquals{"A": ptr("1"), "B": ptr("2")})
	h := ConfigHash(s)
	if h != ConfigHash(service(types.MappingWithEquals{"B": ptr("2"), "A": ptr("1")})) {
		t.Error("the hash depends on key order")
	}
	same := s
	same.Scale, same.PullPolicy, same.Profiles = ptr(3), "always", []string{"x"}
	same.Build = &types.BuildConfig{Context: "."}
	same.DependsOn = types.DependsOnConfig{"db": {Condition: "service_started"}}
	same.Deploy = &types.DeployConfig{Replicas: ptr(2)}
	plain := service(s.Environment)
	plain.Deploy = &types.DeployConfig{}
	if ConfigHash(same) != ConfigHash(plain) {
		t.Error("scale, pull policy, profiles, build or dependencies change the hash")
	}
	if same.Deploy.Replicas == nil {
		t.Error("ConfigHash changed its argument's deploy")
	}
	changed := s
	changed.Environment = types.MappingWithEquals{"A": ptr("2")}
	if ConfigHash(changed) == h {
		t.Error("the environment does not change the hash")
	}
}

func TestPrefixer(t *testing.T) {
	var out, errs bytes.Buffer
	p := NewPrefixer(Width([]string{"a-1", "bb-1", "longername-1"}), false, true)
	a, bb := p.Writer("a-1", &out), p.Writer("bb-1", &errs)
	a.Write([]byte("a partial"))
	bb.Write([]byte("bb hello\r\nbb two\n"))
	a.Write([]byte(" done\nlast"))
	a.Close()
	p.Exited(&out, "bb-1", 3)
	if got := out.String(); got != "a-1           | a partial done\na-1           | last\n\r\x1b[Kbb-1 exited with code 3\n" {
		t.Errorf("stdout %q", got)
	}
	if got := errs.String(); got != "bb-1          | bb hello\r\nbb-1          | bb two\n" {
		t.Errorf("stderr %q", got)
	}

	out.Reset()
	c := NewPrefixer(4, true, true)
	w1, w2 := c.Writer("db-1", &out), c.Writer("ab-1", &out)
	w1.Write([]byte("x\n"))
	w2.Write([]byte("y\n"))
	c.Exited(&out, "ab-1", 0)
	if got := out.String(); got != "\x1b[36mdb-1  | \x1b[0mx\n\x1b[33mab-1  | \x1b[0my\n\x1b[33m\r\x1b[Kab-1 exited with code 0\n\x1b[0m" {
		t.Errorf("coloured %q", got)
	}

	out.Reset()
	n := NewPrefixer(4, true, false)
	n.Writer("db-1", &out).Write([]byte("bare\n"))
	if out.String() != "bare\n" {
		t.Errorf("no prefix: %q", out.String())
	}
}
