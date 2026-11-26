//go:build windows

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// composeProject is a compose project in a directory of its own, run on a
// store of the test's own.
type composeProject struct {
	s   *ownStore
	dir string
}

func newComposeProject(t *testing.T, s *ownStore, files map[string]string) *composeProject {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &composeProject{s, dir}
	t.Cleanup(func() { p.run("down", "-t", "0", "--remove-orphans") })
	return p
}

func (p *composeProject) command(args ...string) *exec.Cmd {
	cmd := exec.Command(runwhp(p.s.t), append([]string{"compose"}, args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+p.s.root)
	return cmd
}

func (p *composeProject) run(args ...string) result {
	p.s.t.Helper()
	cmd := p.command(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

func (p *composeProject) ok(args ...string) result {
	p.s.t.Helper()
	r := p.run(args...)
	if r.code != 0 {
		p.s.t.Fatalf("runwhp compose %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// ids are the project's containers' IDs by name, from ps -a --format json.
func (p *composeProject) ids() map[string]string {
	p.s.t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(p.ok("ps", "-a", "--format", "json").stdout), "\n") {
		if line == "" {
			continue
		}
		var c struct{ Name, ID string }
		json.Unmarshal([]byte(line), &c)
		out[c.Name] = c.ID
	}
	return out
}

const composeDB = `
  db:
    image: %ALPINE%
    command: ["sh", "-c", "echo db starting; sleep 2; touch /tmp/ready; echo db ready; exec nc -lk -p 5000 -e echo hi"]
    healthcheck:
      test: ["CMD-SHELL", "test -f /tmp/ready"]
      interval: 1s
      start_period: 10s
      start_interval: 300ms
    volumes: [data:/data]
    networks: [back]
`

func composeFile(t *testing.T, body string) string {
	return strings.NewReplacer("%ALPINE%", mirrored(t, alpine), "%BUSYBOX%", mirrored(t, busybox)).Replace(body)
}

// TestComposeUpDown runs a project as people run one: a database with a
// healthcheck, an app built from a Dockerfile that waits for it, a web
// server publishing a port, a volume and two networks; then takes it down.
func TestComposeUpDown(t *testing.T) {
	s := newOwnStore(t)
	port := freePort(t)
	p := newComposeProject(t, s, map[string]string{
		"compose.yaml": composeFile(t, `name: e2eapp
services:`+composeDB+`
  app:
    build: ./app
    environment: {GREETING: "${GREETING:-hello}"}
    depends_on: {db: {condition: service_healthy}}
    networks: [back, front]
  web:
    image: %BUSYBOX%
    command: ["sh", "-c", "mkdir -p /www && echo web-ok > /www/index.html && exec httpd -f -p 80 -h /www"]
    ports: ["`+port+`:80"]
    networks:
      front: {aliases: [www]}
    depends_on: [app]
volumes:
  data: {}
networks:
  front: {}
  back: {}
`),
		"app/Dockerfile": "FROM " + mirrored(t, alpine) + "\nLABEL from.image=yes\nCMD [\"sh\", \"-c\", \"echo app says $GREETING; exec sleep 1d\"]\n",
	})
	r := p.ok("up", "-d")
	for _, want := range []string{" Image e2eapp-app Built \n", " Network e2eapp_back Created \n", " Volume e2eapp_data Created \n",
		" Container e2eapp-db-1 Healthy \n", " Container e2eapp-web-1 Started \n"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("up -d's output has no %q:\n%s", want, r.stderr)
		}
	}
	// db is healthy before app starts.
	if i, j := strings.Index(r.stderr, "e2eapp-db-1 Healthy"), strings.Index(r.stderr, "e2eapp-app-1 Starting"); i < 0 || j < i {
		t.Errorf("app started before db was healthy:\n%s", r.stderr)
	}

	ps := p.ok("ps").stdout
	if !strings.HasPrefix(ps, "NAME") || !regexp.MustCompile(`e2eapp-db-1 .* db .*\(healthy\)`).MatchString(ps) || strings.Count(ps, "\n") != 4 {
		t.Errorf("ps:\n%s", ps)
	}
	if got := p.ok("ps", "--services").stdout; got != "app\ndb\nweb\n" {
		t.Errorf("ps --services: %q", got)
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "web-ok\n" {
		t.Errorf("web from the host: %q", body)
	}
	if got := p.ok("exec", "-T", "app", "sh", "-c", "wget -qO- http://www/; nc db 5000 </dev/null").stdout; got != "web-ok\nhi\n" {
		t.Errorf("web and db from app: %q", got)
	}
	if got := p.ok("logs", "--no-log-prefix", "app").stdout; got != "app says hello\n" {
		t.Errorf("logs app: %q", got)
	}
	if got := p.ok("port", "web", "80").stdout; got != "127.0.0.1:"+port+"\n" {
		t.Errorf("port: %q", got)
	}
	var st struct{ Labels map[string]string }
	json.Unmarshal([]byte(s.ok("state", "e2eapp-app-1").stdout), &st)
	for k, v := range map[string]string{"com.docker.compose.project": "e2eapp", "com.docker.compose.service": "app",
		"com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
		"com.docker.compose.depends_on": "db:service_healthy:false", "from.image": "yes"} {
		if st.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, st.Labels[k], v)
		}
	}
	if got := p.ok("ls").stdout; !strings.Contains(got, "e2eapp") || !strings.Contains(got, "running(3)") {
		t.Errorf("ls:\n%s", got)
	}

	r = p.ok("down", "-v", "--rmi", "local")
	for _, want := range []string{" Container e2eapp-db-1 Removed \n", " Volume e2eapp_data Removed \n", " Network e2eapp_front Removed \n", " Image e2eapp-app:latest Removed \n"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("down's output has no %q:\n%s", want, r.stderr)
		}
	}
	// web stops before app, and app before db.
	if i, j := strings.Index(r.stderr, "e2eapp-web-1 Stopping"), strings.Index(r.stderr, "e2eapp-db-1 Stopping"); i < 0 || j < i {
		t.Errorf("down's order:\n%s", r.stderr)
	}
	if got := s.ok("list", "-q").stdout; got != "" {
		t.Errorf("containers left: %q", got)
	}
	if got := s.ok("network", "ls", "-q", "-f", "type=custom").stdout; got != "" {
		t.Errorf("networks left: %q", got)
	}
	if got := s.ok("volume", "ls", "-q").stdout; got != "" {
		t.Errorf("volumes left: %q", got)
	}
}

// TestComposeRecreate changes a project and ups it again.
func TestComposeRecreate(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{"compose.yaml": composeFile(t, `name: e2erec
services:
  a: {image: "%ALPINE%", command: [sleep, 1d], environment: {V: "${V:-1}"}}
  b: {image: "%ALPINE%", command: [sleep, 1d]}
`)})
	p.ok("up", "-d")
	before := p.ids()
	if r := p.ok("up", "-d"); strings.Contains(r.stderr, "Recreate") || !strings.Contains(r.stderr, " Container e2erec-a-1 Running \n") {
		t.Errorf("an unchanged up:\n%s", r.stderr)
	}
	cmd := p.command("up", "-d")
	cmd.Env = append(cmd.Env, "V=2")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), " Container e2erec-a-1 Recreated \n") || strings.Contains(string(out), "e2erec-b-1 Recreate") {
		t.Errorf("up with a changed environment: %v\n%s", err, out)
	}
	after := p.ids()
	if after["e2erec-a-1"] == before["e2erec-a-1"] || after["e2erec-b-1"] != before["e2erec-b-1"] || len(after) != 2 {
		t.Errorf("IDs before %v, after %v", before, after)
	}
	if r := p.ok("up", "-d", "--force-recreate", "b"); !strings.Contains(r.stderr, " Container e2erec-b-1 Recreated \n") {
		t.Errorf("--force-recreate:\n%s", r.stderr)
	}
	if r := p.ok("up", "-d", "--no-recreate"); strings.Contains(r.stderr, "Recreate") {
		t.Errorf("--no-recreate:\n%s", r.stderr)
	}

	// Orphans.
	os.WriteFile(filepath.Join(p.dir, "compose.yaml"), []byte(composeFile(t, "name: e2erec\nservices:\n  a: {image: \"%ALPINE%\", command: [sleep, 1d]}\n")), 0o644)
	if r := p.ok("up", "-d"); !strings.Contains(r.stderr, "Found orphan containers (e2erec-b-1) for this project.") {
		t.Errorf("an orphan:\n%s", r.stderr)
	}
	if r := p.ok("up", "-d", "--remove-orphans"); !strings.Contains(r.stderr, " Container e2erec-b-1 Removed \n") {
		t.Errorf("--remove-orphans:\n%s", r.stderr)
	}
}

