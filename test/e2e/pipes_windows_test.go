//go:build windows

package e2e

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Microsoft/go-winio"
)

// sockcatDir builds sockcat, the tests' Unix socket client, for Linux,
// once, and gives the directory it is in, to bind into containers.
var sockcatDir = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "runwhp-sockcat-")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", filepath.Join(dir, "sockcat"), "./sockcat")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building sockcat: %v\n%s", err, out)
	}
	return dir, nil
})

func sockcat(t *testing.T) string {
	t.Helper()
	dir, err := sockcatDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir + ":/t:ro"
}

var echoSeq atomic.Int32

// echoPipe serves a named pipe of the test's own that answers each
// connection with "got:" and what it read: in message mode, all it read
// until the client's half-close, which it answers with its own; in byte
// mode, which carries no half-close, the first line. It gives the pipe's
// name as a bind source spells it.
func echoPipe(t *testing.T, message bool) (name, source string) {
	t.Helper()
	name = fmt.Sprintf(`\\.\pipe\runwhp-e2e-echo-%d-%d`, os.Getpid(), echoSeq.Add(1))
	l, err := winio.ListenPipe(name, &winio.PipeConfig{MessageMode: message})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if !message {
					if line, err := bufio.NewReader(c).ReadString('\n'); err == nil {
						io.WriteString(c, "got:"+line)
					}
					return
				}
				data, err := io.ReadAll(c)
				if err != nil {
					return
				}
				io.WriteString(c, "got:"+string(data))
				c.(interface{ CloseWrite() error }).CloseWrite()
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return name, "//./pipe/" + strings.TrimPrefix(name, `\\.\pipe\`)
}

// TestHostPipes binds named pipes into containers as Unix sockets: each
// connection reaches the pipe, half-closes included, many at once, from
// exec too, and again after a restart.
func TestHostPipes(t *testing.T) {
	_, msg := echoPipe(t, true)
	_, byteMode := echoPipe(t, false)
	bin := sockcat(t)

	r := runWithInput(t, strings.NewReader("hello"), "-i", "-v", bin, "-v", msg+":/var/run/echo.sock", alpine,
		"sh", "-c", "stat -c %a:%u:%g:%F /var/run/echo.sock; /t/sockcat /var/run/echo.sock; echo; /t/sockcat -n 100 /var/run/echo.sock m")
	expect(t, r, "660:0:0:socket\ngot:hello\n100 connections answered\n", 0)

	r = runWithInput(t, strings.NewReader("line\n"), "-i", "-v", bin, "--mount", "type=npipe,source="+byteMode+",target=/run/b.sock", alpine,
		"/t/sockcat", "/run/b.sock")
	expect(t, r, "got:line\n", 0)

	// Kept, exec'd into, restarted.
	name := fmt.Sprintf("pipes-%d", os.Getpid())
	if r := cli(t, keepArgs("-d", "--name", name, "-v", bin, "-v", msg+":/run/echo.sock", alpine, "sleep", "1d")...); r.code != 0 {
		t.Fatalf("run -d: %+v", r)
	}
	defer cli(t, "delete", "-f", name)
	execEcho := func(what string) {
		t.Helper()
		cmd := exec.Command(runwhp(t), "exec", "-i", name, "/t/sockcat", "/run/echo.sock")
		cmd.Stdin = strings.NewReader(what)
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "got:"+what {
			t.Errorf("exec %s: %q, %v", what, out, err)
		}
	}
	execEcho("before")
	if r := cli(t, "restart", "-t", "0", name); r.code != 0 {
		t.Fatalf("restart: %+v", r)
	}
	execEcho("after")
}

// TestHostPipeAbsent warns when nothing serves a pipe yet, and the
// container's connections to it end, while the container goes on.
func TestHostPipeAbsent(t *testing.T) {
	absent := fmt.Sprintf("//./pipe/runwhp-e2e-absent-%d", os.Getpid())
	r := runWithInput(t, strings.NewReader("x"), "-i", "-v", sockcat(t), "-v", absent+":/run/none.sock", alpine,
		"sh", "-c", "/t/sockcat /run/none.sock; echo done")
	if r.code != 0 || r.stdout != "done\n" {
		t.Errorf("run: %+v", r)
	}
	if !strings.Contains(r.stderr, "is not served yet") {
		t.Errorf("no warning: %q", r.stderr)
	}
}

// TestDockerSocketInContainer runs the docker CLI in a container with
// Docker's socket bound, through runwhp's Engine API: it reaches the
// server that made the container, runs a container attached, with input
// ended by a half-close, and removes it.
func TestDockerSocketInContainer(t *testing.T) {
	d := newDockerAPI(t)
	img := d.image(alpine)
	cliImg := d.image("docker:cli")
	pipeSource := "//./pipe/" + strings.TrimPrefix(d.pipe, `\\.\pipe\`)
	for _, src := range []string{"/var/run/docker.sock", "//var/run/docker.sock", pipeSource} {
		out := d.dok("run", "--rm", "-v", src+":/var/run/docker.sock", cliImg, "docker", "version", "--format", "{{.Server.Os}} {{.Server.APIVersion}}")
		if !strings.HasPrefix(out, "linux 1.") {
			t.Errorf("-v %s: docker version printed %q", src, out)
		}
	}
	script := fmt.Sprintf("docker run --rm %s echo nested && echo -n piped | docker run --rm -i %s cat && echo && docker ps -a --format '{{.Image}}'", img, img)
	out := d.dok("run", "--rm", "--name", "outer", "-v", "/var/run/docker.sock:/var/run/docker.sock", cliImg, "sh", "-c", script)
	if !strings.HasPrefix(out, "nested\npiped\n") || !strings.Contains(out, "docker:cli") {
		t.Errorf("the docker CLI in a container printed %q", out)
	}
	// The mount as Docker's inspect shows a bind of its socket.
	d.dok("create", "--name", "sock", "-v", "/var/run/docker.sock:/var/run/docker.sock", cliImg, "true")
	got := strings.TrimSpace(d.dok("inspect", "--format", "{{range .Mounts}}{{.Type}} {{.Source}} {{.Destination}} {{.RW}}{{end}}", "sock"))
	if got != "bind /var/run/docker.sock /var/run/docker.sock true" {
		t.Errorf("inspect mounts %q", got)
	}
}

// apiSocketCompose is a service with use_api_socket that prints what it
// finds: the API through the socket, and the credentials.
const apiSocketCompose = `services:
  ci:
    image: docker:cli
    use_api_socket: true
    command: ["sh", "-c", "docker version --format '{{.Server.Os}}' && cat $${DOCKER_CONFIG:-/none}/config.json"]
`

// apiSocketAuths is a credential the docker CLI has stored, as Docker
// Compose gives it to a service with use_api_socket.
const apiSocketAuths = `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`

// TestComposeAPISocket runs a service with use_api_socket with runwhp
// compose, on the store's own Engine API, and with Docker Compose through
// the API: each reaches the API, and finds the docker CLI's credentials.
func TestComposeAPISocket(t *testing.T) {
	s := newOwnStore(t)
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config) // start writes its context there, compose reads the credentials
	os.WriteFile(filepath.Join(config, "config.json"), []byte(apiSocketAuths), 0o644)
	s.ok("docker-api", "start")
	t.Cleanup(func() { s.cli("", "docker-api", "stop") })
	there := mirrored(t, "docker:cli")
	s.ok("pull", there)
	s.ok("tag", there, "docker:cli")

	p := newComposeProject(t, s, map[string]string{"compose.yaml": apiSocketCompose})
	p.ok("up", "-d")
	s.ok("wait", "proj-ci-1")
	if out := p.ok("logs", "--no-log-prefix", "ci").stdout; !strings.Contains(out, "linux\n") || !strings.Contains(out, "registry.example.com") {
		t.Errorf("runwhp compose: the service printed %q", out)
	}
	// A foreground run gets the socket, not the credentials.
	r := p.run("run", "--rm", "-T", "ci")
	if r.code != 1 || !strings.Contains(r.stdout, "linux\n") || !strings.Contains(r.stderr, "detached runs only") {
		t.Errorf("runwhp compose run: %+v", r)
	}

	plugin := os.Getenv("RUNWHP_E2E_COMPOSE_PLUGIN")
	if plugin == "" {
		return
	}
	d := newDockerAPI(t)
	d.image("docker:cli")
	installComposePlugin(t, d, plugin)
	os.WriteFile(filepath.Join(d.config, "config.json"), []byte(apiSocketAuths), 0o644)
	dir := buildDir(t, map[string]string{"compose.yaml": apiSocketCompose})
	t.Cleanup(func() { d.run(dir, nil, d.docker, "compose", "-p", "uas", "down", "-t", "0") })
	up := d.run(dir, nil, d.docker, "compose", "-p", "uas", "up")
	if up.code != 0 || !strings.Contains(up.stdout, "linux\n") || !strings.Contains(up.stdout, "registry.example.com") {
		t.Errorf("Docker Compose: %+v", up)
	}
}
