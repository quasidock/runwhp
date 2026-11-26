//go:build windows

package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/google/go-containerregistry/pkg/registry"
)

// The docker CLI's tests need Docker's static Windows CLI, docker.exe,
// named by RUNWHP_E2E_DOCKER; they are skipped without it.

// dockerAPI is runwhp's Engine API serving a store of the test's own, and
// the docker CLI pointed at it.
type dockerAPI struct {
	*ownStore
	t      *testing.T
	docker string
	pipe   string
	config string // DOCKER_CONFIG, of the test's own
}

var pipeSeq atomic.Int32

func newDockerAPI(t *testing.T) *dockerAPI {
	t.Helper()
	docker := os.Getenv("RUNWHP_E2E_DOCKER")
	if docker == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_DOCKER to docker.exe to run the docker CLI's tests")
	}
	s := newOwnStore(t)
	d := &dockerAPI{ownStore: s, t: t, docker: docker, config: t.TempDir(),
		pipe: fmt.Sprintf(`\\.\pipe\runwhp_engine-e2e-%d-%d`, os.Getpid(), pipeSeq.Add(1))}
	r := s.ok("docker-api", "start", "--pipe", d.pipe)
	if !strings.Contains(r.stdout, d.pipe) {
		t.Fatalf("docker-api start:\n%s", r.stdout)
	}
	t.Cleanup(func() {
		// Containers first: their runwhps hold the store's files.
		for _, id := range strings.Fields(d.docker1("ps", "-aq").stdout) {
			d.docker1("rm", "-f", id)
		}
		s.cli("", "docker-api", "stop", "--pipe", d.pipe)
	})
	return d
}

func (d *dockerAPI) cmd(stdin io.Reader, args ...string) *exec.Cmd {
	cmd := exec.Command(d.docker, args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=npipe://"+strings.ReplaceAll(d.pipe, `\`, "/"), "DOCKER_CONFIG="+d.config,
		"RUNWHP_ROOT="+d.root, "DOCKER_CLI_HINTS=false")
	cmd.Stdin = stdin
	return cmd
}

// docker1 runs the docker CLI with args, and gives what it printed.
func (d *dockerAPI) docker1(args ...string) result { return d.dockerIn(nil, args...) }

func (d *dockerAPI) dockerIn(stdin io.Reader, args ...string) result {
	d.t.Helper()
	cmd := d.cmd(stdin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// dok runs the docker CLI and fails the test unless it exits 0.
func (d *dockerAPI) dok(args ...string) string {
	d.t.Helper()
	r := d.docker1(args...)
	if r.code != 0 {
		d.t.Fatalf("docker %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// image pulls ref from the e2e mirror through the API, and names it ref
// too, so that tests use the names they know.
func (d *dockerAPI) image(ref string) string {
	d.t.Helper()
	there := mirrored(d.t, ref)
	d.dok("pull", there)
	d.dok("tag", there, ref)
	return ref
}

// get asks the API at path and decodes its JSON answer.
func (d *dockerAPI) get(path string, v any) {
	d.t.Helper()
	resp, err := d.client().Get("http://runwhp/v1.56" + path)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		d.t.Fatalf("GET %s: %s\n%s", path, resp.Status, data)
	}
	if err := json.Unmarshal(data, v); err != nil {
		d.t.Fatalf("GET %s: %v\n%s", path, err, data)
	}
}

func (d *dockerAPI) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, d.pipe)
	}}}
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

