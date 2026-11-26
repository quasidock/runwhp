//go:build windows

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

const dockerEngine = `\\.\pipe\docker_engine`

func pipeServed(pipe string) bool {
	timeout := 200 * time.Millisecond
	c, err := winio.DialPipe(pipe, &timeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// dockerNoHost runs the docker CLI with no DOCKER_HOST, no context and a
// configuration of its own, so that it looks for docker_engine, as tools
// that take no context do.
func dockerNoHost(t *testing.T, docker string, args ...string) result {
	t.Helper()
	cmd := exec.Command(docker, args...)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "DOCKER_HOST") && !strings.EqualFold(k, "DOCKER_CONTEXT") &&
			!strings.EqualFold(k, "DOCKER_CONFIG") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+t.TempDir(), "DOCKER_CLI_HINTS=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// TestDockerEnginePipe serves docker_engine with docker-api start
// --docker-engine, where the docker CLI finds it with no context or
// DOCKER_HOST; a second server, or one started while something else
// serves docker_engine, is refused, and stop gives the pipe back.
func TestDockerEnginePipe(t *testing.T) {
	docker := os.Getenv("RUNWHP_E2E_DOCKER")
	if docker == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_DOCKER to docker.exe to run the docker CLI's tests")
	}
	if pipeServed(dockerEngine) {
		t.Skip("something on this machine serves docker_engine already")
	}
	s := newOwnStore(t)
	pipe := fmt.Sprintf(`\\.\pipe\runwhp_engine-e2e-%d-%d`, os.Getpid(), pipeSeq.Add(1))
	r := s.ok("docker-api", "start", "--pipe", pipe, "--docker-engine")
	t.Cleanup(func() { s.cli("", "docker-api", "stop", "--pipe", pipe) })
	if !strings.Contains(r.stdout, dockerEngine) || !strings.Contains(r.stdout, "no\ncontext or DOCKER_HOST") {
		t.Errorf("start --docker-engine:\n%s", r.stdout)
	}
	if out := dockerNoHost(t, docker, "version", "--format", "{{.Server.Platform.Name}}"); strings.TrimSpace(out.stdout) != "runwhp" {
		t.Errorf("docker version with no DOCKER_HOST: %+v", out)
	}
	if r := s.ok("docker-api", "status", "--pipe", pipe); !strings.Contains(r.stdout, pipe+" and "+dockerEngine) {
		t.Errorf("status:\n%s", r.stdout)
	}
	// Starting it again says it serves both.
	if r := s.ok("docker-api", "start", "--pipe", pipe, "--docker-engine"); !strings.Contains(r.stdout, "already serves") {
		t.Errorf("start again:\n%s", r.stdout)
	}

	// Another store's server cannot have docker_engine too.
	s2 := newOwnStore(t)
	pipe2 := fmt.Sprintf(`\\.\pipe\runwhp_engine-e2e-%d-%d`, os.Getpid(), pipeSeq.Add(1))
	r = s2.cli("", "docker-api", "start", "--pipe", pipe2, "--docker-engine")
	t.Cleanup(func() { s2.cli("", "docker-api", "stop", "--pipe", pipe2) })
	if r.code == 0 || !strings.Contains(r.stderr, "already served") || pipeServed(pipe2) {
		t.Errorf("a second server for docker_engine: %+v", r)
	}

	s.ok("docker-api", "stop", "--pipe", pipe)
	if pipeServed(dockerEngine) || pipeServed(pipe) {
		t.Errorf("pipes still served after stop")
	}

	// Something else serving docker_engine, as Docker Desktop would.
	l, err := winio.ListenPipe(dockerEngine, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r = s.cli("", "docker-api", "start", "--pipe", pipe, "--docker-engine")
	if r.code == 0 || !strings.Contains(r.stderr, "Docker Desktop or another engine") || pipeServed(pipe) {
		t.Errorf("start --docker-engine with docker_engine taken: %+v", r)
	}
}
