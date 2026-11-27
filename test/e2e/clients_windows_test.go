//go:build windows

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Docker's own tools against runwhp's Engine API, each where it is
// installed: RUNWHP_E2E_CLIENTS=1 runs Docker's Go SDK and
// testcontainers-go from test/clients/go; RUNWHP_E2E_PYTHON names a python
// with test/clients/python's requirements; RUNWHP_E2E_COMPOSE_PLUGIN names
// Docker Compose's docker-compose.exe; RUNWHP_E2E_DEVCONTAINER names the
// Dev Containers CLI's devcontainer.js. Each needs RUNWHP_E2E_DOCKER too.

// clientsAPI is a test's API with the images the clients' tests use.
func clientsAPI(t *testing.T, images ...string) *dockerAPI {
	d := newDockerAPI(t)
	for _, ref := range append([]string{alpine}, images...) {
		d.image(ref)
	}
	return d
}

// run runs a client with the API's environment, and more, in dir.
func (d *dockerAPI) run(dir string, extraEnv []string, name string, args ...string) result {
	d.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(d.env(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// TestGoClients runs testcontainers-go and Docker's Go SDK, with Ryuk on:
// it reaches the API through Docker's socket in its container, bound to
// the API's pipe, as testcontainers-go binds the pipe DOCKER_HOST names.
func TestGoClients(t *testing.T) {
	parallel(t)
	if os.Getenv("RUNWHP_E2E_CLIENTS") != "1" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_CLIENTS=1 to run Docker's Go clients")
	}
	d := clientsAPI(t, "nginx:alpine", "redis:7-alpine", "testcontainers/ryuk:0.14.0")
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	dir, _ := filepath.Abs(filepath.Join("..", "clients", "go"))
	r := d.run(dir, []string{"RUNWHP_E2E_CLIENTS=1", "TESTCONTAINERS_RYUK_DISABLED=false", "GOFLAGS=-mod=mod"},
		goExe, "test", "-count=1", "-v", "-timeout", "15m", "./...")
	if r.code != 0 {
		t.Errorf("go test in test/clients/go: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	} else {
		t.Logf("%s", lastLines(r.stdout, 12))
	}
}

// TestDockerPy runs docker-py, found through DOCKER_HOST, then through
// the docker CLI's current context alone.
func TestDockerPy(t *testing.T) {
	parallel(t)
	python := os.Getenv("RUNWHP_E2E_PYTHON")
	if python == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_PYTHON to a python with test/clients/python's requirements")
	}
	d := clientsAPI(t, "docker:cli")
	script, _ := filepath.Abs(filepath.Join("..", "clients", "python", "test_dockerpy.py"))
	if r := d.run(t.TempDir(), nil, python, "-I", script); r.code != 0 {
		t.Errorf("docker-py through DOCKER_HOST: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// With a context of its own as the current one, and no DOCKER_HOST.
	config := t.TempDir()
	sum := sha256.Sum256([]byte("e2e"))
	meta := filepath.Join(config, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	ctxJSON := fmt.Sprintf(`{"Name":"e2e","Metadata":{},"Endpoints":{"docker":{"Host":%q,"SkipTLSVerify":false}}}`, d.host())
	os.WriteFile(filepath.Join(meta, "meta.json"), []byte(ctxJSON), 0o644)
	os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"currentContext":"e2e"}`), 0o644)
	cmd := exec.Command(python, "-I", script, "context")
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "DOCKER_HOST") && !strings.EqualFold(k, "DOCKER_CONFIG") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+config)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("docker-py through the current context: %v\n%s", err, out)
	}
}

// TestDockerCompose runs a project with Docker Compose through the API: a
// service built with the classic builder, which Compose falls back to
// without buildx, one waiting for another's health, random and fixed host
// ports, a named volume and two networks; then exec, top, logs, stop and
// start, run, and down with its volumes and images; and an attached up.
func TestDockerCompose(t *testing.T) {
	parallel(t)
	plugin := os.Getenv("RUNWHP_E2E_COMPOSE_PLUGIN")
	if plugin == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_COMPOSE_PLUGIN to Docker Compose's docker-compose.exe")
	}
	d := clientsAPI(t)
	installComposePlugin(t, d, plugin)
	fixed := freePort(t)
	dir := buildDir(t, map[string]string{
		"compose.yaml": `services:
  db:
    image: alpine:3.20
    command: ["sh", "-c", "echo db up; touch /data/ready; exec sleep 1000"]
    volumes: [dbdata:/data]
    networks: [back]
    healthcheck: {test: ["CMD", "test", "-f", "/data/ready"], interval: 1s, retries: 30}
  app:
    build: ./app
    depends_on: {db: {condition: service_healthy}}
    networks: [back, front]
    ports: ["80"]
  web:
    image: alpine:3.20
    command: ["sh", "-c", "echo fixed > /i && exec nc -lk -p 80 -e cat /i"]
    ports: ["127.0.0.1:` + fixed + `:80"]
    networks: [front]
  backup:
    image: alpine:3.20
    command: ["sh", "-c", "while [ ! -e /data/ready ]; do sleep 0.2; done; echo saw ready; exec sleep 1000"]
    volumes: [dbdata:/data:ro]
    deploy: {replicas: 2}
volumes:
  dbdata: {}
networks:
  back: {}
  front: {}
`,
		"app/Dockerfile": "FROM alpine:3.20\nRUN echo built > /index.html\nCMD [\"nc\", \"-lk\", \"-p\", \"80\", \"-e\", \"cat\", \"/index.html\"]\n",
	})
	compose := func(args ...string) result {
		t.Helper()
		return d.run(dir, nil, d.docker, append([]string{"compose", "-p", "rwc"}, args...)...)
	}
	ok := func(args ...string) string {
		t.Helper()
		r := compose(args...)
		if r.code != 0 {
			t.Fatalf("docker compose %s: exit %d\n%s\n%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
		}
		return r.stdout + r.stderr
	}
	t.Cleanup(func() { compose("down", "-v", "--rmi", "local", "-t", "0") })

	if out := ok("up", "-d", "--wait"); !strings.Contains(out, "classic builder") {
		t.Errorf("up gave no word of the classic builder:\n%s", out)
	}
	addr := strings.TrimSpace(ok("port", "app", "80"))
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Fatalf("port app 80: %q", addr)
	}
	if got := tcpBody(t, addr); got != "built\n" {
		t.Errorf("the built app at %s: %q", addr, got)
	}
	if got := tcpBody(t, "127.0.0.1:"+fixed); got != "fixed\n" {
		t.Errorf("web at %s: %q", fixed, got)
	}
	ps := ok("ps")
	for _, want := range []string{"rwc-app-1", "rwc-db-1", "rwc-web-1", "rwc-backup-1", "rwc-backup-2", "(healthy)", addr + "->80/tcp"} {
		if !strings.Contains(ps, want) {
			t.Errorf("ps lacks %q:\n%s", want, ps)
		}
	}
	// db and backup's replicas share dbdata: Compose made each container
	// before starting any, and the volume became shared as it did.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if out := ok("logs", "backup"); strings.Count(out, "saw ready") == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("backup's replicas never saw db's file on the volume they share:\n%s", out)
		}
	}
	if out := ok("exec", "-T", "app", "nc", "web", "80"); out != "fixed\n" {
		t.Errorf("exec app nc web 80: %q", out)
	}
	if out := ok("top", "app"); !strings.Contains(out, "nc -lk") {
		t.Errorf("top:\n%s", out)
	}
	if out := ok("logs", "db"); !strings.Contains(out, "db up") {
		t.Errorf("logs db: %q", out)
	}
	if r := d.cli("", "compose", "-p", "rwc", "ps", "--services"); r.code != 0 || strings.Fields(r.stdout) == nil ||
		!strings.Contains(r.stdout, "app") || !strings.Contains(r.stdout, "db") || !strings.Contains(r.stdout, "web") {
		t.Errorf("runwhp compose ps of Docker Compose's project: %+v", r)
	}
	ok("stop", "web")
	if out := ok("ps", "-a", "web"); !strings.Contains(out, "Exited") {
		t.Errorf("after stop:\n%s", out)
	}
	ok("start", "web")
	if got := tcpBody(t, "127.0.0.1:"+fixed); got != "fixed\n" {
		t.Errorf("web after start: %q", got)
	}
	if out := ok("run", "--rm", "--no-deps", "web", "echo", "once"); !strings.Contains(out, "once") {
		t.Errorf("run --rm: %q", out)
	}
	ok("down", "-v", "--rmi", "local", "-t", "0")
	if out := d.dok("ps", "-aq", "--filter", "label=com.docker.compose.project=rwc"); out != "" {
		t.Errorf("containers left after down: %q", out)
	}
	if out := d.dok("volume", "ls", "-q"); strings.Contains(out, "rwc_dbdata") {
		t.Errorf("volume left after down -v: %q", out)
	}

	// Attached, ended by a container's exit, whose code up gives.
	one := buildDir(t, map[string]string{"compose.yaml": `services:
  one:
    image: alpine:3.20
    command: ["sh", "-c", "echo attached; sleep 1; exit 3"]
  two:
    image: alpine:3.20
    command: ["sleep", "1000"]
`})
	r := d.run(one, nil, d.docker, "compose", "-p", "rwc2", "up", "--abort-on-container-exit", "--exit-code-from", "one")
	if r.code != 3 || !strings.Contains(r.stdout, "attached") {
		t.Errorf("attached up: %+v", r)
	}
	d.run(one, nil, d.docker, "compose", "-p", "rwc2", "down", "-t", "0")
}

// installComposePlugin puts docker-compose.exe where the test's docker CLI
// finds its plugins.
func installComposePlugin(t *testing.T, d *dockerAPI, plugin string) {
	t.Helper()
	dir := filepath.Join(d.config, "cli-plugins")
	os.MkdirAll(dir, 0o755)
	data, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.exe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDevcontainerCLI brings dev containers up with the Dev Containers
// CLI, the code VS Code's extension runs: from an image, from a Dockerfile
// with a feature, which it builds with the classic builder, and, with
// Docker Compose there, from a compose file; each takes a command, and
// writes into the workspace the host sees.
func TestDevcontainerCLI(t *testing.T) {
	parallel(t)
	js := os.Getenv("RUNWHP_E2E_DEVCONTAINER")
	if js == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_DEVCONTAINER to the Dev Containers CLI's devcontainer.js")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	d := clientsAPI(t)
	cases := map[string]map[string]string{
		"image":   {".devcontainer/devcontainer.json": `{"image": "alpine:3.20"}`},
		"feature": devcontainerFeature,
	}
	if plugin := os.Getenv("RUNWHP_E2E_COMPOSE_PLUGIN"); plugin != "" {
		installComposePlugin(t, d, plugin)
		cases["compose"] = map[string]string{
			".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yaml", "service": "dev", "workspaceFolder": "/workspace"}`,
			".devcontainer/compose.yaml": `services:
  dev:
    image: alpine:3.20
    command: ["sleep", "infinity"]
    volumes: ["..:/workspace"]
`,
		}
	}
	// docker-outside-of-docker binds Docker's socket into the container,
	// which reaches the API: the dev container's docker, as its own user
	// through the feature's proxy, lists it and runs another. Its image
	// comes from the e2e mirror, rather than from MCR into each store.
	cases["docker-outside-of-docker"] = map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + mirrored(t, "mcr.microsoft.com/devcontainers/base:bookworm") + `",
  "features": {"ghcr.io/devcontainers/features/docker-outside-of-docker:1": {}}}`,
	}
	// The cases share the API, each with a workspace of its own, as dev
	// containers of several folders do, but for docker-outside-of-docker:
	// the CLI builds every container's features into one fixed tag,
	// dev_container_feature_content_temp, which two cases with features
	// building at once on one daemon take from each other.
	var subtests []subtest
	for name, files := range cases {
		subtests = append(subtests, subtest{name, func(t *testing.T) {
			api := d
			if name == "docker-outside-of-docker" {
				api = clientsAPI(t)
			}
			devcontainerCase(t, api, node, js, name, files)
		}})
	}
	concurrently(t, subtests...)
}

// devcontainerFeature is a dev container from a Dockerfile with a feature,
// which Dev Containers builds on the Dockerfile's image.
var devcontainerFeature = map[string]string{
	".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile"},
  "features": {"ghcr.io/devcontainers/features/common-utils:2": {"installZsh": false, "installOhMyZsh": false, "upgradePackages": false, "username": "none"}}}`,
	".devcontainer/Dockerfile": "FROM alpine:3.20\nRUN echo from-dockerfile > /etc/marker\n",
}

// TestDevcontainerBuildx brings a dev container up from a Dockerfile with
// a feature with buildx installed, as Docker Desktop installs it: the Dev
// Containers CLI then builds through BuildKit, the feature's stage FROM
// the image it built first, which only runwhp's store has.
func TestDevcontainerBuildx(t *testing.T) {
	parallel(t)
	js := os.Getenv("RUNWHP_E2E_DEVCONTAINER")
	if js == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_DEVCONTAINER to the Dev Containers CLI's devcontainer.js")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	d := buildxAPI(t)
	devcontainerCase(t, d, node, js, "feature", devcontainerFeature)
}

// devcontainerCase brings a dev container up from files, execs in it, and
// checks what it sees.
func devcontainerCase(t *testing.T, d *dockerAPI, node, js, name string, files map[string]string) {
	t.Helper()
	ws := buildDir(t, files)
	t.Cleanup(func() {
		for _, id := range strings.Fields(d.docker1("ps", "-aq", "--filter", "label=devcontainer.local_folder="+ws).stdout) {
			d.docker1("rm", "-f", id)
		}
		// The project is named for the workspace's folder, 001 as every
		// case's is, so only the compose case brings it down, while the
		// others may still run.
		if name == "compose" {
			d.run(ws, nil, d.docker, "compose", "-p", strings.ToLower(filepath.Base(ws))+"_devcontainer", "down", "-t", "0")
		}
	})
	up := d.run(ws, nil, node, js, "up", "--workspace-folder", ws, "--docker-path", d.docker, "--log-format", "json")
	outcome := lastJSONLine(up.stdout)
	if up.code != 0 || outcome["outcome"] != "success" {
		t.Fatalf("devcontainer up: exit %d, %v\n%s\n%s", up.code, outcome, lastLines(up.stdout, 30), up.stderr)
	}
	folder, _ := outcome["remoteWorkspaceFolder"].(string)
	write := "echo from-the-container > " + folder + "/written"
	if name == "docker-outside-of-docker" {
		// The workspace's files are the container's user's, root's,
		// and not the remote user's, vscode's: see gaps.md.
		write = "sudo sh -c '" + write + "'"
	}
	ex := d.run(ws, nil, node, js, "exec", "--workspace-folder", ws, "--docker-path", d.docker,
		"sh", "-c", "uname -s; cat /etc/marker 2>/dev/null; "+write)
	if ex.code != 0 || !strings.Contains(ex.stdout, "Linux") {
		t.Errorf("devcontainer exec: %+v", ex)
	}
	if name == "feature" && !strings.Contains(ex.stdout, "from-dockerfile") {
		t.Errorf("the Dockerfile's image was not used: %q", ex.stdout)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "written")); err != nil || string(b) != "from-the-container\n" {
		t.Errorf("the workspace as the host sees it: %q, %v", b, err)
	}
	if name == "docker-outside-of-docker" {
		ex := d.run(ws, nil, node, js, "exec", "--workspace-folder", ws, "--docker-path", d.docker,
			"sh", "-c", "id -un; docker ps --format '{{.Label \"devcontainer.local_folder\"}}'; docker run --rm "+alpine+" echo nested")
		// The label has the folder as Dev Containers spells it, its
		// drive letter in lower case.
		if ex.code != 0 || !strings.Contains(ex.stdout, "vscode\n") || !strings.Contains(strings.ToLower(ex.stdout), strings.ToLower(ws)) ||
			!strings.Contains(ex.stdout, "nested\n") {
			t.Errorf("docker in the dev container: %+v", ex)
		}
	}
}

func lastJSONLine(s string) map[string]any {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m["outcome"] != nil {
			return m
		}
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// tcpBody reads what a server at addr sends, trying for 20 s while
// nothing answers.
func tcpBody(t *testing.T, addr string) string {
	t.Helper()
	var last error
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			last = err
			continue
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		c.Close()
		if len(b) > 0 {
			return string(b)
		}
	}
	t.Errorf("nothing served at %s: %v", addr, last)
	return ""
}