// TestDockerRun runs containers with the docker CLI as runwhp's run would.
func TestDockerRun(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	if v := d.dok("version", "--format", "{{.Server.APIVersion}} {{.Server.Os}}"); v != "1.56 linux\n" {
		t.Errorf("version: %q", v)
	}
	if info := d.dok("info", "--format", "{{.OSType}} {{.Driver}} {{.DefaultRuntime}}"); info != "linux runwhp runwhp\n" {
		t.Errorf("info: %q", info)
	}
	if out := d.dok("run", "--rm", img, "echo", "hello"); out != "hello\n" {
		t.Errorf("run: %q", out)
	}
	r := d.docker1("run", "--rm", img, "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.stdout != "out\n" || r.stderr != "err\n" || r.code != 3 {
		t.Errorf("streams and exit code: %+v", r)
	}
	r = d.dockerIn(strings.NewReader("line one\nline two\n"), "run", "--rm", "-i", img, "sh", "-c", "wc -l")
	if strings.TrimSpace(r.stdout) != "2" || r.code != 0 {
		t.Errorf("run -i: %+v", r)
	}
	if out := d.dok("run", "--rm", "-t", img, "sh", "-c", "test -t 1 && echo tty"); strings.TrimSpace(out) != "tty" {
		t.Errorf("run -t: %q", out)
	}
	out := d.dok("run", "--rm", "-e", "A=1", "-w", "/tmp", "-u", "nobody", "--hostname", "box", img, "sh", "-c", "echo $A $(pwd) $(id -u) $(hostname)")
	if out != "1 /tmp 65534 box\n" {
		t.Errorf("env, workdir, user, hostname: %q", out)
	}
	before := d.dok("ps", "-aq")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--privileged"}, "--privileged"},
		{[]string{"--network", "host"}, "--network host"},
		{[]string{"--dns", "1.1.1.1"}, "--dns"},
		{[]string{"-p", "53:53/udp"}, "only tcp"},
		{[]string{"--cap-add", "NET_ADMIN"}, "--cap-add"},
	} {
		r := d.docker1(append(append([]string{"run", "--rm"}, tc.args...), img, "true")...)
		if r.code == 0 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("run %v: exit %d, stderr %q; want a refusal naming %s", tc.args, r.code, r.stderr, tc.want)
		}
	}
	if after := d.dok("ps", "-aq"); after != before {
		t.Errorf("refusals left containers: %q", after)
	}
}