// TestComposeScale scales a service up and down, and finds its replicas by
// its name.
func TestComposeScale(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{"compose.yaml": composeFile(t, `name: e2esc
services:
  w: {image: "%ALPINE%", command: [sleep, 1d]}
  q: {image: "%ALPINE%", command: [sleep, 1d]}
`)})
	p.ok("up", "-d", "--scale", "w=3")
	// A replica just booted may be missing from answers for up to 2 s.
	var got string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		got = strings.TrimSpace(p.ok("exec", "-T", "q", "sh", "-c", "nslookup w 2>&1 | grep -c '^Address: 172'").stdout)
		if got == "3" || time.Now().After(deadline) {
			break
		}
	}
	if got != "3" {
		t.Errorf("w resolves to %s addresses, want 3", got)
	}
	r := p.ok("up", "-d", "--scale", "w=1")
	if !strings.Contains(r.stderr, " Container e2esc-w-3 Removed \n") || !strings.Contains(r.stderr, " Container e2esc-w-2 Removed \n") {
		t.Errorf("scaling down:\n%s", r.stderr)
	}
	ids := p.ids()
	if _, ok := ids["e2esc-w-1"]; !ok || len(ids) != 2 {
		t.Errorf("left %v", ids)
	}
}

const attachProject = `name: e2eatt
services:
  a:
    image: "%ALPINE%"
    command: ["sh", "-c", "trap 'echo a got TERM; exit 0' TERM; echo a line1; echo a err >&2; printf 'a partial'; sleep 1; echo ' done'; while :; do sleep 0.2; done"]
  bb:
    image: "%ALPINE%"
    command: ["sh", "-c", "echo bb hello; sleep 2; echo bb bye; exit 3"]
`

