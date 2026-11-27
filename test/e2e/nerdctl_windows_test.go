//go:build windows

package e2e

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// The nerdctl tests run Linux containers through containerd and runwhp's
// shim, as a user would: runwhp containerd, then stock nerdctl. They need
// nerdctl.exe and containerd.exe, from RUNWHP_E2E_CONTAINERD_DIR or PATH,
// and the shim, built by build.ps1 beside runwhp.exe.

// nerdctlRun is what every nerdctl run of these tests needs.
var nerdctlRun = []string{"--runtime", "io.containerd.runwhp.v1", "--platform", "linux/amd64", "--network", "none"}

// nerdctlEnv is one runwhp containerd of a test's own, and nerdctl's
// configuration for it.
type nerdctlEnv struct {
	t        *testing.T
	nerdctl  string
	dir      string // runwhp containerd's directory
	address  string
	toml     string
	launcher *exec.Cmd
	exited   chan struct{}
	log      *syncBuffer // the running launcher's output
	history  string      // earlier launchers'
}

// findTool finds name in RUNWHP_E2E_CONTAINERD_DIR, else on PATH.
func findTool(name string) string {
	if dir := os.Getenv("RUNWHP_E2E_CONTAINERD_DIR"); dir != "" {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return ""
	}
	p, _ := exec.LookPath(name)
	return p
}

func newNerdctl(t *testing.T) *nerdctlEnv {
	t.Helper()
	exe := runwhp(t)
	nerdctl, containerd := findTool("nerdctl.exe"), findTool("containerd.exe")
	if nerdctl == "" || containerd == "" {
		t.Skip("nerdctl.exe and containerd.exe are not in RUNWHP_E2E_CONTAINERD_DIR or on PATH")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "containerd-shim-runwhp-v1.exe")); err != nil {
		t.Fatalf("%v; run build.ps1", err)
	}
	e := &nerdctlEnv{t: t, nerdctl: nerdctl, dir: t.TempDir(), address: "runwhp-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		exited: make(chan struct{}), log: &syncBuffer{}}
	e.toml = filepath.Join(t.TempDir(), "nerdctl.toml")
	conf := fmt.Sprintf("address = '\\\\.\\pipe\\%s'\ndata_root = '%s'\nsnapshotter = 'runwhp'\nhost_gateway_ip = '10.0.2.2'\n", e.address, filepath.Join(e.dir, "nerdctl"))
	if err := os.WriteFile(e.toml, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	e.start(containerd)
	t.Cleanup(func() {
		// Shims outlive containerd: remove what the test left first.
		for _, id := range strings.Fields(e.cli("", "ps", "-aq").stdout) {
			e.cli("", "rm", "-f", id)
		}
		e.stop()
		// A shim nerdctl could not reach, its containerd gone, is ended
		// through its container's record, which holds its pid.
		states, _ := filepath.Glob(filepath.Join(e.dir, "store", "containers", "*", "state.json"))
		for _, path := range states {
			var st struct {
				Pid int `json:"pid"`
			}
			if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &st) == nil && st.Pid > 0 {
				if p, err := os.FindProcess(st.Pid); err == nil {
					t.Errorf("a shim (pid %d) was still running a container; ending it", st.Pid)
					p.Kill()
				}
			}
		}
		if t.Failed() {
			t.Logf("runwhp containerd:\n%s", e.log.String())
			if data, err := os.ReadFile(filepath.Join(e.dir, "containerd.log")); err == nil {
				t.Logf("containerd.log:\n%s", data)
			}
		}
	})
	return e
}