// TestDockerLifecycle stops, starts, pauses, renames, waits for and
// removes containers, and follows them in ps, logs, inspect and events.
func TestDockerLifecycle(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	since := time.Now().Add(-time.Second).Unix()
	d.dok("run", "-d", "--name", "ticker", "--label", "app=demo", "--health-cmd", "true", "--health-interval", "1s",
		img, "sh", "-c", "i=0; trap 'echo got TERM; exit 7' TERM; while true; do echo tick $i; i=$((i+1)); sleep 0.2; done")
	if out := d.dok("ps", "--filter", "label=app=demo", "--format", "{{.Names}}"); out != "ticker\n" {
		t.Errorf("ps --filter label: %q", out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(d.dok("ps", "--format", "{{.Status}}"), "(healthy)") {
		if time.Now().After(deadline) {
			t.Fatalf("never healthy: %s", d.dok("ps"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if out := d.dok("logs", "--tail", "2", "ticker"); len(lines(out)) != 2 || !strings.HasPrefix(out, "tick ") {
		t.Errorf("logs --tail 2: %q", out)
	}
	if out := d.dok("logs", "-t", "--tail", "1", "ticker"); !strings.Contains(out, "Z tick ") {
		t.Errorf("logs -t: %q", out)
	}
	d.dok("pause", "ticker")
	if st := d.dok("inspect", "-f", "{{.State.Status}} {{.State.Paused}}", "ticker"); st != "paused true\n" {
		t.Errorf("paused: %q", st)
	}
	d.dok("unpause", "ticker")
	d.dok("rename", "ticker", "tocker")
	d.dok("stop", "-t", "5", "tocker")
	if out := d.dok("logs", "--tail", "1", "tocker"); out != "got TERM\n" {
		t.Errorf("stop sent no TERM: %q", out)
	}
	if code := d.dok("wait", "tocker"); code != "7\n" {
		t.Errorf("wait: %q", code)
	}
	if st := d.dok("inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{.Name}} {{.Config.Labels.app}}", "tocker"); st != "exited 7 /tocker demo\n" {
		t.Errorf("inspect: %q", st)
	}
	d.dok("start", "tocker")
	d.dok("kill", "-s", "TERM", "tocker")
	if code := d.dok("wait", "tocker"); code != "7\n" {
		t.Errorf("kill -s TERM, wait: %q", code)
	}
	d.dok("restart", "-t", "1", "tocker")
	if st := d.dok("inspect", "-f", "{{.State.Running}}", "tocker"); st != "true\n" {
		t.Errorf("restart: %q", st)
	}
	d.dok("rm", "-f", "tocker")

	d.dok("create", "--name", "later", img, "sh", "-c", "echo started; exit 2")
	r := d.docker1("start", "-a", "later")
	if r.stdout != "started\n" || r.code != 2 {
		t.Errorf("start -a: %+v", r)
	}
	r = d.docker1("rm", "later")
	if r.code != 0 {
		t.Errorf("rm: %+v", r)
	}
	r = d.docker1("rm", "nosuch")
	if r.code == 0 || !strings.Contains(r.stderr, "No such container: nosuch") {
		t.Errorf("rm nosuch: %+v", r)
	}

	evs := d.dok("events", "--since", fmt.Sprint(since), "--until", fmt.Sprint(time.Now().Unix()+1),
		"--filter", "container=later", "--format", "{{.Action}}")
	if got := strings.Join(lines(evs), ","); got != "create,attach,start,die,destroy" {
		t.Errorf("events: %s", got)
	}
}

// TestDockerExec runs commands in a running container.
func TestDockerExec(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	d.dok("run", "-d", "--name", "ex", img, "sleep", "600")
	r := d.docker1("exec", "ex", "sh", "-c", "echo in; exit 4")
	if r.stdout != "in\n" || r.code != 4 {
		t.Errorf("exec: %+v", r)
	}
	if out := d.dok("exec", "-e", "B=2", "-w", "/etc", "-u", "nobody", "ex", "sh", "-c", "echo $B $(pwd) $(id -u)"); out != "2 /etc 65534\n" {
		t.Errorf("exec -e -w -u: %q", out)
	}
	r = d.dockerIn(strings.NewReader("from stdin\n"), "exec", "-i", "ex", "cat")
	if r.stdout != "from stdin\n" || r.code != 0 {
		t.Errorf("exec -i: %+v", r)
	}
	d.dok("exec", "-d", "ex", "sh", "-c", "sleep 0.3; echo done > /tmp/d")
	time.Sleep(time.Second)
	if out := d.dok("exec", "ex", "cat", "/tmp/d"); out != "done\n" {
		t.Errorf("exec -d: %q", out)
	}
	// docker top, as ps -ef and ps aux show the processes.
	if out := d.dok("top", "ex"); !strings.HasPrefix(out, "UID") || !strings.Contains(out, "sleep 600") {
		t.Errorf("top: %q", out)
	}
	if out := d.dok("top", "ex", "aux"); !strings.HasPrefix(out, "USER") || !strings.Contains(out, "sleep 600") {
		t.Errorf("top aux: %q", out)
	}
	if r := d.docker1("top", "ex", "-o", "pid"); r.code == 0 || !strings.Contains(r.stderr, "not supported") {
		t.Errorf("top -o pid: %+v", r)
	}
	d.dok("stop", "-t", "0", "ex")
	if r := d.docker1("exec", "ex", "true"); r.code == 0 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("exec in a stopped container: %+v", r)
	}
	if r := d.docker1("top", "ex"); r.code == 0 || !strings.Contains(r.stderr, "is not running") {
		t.Errorf("top of a stopped container: %+v", r)
	}
}

// TestDockerFiles copies files in and out of running and stopped
// containers, and diffs, exports and commits them.
func TestDockerFiles(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("copied in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.dok("run", "-d", "--name", "fs", img, "sleep", "600")
	for _, state := range []string{"running", "stopped"} {
		d.dok("cp", filepath.Join(dir, "in.txt"), "fs:/root/"+state+".txt")
		out := filepath.Join(dir, state+"-out.txt")
		d.dok("cp", "fs:/root/"+state+".txt", out)
		if data, _ := os.ReadFile(out); string(data) != "copied in\n" {
			t.Errorf("%s: cp round trip gave %q", state, data)
		}
		if diff := d.dok("diff", "fs"); !strings.Contains(diff, "A /root/"+state+".txt") {
			t.Errorf("%s: diff:\n%s", state, diff)
		}
		if state == "running" {
			d.dok("stop", "-t", "0", "fs")
		}
	}
	if r := d.docker1("cp", "fs:/nosuch", dir); r.code == 0 || !strings.Contains(r.stderr, "Could not find the file /nosuch") {
		t.Errorf("cp of a missing file: %+v", r)
	}
	cmd := d.cmd(nil, "export", "fs")
	tarOut, err := cmd.Output()
	if err != nil || !bytes.Contains(tarOut, []byte("root/running.txt")) || !bytes.Contains(tarOut, []byte("etc/alpine-release")) {
		t.Errorf("export: %v, %d bytes", err, len(tarOut))
	}
	id := strings.TrimSpace(d.dok("commit", "-c", `CMD ["cat","/root/stopped.txt"]`, "fs", "committed:1"))
	if !strings.HasPrefix(id, "sha256:") {
		t.Errorf("commit: %q", id)
	}
	if out := d.dok("run", "--rm", "committed:1"); out != "copied in\n" {
		t.Errorf("run of the commit: %q", out)
	}
}

// basicAuthRegistry is a registry that wants user and password.
func basicAuthRegistry(t *testing.T, user, password string) string {
	t.Helper()
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1)
}

// TestDockerImages pulls, lists, tags, removes, saves, loads, imports,
// builds and pushes images, and logs in to a registry.
func TestDockerImages(t *testing.T) {
	d := newDockerAPI(t)
	there := mirrored(t, alpine)
	if out := d.dok("pull", there); !strings.Contains(out, "Pull complete") || !strings.Contains(out, "Status: Downloaded newer image") {
		t.Errorf("pull:\n%s", out)
	}
	if out := d.dok("pull", there); !strings.Contains(out, "Already exists") || !strings.Contains(out, "Image is up to date") {
		t.Errorf("pull again:\n%s", out)
	}
	img := d.image(alpine)
	id := d.imageID(img)
	if got := d.dok("image", "inspect", "-f", "{{.Id}} {{.Os}}/{{.Architecture}}", img); got != id+" linux/amd64\n" {
		t.Errorf("image inspect: %q, want ID %s", got, id)
	}
	if out := d.dok("images", "--filter", "reference=alpine*", "--format", "{{.Repository}}:{{.Tag}}"); out != "alpine:3.20\n" {
		t.Errorf("images --filter: %q", out)
	}
	if out := d.dok("history", "-q", img); !strings.HasPrefix(out, strings.TrimPrefix(id, "sha256:")[:12]) {
		t.Errorf("history: %q", out)
	}

	d.dok("run", "-d", "--name", "user", img, "sleep", "600")
	if r := d.docker1("rmi", img); r.code == 0 || !strings.Contains(r.stderr, "used by container") {
		t.Errorf("rmi of an image in use: %+v", r)
	}
	d.dok("rm", "-f", "user")

	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar")
	d.dok("save", "-o", archive, img)
	d.dok("rmi", img, there)
	if out := d.dok("load", "-i", archive); !strings.Contains(out, "Loaded image: alpine:3.20") {
		t.Errorf("load: %q", out)
	}
	if d.imageID(img) != id {
		t.Errorf("loaded as %s, want %s", d.imageID(img), id)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := d.dockerIn(tarOfFirstLayer(t, archive), "import", "-", "imported:1")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "sha256:") {
		t.Errorf("import: %+v", r)
	}

	ctx := buildDir(t, map[string]string{
		"Dockerfile":    "FROM " + img + " AS base\nARG GREETING=hi\nCOPY hello.txt /hello.txt\nRUN echo \"$GREETING\" > /greet\nFROM base AS final\nCMD [\"cat\", \"/greet\", \"/hello.txt\"]\n",
		"hello.txt":     "from the context\n",
		"secret.txt":    "kept out\n",
		".dockerignore": "secret.txt\n",
	})
	out := d.dok("build", "-t", "built:1", "--build-arg", "GREETING=hey", "--target", "final", ctx)
	if !strings.Contains(out, "Successfully built") || !strings.Contains(out, "Successfully tagged built:1") {
		t.Errorf("build:\n%s", out)
	}
	if got := d.dok("run", "--rm", "built:1"); got != "hey\nfrom the context\n" {
		t.Errorf("run of the build: %q", got)
	}
	if r := d.docker1("run", "--rm", "built:1", "ls", "/secret.txt"); r.code == 0 {
		t.Errorf(".dockerignore let secret.txt in")
	}
	builtID := d.imageID("built:1")
	if again := strings.TrimSpace(d.dok("build", "-q", "-t", "built:1", "--build-arg", "GREETING=hey", "--target", "final", ctx)); again != builtID {
		t.Errorf("rebuilt from the cache as %q, want %s", again, builtID)
	}
	d.ok("build", "-t", "built:cli", "--build-arg", "GREETING=hey", "--target", "final", ctx)
	if cliID := d.imageID("built:cli"); cliID != builtID {
		t.Errorf("runwhp build made %s, the API's %s", cliID, builtID)
	}

	host, uploads := countingRegistry(t)
	d.dok("tag", "built:1", host+"/b/img:1")
	if out := d.dok("push", host+"/b/img:1"); !strings.Contains(out, "1: digest: sha256:") || !strings.Contains(out, "Pushed") {
		t.Errorf("push:\n%s", out)
	}
	n := uploads.Load()
	d.dok("push", host+"/b/img:1")
	if uploads.Load() != n {
		t.Errorf("a second push uploaded %d blobs", uploads.Load()-n)
	}

	auth := basicAuthRegistry(t, "e2e", "secret")
	if r := d.docker1("login", auth, "-u", "e2e", "-p", "wrong"); r.code == 0 {
		t.Errorf("login with a wrong password: %+v", r)
	}
	if out := d.dok("login", auth, "-u", "e2e", "-p", "secret"); !strings.Contains(out, "Login Succeeded") {
		t.Errorf("login: %q", out)
	}
	d.dok("tag", "built:1", auth+"/b/img:1")
	d.dok("push", auth+"/b/img:1")
	// runwhp's own pull finds the credentials docker login stored.
	cmd := exec.Command(runwhp(t), "pull", auth+"/b/img:1")
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+t.TempDir(), "DOCKER_CONFIG="+d.config)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("runwhp pull with docker login's credentials: %v\n%s", err, out)
	}
}

// tarOfFirstLayer gives the first layer of a docker save archive, as a
// root file system tar to import.
func tarOfFirstLayer(t *testing.T, archive string) io.Reader {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	files := readTar(t, data)
	var manifest []struct{ Layers []string }
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil || len(manifest) == 0 {
		t.Fatalf("manifest.json: %v", err)
	}
	return bytes.NewReader(files[manifest[0].Layers[0]])
}

// TestDockerNetworksVolumes makes networks and volumes, runs containers on
// them, and publishes a port.
func TestDockerNetworksVolumes(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	d.dok("network", "create", "--label", "k=v", "appnet")
	if out := d.dok("network", "ls", "--format", "{{.Name}} {{.Driver}}"); !strings.Contains(out, "appnet bridge") || !strings.Contains(out, "bridge bridge") {
		t.Errorf("network ls:\n%s", out)
	}
	d.dok("run", "-d", "--name", "db", "--network", "appnet", "--network-alias", "database", img, "sleep", "600")
	if out := d.dok("run", "--rm", "--network", "appnet", img, "sh", "-c", "ping -c1 -W3 database >/dev/null && ping -c1 -W3 db >/dev/null && echo reached"); out != "reached\n" {
		t.Errorf("by alias and name: %q", out)
	}
	if out := d.dok("network", "inspect", "-f", "{{range .Containers}}{{.Name}}{{end}} {{.Labels.k}}", "appnet"); out != "db v\n" {
		t.Errorf("network inspect: %q", out)
	}
	if r := d.docker1("network", "rm", "appnet"); r.code == 0 {
		t.Errorf("removed a network in use")
	}
	d.dok("create", "--name", "joiner", "--network", "none", img, "true")
	d.dok("rm", "-f", "db", "joiner")
	d.dok("network", "rm", "appnet")
	d.dok("network", "create", "spare")
	if out := d.dok("network", "prune", "-f"); !strings.Contains(out, "spare") {
		t.Errorf("network prune: %q", out)
	}

	d.dok("volume", "create", "--label", "k=v", "data")
	if out := d.dok("volume", "inspect", "-f", "{{.Name}} {{.Driver}} {{.Labels.k}}", "data"); out != "data local v\n" {
		t.Errorf("volume inspect: %q", out)
	}
	d.dok("run", "--rm", "-v", "data:/data", img, "sh", "-c", "echo kept > /data/f")
	if out := d.dok("run", "--rm", "-v", "data:/data", img, "cat", "/data/f"); out != "kept\n" {
		t.Errorf("volume content: %q", out)
	}
	if out := d.dok("volume", "ls", "-q", "--filter", "label=k=v"); out != "data\n" {
		t.Errorf("volume ls: %q", out)
	}
	d.dok("volume", "rm", "data")
	d.dok("volume", "create", "spare")
	if out := d.dok("volume", "prune", "-a", "-f"); !strings.Contains(out, "spare") {
		t.Errorf("volume prune -a: %q", out)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	d.dok("run", "-d", "--name", "web", "-p", fmt.Sprintf("%d:80", port), img, "sh", "-c", "while true; do echo served | nc -l -p 80; done")
	if out := d.dok("port", "web"); !strings.Contains(out, fmt.Sprintf("80/tcp -> 127.0.0.1:%d", port)) {
		t.Errorf("port: %q", out)
	}
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && got == ""; time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			got, _ = bufio.NewReader(c).ReadString('\n')
			c.Close()
		}
	}
	if got != "served\n" {
		t.Errorf("published port gave %q", got)
	}

	// Random host ports, with -p PORT and with -P over the image's and
	// --expose's ports, as docker port and inspect report them.
	d.dok("run", "-d", "--name", "rand", "-p", "80", "--expose", "9000", "-P", img, "sh", "-c",
		"nc -lk -p 9000 -e echo nine & while true; do echo served | nc -l -p 80; done")
	addr := strings.TrimSpace(d.dok("port", "rand", "80"))
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Fatalf("port rand 80: %q", addr)
	}
	if out := d.dok("port", "rand"); !strings.Contains(out, "80/tcp -> "+addr) || !strings.Contains(out, "9000/tcp -> 127.0.0.1:") {
		t.Errorf("port rand: %q", out)
	}
	got = ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && got == ""; time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			got, _ = bufio.NewReader(c).ReadString('\n')
			c.Close()
		}
	}
	if got != "served\n" {
		t.Errorf("random port %s gave %q", addr, got)
	}
	var insp []struct {
		Config     struct{ ExposedPorts map[string]any }
		HostConfig struct {
			PublishAllPorts bool
			PortBindings    map[string][]struct{ HostIp, HostPort string }
		}
		NetworkSettings struct {
			Ports map[string][]struct{ HostIp, HostPort string }
		}
	}
	if err := json.Unmarshal([]byte(d.dok("inspect", "rand")), &insp); err != nil || len(insp) != 1 {
		t.Fatalf("inspect rand: %v", err)
	}
	if b := insp[0].HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostPort != "" {
		t.Errorf("PortBindings %+v: want 80/tcp asked for with no host port", insp[0].HostConfig.PortBindings)
	}
	if p := insp[0].NetworkSettings.Ports["80/tcp"]; len(p) != 1 || "127.0.0.1:"+p[0].HostPort != addr {
		t.Errorf("Ports %+v: want 80/tcp at %s", insp[0].NetworkSettings.Ports, addr)
	}
	if _, ok := insp[0].Config.ExposedPorts["9000/tcp"]; !ok {
		t.Errorf("ExposedPorts %v", insp[0].Config.ExposedPorts)
	}
}