// TestComposeAttached follows the containers' output as up does without
// -d, and stops them as it does.
func TestComposeAttached(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{"compose.yaml": composeFile(t, attachProject)})
	r := p.run("up", "--abort-on-container-exit")
	if r.code != 3 {
		t.Errorf("--abort-on-container-exit: exit %d, want bb's 3\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"Attaching to a-1, bb-1\n", "a-1   | a line1\n", "a-1   | a partial done\n", "bb-1  | bb bye\n",
		"\r\x1b[Kbb-1 exited with code 3\n", "a-1   | a got TERM\n", "\r\x1b[Ka-1 exited with code 0\n"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("up's output has no %q:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, "a-1   | a err\n") || !strings.Contains(r.stderr, " Compose Stopping Aborting on container exit...\n") {
		t.Errorf("up's errors:\n%s", r.stderr)
	}
	p.ok("down", "-t", "0")
	if r := p.run("up", "--exit-code-from", "a"); r.code != 0 {
		t.Errorf("--exit-code-from a: exit %d", r.code)
	}
	p.ok("down", "-t", "0")
	if r := p.run("up", "bb"); r.code != 0 || !strings.Contains(r.stdout, "bb-1 exited with code 3") {
		t.Errorf("up bb: exit %d\n%s", r.code, r.stdout)
	}
	p.ok("down", "-t", "0")

	// Ctrl+C stops them all.
	cmd := p.command("up")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(stdout.String(), "a line1"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("up never showed a's output:\n%s", stderr.String())
		}
	}
	const ctrlBreak = 1
	if r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent").Call(ctrlBreak, uintptr(cmd.Process.Pid)); r == 0 {
		cmd.Process.Kill()
		t.Skipf("cannot send Ctrl+Break here (no shared console?): %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(err); code != 130 {
			t.Errorf("exit %d after Ctrl+C", code)
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("up did not stop after Ctrl+C")
	}
	if !strings.Contains(stderr.String(), " Compose Stopping Gracefully Stopping... press Ctrl+C again to force\n") {
		t.Errorf("after Ctrl+C:\n%s", stderr.String())
	}
	if got := p.ok("ps").stdout; strings.Count(got, "\n") != 1 {
		t.Errorf("still running after Ctrl+C:\n%s", got)
	}
}