// start starts runwhp containerd, and waits until containerd serves.
func (e *nerdctlEnv) start(containerd string) {
	e.t.Helper()
	e.exited = make(chan struct{})
	if e.log.String() != "" {
		e.history += e.log.String()
	}
	e.log = &syncBuffer{}
	cmd := command(e.t, "containerd", "--address", e.address, "--dir", e.dir, "--containerd", containerd)
	cmd.Stdout, cmd.Stderr = e.log, e.log
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.launcher = cmd
	go func() { cmd.Wait(); close(e.exited) }()
	timeout := 200 * time.Millisecond
	for deadline := time.Now().Add(60 * time.Second); ; {
		if strings.Contains(e.log.String(), "containerd serves") {
			if c, err := winio.DialPipe(`\\.\pipe\`+e.address, &timeout); err == nil {
				c.Close()
				return
			}
		}
		select {
		case <-e.exited:
			e.t.Fatalf("runwhp containerd exited:\n%s", e.log.String())
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("runwhp containerd did not serve within 60 s:\n%s", e.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop ends runwhp containerd, and the containerd it runs with it.
func (e *nerdctlEnv) stop() {
	select {
	case <-e.exited:
		return
	default:
	}
	e.launcher.Process.Kill()
	<-e.exited
	// containerd goes with the launcher's job, a moment later.
	timeout := 100 * time.Millisecond
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		c, err := winio.DialPipe(`\\.\pipe\`+e.address, &timeout)
		if err != nil {
			return
		}
		c.Close()
	}
	e.t.Errorf("containerd still serves %s 10 s after runwhp containerd ended", e.address)
}

// cli runs nerdctl with args and stdin.
func (e *nerdctlEnv) cli(stdin string, args ...string) result {
	e.t.Helper()
	cmd := exec.Command(e.nerdctl, args...)
	cmd.Env = append(os.Environ(), "NERDCTL_TOML="+e.toml)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// ok runs nerdctl and fails the test unless it exits 0.
func (e *nerdctlEnv) ok(args ...string) result {
	e.t.Helper()
	r := e.cli("", args...)
	if r.code != 0 {
		e.t.Fatalf("nerdctl %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// run runs nerdctl run with the flags runwhp needs, then args.
func (e *nerdctlEnv) run(stdin string, args ...string) result {
	e.t.Helper()
	return e.cli(stdin, append(append([]string{"run"}, nerdctlRun...), args...)...)
}

// pull pulls ref from the mirror into the test's containerd, and returns
// its reference there.
func (e *nerdctlEnv) pull(ref string) string {
	e.t.Helper()
	there := mirrored(e.t, ref)
	e.ok("pull", "--insecure-registry", "--platform", "linux/amd64", "-q", there)
	return there
}

func TestNerdctlRun(t *testing.T) {
	e := newNerdctl(t)
	img := e.pull(alpine)
	expect(t, e.run("", "--rm", img, "echo", "hello"), "hello\n", 0)
	expect(t, e.run("", "--rm", img, "sh", "-c", "exit 3"), "", 3)
	expect(t, e.run("piped\n", "--rm", "-i", img, "sh", "-c", "read x; echo got $x"), "got piped\n", 0)
	expect(t, e.run("", "--rm", "-e", "FOO=bar", "-w", "/tmp", "-u", "nobody", "--hostname", "h1", img,
		"sh", "-c", "echo $FOO $(pwd) $(id -un) $(hostname)"), "bar /tmp nobody h1\n", 0)
	expect(t, e.run("", "--rm", "--cpus", "2", img, "nproc"), "2\n", 0)
	r := e.run("", "--rm", "--memory", "512m", img, "sh", "-c", "grep MemTotal /proc/meminfo")
	var kb int
	fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(r.stdout, "MemTotal:")), "%d", &kb)
	if r.code != 0 || kb < 400<<10 || kb > 512<<10 {
		t.Errorf("--memory 512m: MemTotal %d kB, %+v", kb, r)
	}
	// The default hostname is the container's ID's first 12 digits.
	r = e.run("", "--name", "named", img, "hostname")
	if id := strings.TrimSpace(e.ok("inspect", "--format", "{{.ID}}", "named").stdout); r.stdout != id[:12]+"\n" {
		t.Errorf("hostname %q, ID %q", r.stdout, id)
	}
	// A spec from another snapshotter is refused, saying which to use.
	r = e.cli("", append(append([]string{"run", "--rm", "--snapshotter", "native"}, nerdctlRun...), img, "true")...)
	if r.code == 0 {
		t.Errorf("another snapshotter's container ran: %+v", r)
	}
}

func TestNerdctlDetached(t *testing.T) {
	e := newNerdctl(t)
	img := e.pull(alpine)
	e.ok(append(append([]string{"run", "-d", "--name", "d1"}, nerdctlRun...), img,
		"sh", "-c", "echo started >> /runs; i=0; while true; do echo tick $i; i=$((i+1)); echo $i > /count; sleep 0.1; done")...)
	waitFor(t, 20*time.Second, func() bool { return strings.Contains(e.cli("", "logs", "d1").stdout, "tick 3") })
	if !strings.Contains(e.ok("ps").stdout, "d1") {
		t.Error("ps does not show d1")
	}

	expect(t, e.cli("", "exec", "d1", "echo", "in d1"), "in d1\n", 0)
	expect(t, e.cli("input", "exec", "-i", "d1", "cat"), "input", 0)
	if r := e.cli("", "exec", "d1", "sh", "-c", "exit 7"); r.code == 0 || !strings.Contains(r.stderr, "exit code 7") {
		t.Errorf("exec exiting 7: %+v", r)
	}

	// Paused, the count stands still, and exec is refused.
	e.ok("pause", "d1")
	before := e.ok("inspect", "--format", "{{.State.Status}}", "d1").stdout
	if strings.TrimSpace(before) != "paused" {
		t.Errorf("status while paused: %q", before)
	}
	if r := e.cli("", "exec", "d1", "true"); r.code == 0 || !strings.Contains(r.stderr, "paused") {
		t.Errorf("exec in a paused container: %+v", r)
	}
	e.ok("unpause", "d1")
	count := func() int {
		n, _ := strconv.Atoi(strings.TrimSpace(e.ok("exec", "d1", "cat", "/count").stdout))
		return n
	}
	c1 := count()
	waitFor(t, 10*time.Second, func() bool { return count() > c1 })

	// Stopped and started again, it keeps what it wrote.
	start := time.Now()
	e.ok("stop", "-t", "5", "d1")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stop took %v: SIGTERM did not end it", d)
	}
	if st := strings.TrimSpace(e.ok("inspect", "--format", "{{.State.Status}}", "d1").stdout); st != "exited" {
		t.Errorf("status after stop: %q", st)
	}
	e.ok("start", "d1")
	waitFor(t, 20*time.Second, func() bool {
		return e.cli("", "exec", "d1", "sh", "-c", "wc -l < /runs").stdout == "2\n"
	})
	e.ok("kill", "-s", "KILL", "d1")
	expect(t, e.cli("", "wait", "d1"), "137\n", 0)
	e.ok("rm", "d1")
	if r := e.cli("", "inspect", "d1"); r.code == 0 {
		t.Error("d1 is still there after rm")
	}
	// The shim's store keeps nothing of it.
	if left, _ := filepath.Glob(filepath.Join(e.dir, "store", "containers", "*")); len(left) != 0 {
		t.Errorf("the shim's store still holds %q", left)
	}
}

func TestNerdctlNetwork(t *testing.T) {
	e := newNerdctl(t)
	img := e.pull("busybox:1.36")
	port := freePort(t)
	e.ok(append(append([]string{"run", "-d", "--name", "web", "-p", "127.0.0.1:" + port + ":80"}, nerdctlRun...), img,
		"httpd", "-f", "-p", "80", "-h", "/etc")...)
	var body string
	waitFor(t, 20*time.Second, func() bool {
		resp, err := http.Get("http://127.0.0.1:" + port + "/hostname")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == 200
	})
	if id := strings.TrimSpace(e.ok("inspect", "--format", "{{.ID}}", "web").stdout); body != id[:12]+"\n" {
		t.Errorf("published port served %q", body)
	}

	// The container reaches the host through host-gateway.
	mirrorPort := strings.TrimPrefix(mirror.host, "localhost:")
	r := e.run("", "--rm", "--add-host", "reg:host-gateway", img, "wget", "-q", "-O-", "http://reg:"+mirrorPort+"/v2/")
	if r.code != 0 {
		t.Errorf("reaching the host: %+v", r)
	}

	// io.runwhp.network=none leaves it loopback alone.
	r = e.run("", "--rm", "--annotation", "io.runwhp.network=none", img, "ls", "/sys/class/net")
	if r.code != 0 || strings.Fields(r.stdout)[0] != "lo" || len(strings.Fields(r.stdout)) != 1 {
		t.Errorf("network none: %+v", r)
	}
}

func TestNerdctlMounts(t *testing.T) {
	e := newNerdctl(t)
	img := e.pull(alpine)
	host := t.TempDir()
	os.WriteFile(filepath.Join(host, "from-host"), []byte("host\n"), 0o644)
	r := e.run("", "--rm", "-v", host+`:C:\data`, "-v", host+`:C:\ro:ro`, img,
		"sh", "-c", "cat /data/from-host; echo guest > /data/from-guest; touch /ro/x")
	if r.stdout != "host\n" || r.code == 0 || !strings.Contains(r.stderr, "Read-only") {
		t.Errorf("bind mounts: %+v", r)
	}
	if data, _ := os.ReadFile(filepath.Join(host, "from-guest")); string(data) != "guest\n" {
		t.Errorf("the host has %q", data)
	}

	// A named volume outlives the container.
	expect(t, e.run("", "--rm", "-v", `vol1:C:\vol`, img, "sh", "-c", "echo kept > /vol/f"), "", 0)
	expect(t, e.run("", "--rm", "-v", `vol1:C:\vol`, img, "cat", "/vol/f"), "kept\n", 0)

	// An image's VOLUME gets the image's files, and so does a new named
	// volume mounted there instead.
	seeded := e.pushVolumeImage(t)
	expect(t, e.run("", "--rm", seeded, "cat", "/data/seed"), "seed\n", 0)
	expect(t, e.run("", "--rm", "-v", `vol2:C:\data`, seeded, "sh", "-c", "cat /data/seed; echo mine > /data/mine"), "seed\n", 0)
	expect(t, e.run("", "--rm", "-v", `vol2:C:\data`, img, "cat", "/data/mine"), "mine\n", 0)
}

// pushVolumeImage pushes alpine with /data/seed in a layer of its own and
// VOLUME /data to the mirror, and pulls it into the test's containerd.
func (e *nerdctlEnv) pushVolumeImage(t *testing.T) string {
	t.Helper()
	base, err := remote.Image(parseRef(t, mirrored(t, alpine)), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "data/seed", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
	io.WriteString(tw, "seed\n")
	tw.Close()
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, l)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg := cf.Config.DeepCopy()
	cfg.Volumes = map[string]struct{}{"/data": {}}
	if img, err = mutate.Config(img, *cfg); err != nil {
		t.Fatal(err)
	}
	ref := mirror.host + "/e2e/seeded:latest"
	if err := remote.Write(parseRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	e.ok("pull", "--insecure-registry", "--platform", "linux/amd64", "-q", ref)
	return ref
}

func TestNerdctlContainerdRestart(t *testing.T) {
	e := newNerdctl(t)
	img := e.pull(alpine)
	e.ok(append(append([]string{"run", "-d", "--name", "survivor"}, nerdctlRun...), img,
		"sh", "-c", "while true; do date +%s > /now; sleep 0.2; done")...)
	waitFor(t, 20*time.Second, func() bool { return e.cli("", "exec", "survivor", "cat", "/now").code == 0 })

	// containerd goes with runwhp containerd; the container does not.
	e.stop()
	containerd := findTool("containerd.exe")
	e.start(containerd)
	if !strings.Contains(e.ok("ps").stdout, "survivor") {
		t.Fatal("ps does not show the container after containerd started again")
	}
	r := e.ok("exec", "survivor", "sh", "-c", "echo $(( $(date +%s) - $(cat /now) ))")
	if n, _ := strconv.Atoi(strings.TrimSpace(r.stdout)); n > 2 {
		t.Errorf("the container stopped counting %ds ago", n)
	}
	e.ok("stop", "-t", "2", "survivor")
	if st := strings.TrimSpace(e.ok("inspect", "--format", "{{.State.Status}}", "survivor").stdout); st != "exited" {
		t.Errorf("status after stop: %q", st)
	}
}

// waitFor polls cond until it holds, or fails the test after d.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("still waiting after %v", d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