// TestDockerSystem shows stats, disk use and events, prunes, and has the
// API and runwhp's own commands see the same containers, through a
// restart of the server.
func TestDockerSystem(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	d.dok("run", "-d", "--name", "busy", img, "sh", "-c", "while true; do :; done")
	if out := d.dok("stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}} {{.PIDs}}", "busy"); !strings.HasPrefix(out, "busy ") || strings.HasPrefix(strings.Fields(out)[1], "0.00") {
		t.Errorf("stats: %q", out)
	}
	if out := d.dok("system", "df"); !strings.Contains(out, "Images") || !strings.Contains(out, "Containers") {
		t.Errorf("system df:\n%s", out)
	}
	// Made by runwhp, seen by the API; made by the API, seen by runwhp.
	d.ok(keepArgs("-d", "--name", "native", img, "sleep", "600")...)
	if out := d.dok("ps", "--format", "{{.Names}}", "--filter", "name=native"); out != "native\n" {
		t.Errorf("docker ps of runwhp's container: %q", out)
	}
	if out := d.ok("list", "-q").stdout; len(lines(out)) != 2 {
		t.Errorf("runwhp list: %q", out)
	}
	// The server stops; the containers go on, and it serves them again.
	d.ok("docker-api", "stop", "--pipe", d.pipe)
	if r := d.docker1("ps"); r.code == 0 {
		t.Errorf("docker ps with the server stopped: %+v", r)
	}
	d.ok("docker-api", "start", "--pipe", d.pipe)
	if out := d.dok("ps", "--format", "{{.Names}}"); !strings.Contains(out, "busy") || !strings.Contains(out, "native") {
		t.Errorf("after the server's restart: %q", out)
	}
	d.dok("rm", "-f", "busy", "native")
	d.dok("run", "--name", "done", img, "true")
	if out := d.dok("system", "prune", "-f"); !strings.Contains(out, "Deleted Containers") {
		t.Errorf("system prune:\n%s", out)
	}
}