// TestComposeDepends waits for dependencies as depends_on asks.
func TestComposeDepends(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{
		"ok.yaml": composeFile(t, `name: e2edep
services:
  mig: {image: "%ALPINE%", command: ["sh", "-c", "echo migrating; sleep 1"]}
  app: {image: "%ALPINE%", command: [sleep, 1d], depends_on: {mig: {condition: service_completed_successfully}}}
`),
		"failed.yaml": composeFile(t, `name: e2edep
services:
  mig: {image: "%ALPINE%", command: ["sh", "-c", "exit 4"]}
  app: {image: "%ALPINE%", command: [sleep, 1d], depends_on: {mig: {condition: service_completed_successfully}}}
`),
		"unhealthy.yaml": composeFile(t, `name: e2edep
services:
  db: {image: "%ALPINE%", command: [sleep, 1d], healthcheck: {test: ["CMD", "false"], interval: 1s, retries: 2}}
  app: {image: "%ALPINE%", command: [sleep, 1d], depends_on: {db: {condition: service_healthy}}}
`),
	})
	if r := p.ok("-f", "ok.yaml", "up", "-d"); !strings.Contains(r.stderr, " Container e2edep-mig-1 Exited \n") || !strings.Contains(r.stderr, " Container e2edep-app-1 Started \n") {
		t.Errorf("a completed dependency:\n%s", r.stderr)
	}
	p.ok("-f", "ok.yaml", "down", "-t", "0")
	r := p.run("-f", "failed.yaml", "up", "-d")
	if r.code != 1 || !strings.Contains(r.stderr, `runwhp: service "mig" didn't complete successfully: exit 4`) ||
		!strings.Contains(r.stderr, ` Container e2edep-app-1 not started: condition "service_completed_successfully" of service "mig" not met `) {
		t.Errorf("a failed dependency: %+v", r)
	}
	p.ok("-f", "failed.yaml", "down", "-t", "0")
	r = p.run("-f", "unhealthy.yaml", "up", "-d")
	if r.code != 1 || !strings.Contains(r.stderr, "runwhp: dependency failed to start: container e2edep-db-1 is unhealthy") {
		t.Errorf("an unhealthy dependency: %+v", r)
	}
	if r := p.run("-f", "unhealthy.yaml", "up", "-d", "--wait", "db"); r.code != 1 || !strings.Contains(r.stderr, "container e2edep-db-1 is unhealthy") {
		t.Errorf("--wait on an unhealthy service: %+v", r)
	}
}

// TestComposeCommands covers the rest of compose's commands.
func TestComposeCommands(t *testing.T) {
	s := newOwnStore(t)
	mirrored(t, alpine) // starts the mirror, which names the image to push
	p := newComposeProject(t, s, map[string]string{
		"compose.yaml": composeFile(t, `name: e2ecmd
services:`+composeDB+`
  w:
    image: "%ALPINE%"
    command: ["sh", "-c", "echo w one; echo w two; exec sleep 1d"]
    networks: [back]
    depends_on: [db]
  built:
    build: {context: ./b, args: {V: from-arg}}
    image: `+mirror.host+`/e2ecmd/built:1
    command: [sleep, 1d]
volumes: {data: {}}
networks: {back: {}}
`),
		"b/Dockerfile": "FROM " + mirrored(t, alpine) + "\nARG V\nRUN echo $V > /v\n",
	})
	if got := p.ok("config", "--services").stdout; got != "built\ndb\nw\n" {
		t.Errorf("config --services: %q", got)
	}
	if got := p.ok("config").stdout; !strings.HasPrefix(got, "name: e2ecmd\n") || !strings.Contains(got, "start_interval: 300ms") {
		t.Errorf("config:\n%s", got)
	}
	if r := p.ok("build"); !strings.Contains(r.stderr, " Image "+mirror.host+"/e2ecmd/built:1 Built \n") {
		t.Errorf("build:\n%s", r.stderr)
	}
	p.ok("up", "-d", "--wait")
	if got := p.ok("exec", "-T", "built", "cat", "/v").stdout; got != "from-arg\n" {
		t.Errorf("the built image: %q", got)
	}
	if got := p.ok("logs", "--no-color", "-n", "1", "w").stdout; got != "w-1  | w two\n" {
		t.Errorf("logs -n 1 w: %q", got)
	}
	if got := p.ok("logs", "--no-color", "w", "db").stdout; got != "db-1  | db starting\ndb-1  | db ready\nw-1   | w one\nw-1   | w two\n" {
		t.Errorf("logs w db: %q", got)
	}
	if r := p.ok("images"); !strings.Contains(r.stdout, "e2ecmd-db-1") {
		t.Errorf("images:\n%s", r.stdout)
	}

	// One-offs.
	r := p.run("run", "--rm", "-T", "w", "sh", "-c", "echo one-off; exit 7")
	if r.code != 7 || r.stdout != "one-off\n" || !regexp.MustCompile(` Container e2ecmd-w-run-[0-9a-f]{12} Created `).MatchString(r.stderr) {
		t.Errorf("run --rm: %+v", r)
	}
	id := strings.TrimSpace(p.ok("run", "-d", "w", "sleep", "60").stdout)
	var st struct{ Labels map[string]string }
	json.Unmarshal([]byte(s.ok("state", id).stdout), &st)
	if st.Labels["com.docker.compose.oneoff"] != "True" || len(st.Labels["com.docker.compose.slug"]) != 64 {
		t.Errorf("a one-off's labels: %v", st.Labels)
	}
	s.ok("delete", "-f", id)

	// Lifecycle.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"stop", "w"}, " Container e2ecmd-w-1 Stopped \n"},
		{[]string{"start", "w"}, " Container e2ecmd-w-1 Started \n"},
		{[]string{"restart", "w"}, " Container e2ecmd-w-1 Started \n"},
		{[]string{"pause", "w"}, " Container e2ecmd-w-1 Paused \n"},
		{[]string{"unpause", "w"}, " Container e2ecmd-w-1 Unpaused \n"},
		{[]string{"kill", "w"}, " Container e2ecmd-w-1 Killed \n"},
		{[]string{"kill", "w"}, "No container to kill\n"},
		{[]string{"rm", "-f", "w"}, " Container e2ecmd-w-1 Removed \n"},
	} {
		if r := p.ok(c.args...); !strings.Contains(r.stderr, c.want) {
			t.Errorf("%q: %+v", c.args, r)
		}
	}

	// Push to the test's registry.
	if r := p.ok("push", "built"); !strings.Contains(r.stdout, "1: digest: sha256:") {
		t.Errorf("push: %+v", r)
	}
	p.ok("pull", "db")

	// Without the file, by name.
	other := t.TempDir()
	cmd := exec.Command(runwhp(t), "compose", "-p", "e2ecmd", "ps", "--services")
	cmd.Dir, cmd.Env = other, append(os.Environ(), "RUNWHP_ROOT="+s.root)
	out, err := cmd.Output()
	if err != nil || string(out) != "built\ndb\n" {
		t.Errorf("ps -p without a file: %q, %v", out, err)
	}
	cmd = exec.Command(runwhp(t), "compose", "-p", "e2ecmd", "down", "-v")
	cmd.Dir, cmd.Env = other, append(os.Environ(), "RUNWHP_ROOT="+s.root)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), " Network e2ecmd_back Removed ") || !strings.Contains(string(out), " Volume e2ecmd_data Removed ") {
		t.Errorf("down -p without a file: %v\n%s", err, out)
	}
	if got := s.ok("list", "-q").stdout; got != "" {
		t.Errorf("left: %q", got)
	}
}