// TestDockerAPIShapes compares the keys of runwhp's answers with Docker
// 29.10's, as captured in cmd\runwhp\testdata\dockerapi\docker29: a key
// Docker gives that runwhp does not is a client's field gone missing.
func TestDockerAPIShapes(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	d.dok("network", "create", "--label", "k=v", "shape-net")
	d.dok("volume", "create", "--label", "k=v", "shape-vol")
	d.dok("run", "-d", "--name", "shape-run", "--label", "k=v", "--network", "shape-net", "-p", "127.0.0.1:18081:80",
		"-v", "shape-vol:/data", "--health-cmd", "true", "--health-interval", "1s", img, "sleep", "600")
	d.docker1("run", "--name", "shape-exit", img, "sh", "-c", "exit 3")
	// A build, for the build cache system df lists.
	d.dok("build", "-q", "-t", "shape-built", buildDir(t, map[string]string{"Dockerfile": "FROM " + img + "\nRUN echo > /built\n"}))
	time.Sleep(2 * time.Second)

	// Keys runwhp leaves out, and why: features it has not, and Docker's
	// own machinery.
	absent := map[string]bool{
		// The storage driver's data, and image descriptors: runwhp keeps
		// ext4 layers, and no manifests.
		"GraphDriver": true, "Storage": true, "ImageManifestDescriptor": true, "Descriptor": true, "Manifests": true, "Identity": true,
		// What only a daemon with containerd, swarm or plugins has.
		"ContainerdCommit": true, "RuncCommit": true, "InitCommit": true, "Containerd": true, "RegistryConfig": true,
		"GenericResources": true, "DefaultAddressPools": true, "FirewallBackend": true, "DiscoveredDevices": true,
		"NRI": true, "Swarm.": true, "ProductLicense": true, "SystemStatus": true, "CDISpecDirs": true,
		"HostConfig.Annotations": true,
		// The guest's cgroup statistics, of which runwhp's stats give the
		// memory used and cached alone, and a pids limit it does not set.
		"memory_stats.stats.": true, "pids_stats.limit": true,
		// Sizes that are 0 here, which the types leave out.
		"BuildCacheUsage.Reclaimable": true, "BuildCacheUsage.TotalSize": true, "ImageUsage.Reclaimable": true,
		"VolumeUsage.Reclaimable": true,
		// A misspelt key of Docker's own, " Parents".
		"BuildCacheUsage.Items[]. Parents": true, "BuildCache[]. Parents": true,
	}
	skip := func(k string) bool {
		for p := range absent {
			if k == p || strings.HasPrefix(k, p+".") || strings.HasPrefix(k, p+"[]") || strings.HasPrefix(k, p) && strings.HasSuffix(p, ".") {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct{ fixture, path string }{
		{"version.json", "/version"},
		{"info.json", "/info"},
		{"container-running.json", "/containers/shape-run/json"},
		{"container-exited.json", "/containers/shape-exit/json"},
		{"containers.json", "/containers/json?all=1"},
		{"stats.json", "/containers/shape-run/stats?stream=0&one-shot=1"},
		{"images.json", "/images/json"},
		{"image.json", "/images/alpine:3.20/json"},
		{"history.json", "/images/alpine:3.20/history"},
		{"network.json", "/networks/shape-net"},
		{"networks.json", "/networks"},
		{"volume.json", "/volumes/shape-vol"},
		{"df.json", "/system/df"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "cmd", "runwhp", "testdata", "dockerapi", "docker29", tc.fixture))
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		d.get(tc.path, &got)
		have := map[string]bool{}
		for _, k := range jsonKeys(got, "") {
			have[k] = true
		}
		var missing []string
		for _, k := range jsonKeys(want, "") {
			if !have[k] && !skip(k) && !strings.Contains(k, "Labels.") && !strings.Contains(k, "Options.") &&
				!strings.Contains(k, "Networks.") && !strings.Contains(k, "Ports.") && !strings.Contains(k, "ExposedPorts.") &&
				!strings.Contains(k, "Runtimes.") && !strings.Contains(k, "Containers.") && !strings.Contains(k, "Subnets.") &&
				!strings.Contains(k, "Volumes.") && !strings.Contains(k, "networks.") && !strings.Contains(k, ".Details.") {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s: runwhp's answer lacks Docker's keys:\n  %s", tc.path, strings.Join(missing, "\n  "))
		}
	}
}

// jsonKeys lists the keys of a JSON value, dotted, with [] for an array's
// elements, all of them merged.
func jsonKeys(v any, prefix string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := prefix + k
			out = append(out, p)
			out = append(out, jsonKeys(e, p+".")...)
		}
	case []any:
		seen := map[string]bool{}
		for _, e := range x {
			for _, k := range jsonKeys(e, strings.TrimSuffix(prefix, ".")+"[].") {
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
	}
	return out
}

// readTar reads a tar's regular files, by name.
func readTar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[hdr.Name] = b
		}
	}
}