// TestComposeRefusals refuses what runwhp cannot do, before making
// anything.
func TestComposeRefusals(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{
		"refused.yaml": composeFile(t, `name: e2eref
services:
  host: {image: "%ALPINE%", network_mode: host}
  udp: {image: "%ALPINE%", ports: ["53:53/udp"]}
  ok: {image: "%ALPINE%", privileged: true}
`),
		"shared.yaml": composeFile(t, `name: e2eref
services:
  a: {image: "%ALPINE%", volumes: ["data:/d"]}
  b: {image: "%ALPINE%", volumes: ["data:/d"]}
volumes: {data: {}}
`),
		"external.yaml": composeFile(t, `name: e2eref
services:
  a: {image: "%ALPINE%", networks: [x]}
networks:
  x: {external: true, name: e2e-no-such-net}
`),
		"labelled.yaml": composeFile(t, `name: e2eref
services:
  a: {image: "%ALPINE%", command: [sleep, 1d]}
`),
	})
	r := p.run("-f", "refused.yaml", "up", "-d")
	for _, want := range []string{`runwhp: service "host": network_mode host is not supported`, `runwhp: service "udp": port 53/udp cannot be published`,
		`runwhp: compose: warning: service "ok": privileged is ignored`} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("no %q in:\n%s", want, r.stderr)
		}
	}
	if r := p.run("-f", "shared.yaml", "up", "-d"); r.code != 1 || !strings.Contains(r.stderr, `services "a" and "b" both mount volume e2eref_data`) {
		t.Errorf("a shared volume: %+v", r)
	}
	if r := p.run("-f", "external.yaml", "up", "-d"); r.code != 1 || !strings.Contains(r.stderr, "network e2e-no-such-net declared as external, but could not be found") {
		t.Errorf("a missing external network: %+v", r)
	}
	if got := s.ok("list", "-q").stdout + s.ok("network", "ls", "-q", "-f", "type=custom").stdout + s.ok("volume", "ls", "-q").stdout; got != "" {
		t.Errorf("made despite the refusals: %q", got)
	}
	s.ok("network", "create", "e2eref_default")
	if r := p.ok("-f", "labelled.yaml", "up", "-d"); !strings.Contains(r.stderr, "a network with name e2eref_default exists but was not created by compose") {
		t.Errorf("a network made outside compose: %+v", r)
	}
	if r := p.run("-f", "labelled.yaml", "up", "-d", "nosuch"); r.code != 1 || !strings.Contains(r.stderr, "no such service: nosuch") {
		t.Errorf("an unknown service: %+v", r)
	}
	empty := t.TempDir()
	cmd := exec.Command(runwhp(t), "compose", "up")
	cmd.Dir, cmd.Env = empty, append(os.Environ(), "RUNWHP_ROOT="+s.root)
	if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "no configuration file provided: not found") {
		t.Errorf("no file: %s", out)
	}
}
