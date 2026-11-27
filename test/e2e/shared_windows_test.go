//go:build windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/volume"
)

// lockerDir builds locker, the tests' POSIX lock and extended attribute
// tool, for Linux, once, and gives the directory it is in.
var lockerDir = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "runwhp-locker-")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", filepath.Join(dir, "locker"), "./locker")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building locker: %v\n%s", err, out)
	}
	return dir, nil
})

// locker is a bind mount of locker's directory at /t.
func locker(t *testing.T) string {
	t.Helper()
	dir, err := lockerDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir + ":/t:ro"
}

// sharedVolume makes a volume shared from the start, removed when the
// test ends.
func sharedVolume(t *testing.T) string {
	t.Helper()
	v := newVolume(t)
	if r := volumeCmd(t, nil, "", "create", "--opt", "shared=true", v); r.code != 0 {
		t.Fatalf("volume create: %+v", r)
	}
	return v
}

type volumeInfo struct {
	InUse      bool
	Shared     bool
	Containers []string
}

func inspectVolume(t *testing.T, v string) volumeInfo {
	t.Helper()
	var info []volumeInfo
	r := volumeCmd(t, nil, "", "inspect", v)
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil || len(info) != 1 {
		t.Fatalf("volume inspect %s: %+v", v, r)
	}
	return info[0]
}

// TestSharedVolume has two containers use a volume at once: each sees the
// other's files at once, appends from both are all kept, and flock(2) and
// POSIX locks, extended attributes, names that differ in case alone and
// owners are the volume's, the same for both.
func TestSharedVolume(t *testing.T) {
	v := sharedVolume(t)
	a := newName(t)
	detached(t, "--name", a, "-v", v+":/data", "-v", locker(t), alpine, "sh", "-c", "echo from-a > /data/a; sleep 300")
	waitFor(t, 15*time.Second, func() bool { return execIn(t, a, "test", "-e", "/data/a").code == 0 })
	expect(t, run(t, "-v", v+":/data", alpine, "sh", "-c", "cat /data/a; echo from-b > /data/b"), "from-a\n", 0)
	expect(t, execIn(t, a, "cat", "/data/b"), "from-b\n", 0)
	if info := inspectVolume(t, v); !info.Shared || !info.InUse || len(info.Containers) != 1 {
		t.Errorf("volume inspect with one container on it: %+v", info)
	}

	// Appends from both at once.
	const appends = "i=0; while [ $i -lt 200 ]; do echo $1$i >> /data/log; i=$((i+1)); done"
	var wg sync.WaitGroup
	var fromA result
	wg.Add(1)
	go func() { defer wg.Done(); fromA = execIn(t, a, "sh", "-c", appends, "sh", "a") }()
	fromB := run(t, "-v", v+":/data", alpine, "sh", "-c", appends, "sh", "b")
	wg.Wait()
	if fromA.code != 0 || fromB.code != 0 {
		t.Fatalf("appending: %+v %+v", fromA, fromB)
	}
	expect(t, execIn(t, a, "sh", "-c", "wc -l < /data/log; grep -c '^a' /data/log; grep -c '^b' /data/log"), "400\n200\n200\n", 0)

	// flock: a holds the lock; the other container cannot have it until a
	// lets go.
	// waitFile waits, in the container, up to 30 s for a file a holds the
	// lock by.
	const waitFile = "i=0; while [ ! -e $0 ]; do i=$((i+1)); [ $i -gt 300 ] && echo gave up && exit 9; sleep 0.1; done; "
	var held result
	wg.Add(1)
	go func() {
		defer wg.Done()
		held = execIn(t, a, "flock", "-x", "/data/lock", "sh", "-c", "touch /data/held; sleep 3")
	}()
	expect(t, run(t, "-v", v+":/data", alpine, "sh", "-c",
		waitFile+"flock -n /data/lock true && echo free || echo busy; flock -x /data/lock echo got", "/data/held"),
		"busy\ngot\n", 0)
	wg.Wait()
	if held.code != 0 {
		t.Errorf("flock in the first container: %+v", held)
	}
	// POSIX locks, as fcntl takes them, likewise.
	wg.Add(1)
	go func() { defer wg.Done(); held = execIn(t, a, "/t/locker", "posix", "/data/plock", "3", "/data/pheld") }()
	expect(t, run(t, "-v", v+":/data", "-v", locker(t), alpine, "sh", "-c",
		waitFile+"/t/locker try /data/plock; /t/locker posix /data/plock 0", "/data/pheld"),
		"busy\nlocked\n", 0)
	wg.Wait()
	if held.code != 0 || held.stdout != "locked\n" {
		t.Errorf("a POSIX lock in the first container: %+v", held)
	}

	// Extended attributes, names differing in case, and owners, in a
	// directory anyone may write, as the volume's root is root's; and a
	// user other than its owner cannot write there.
	expect(t, execIn(t, a, "sh", "-c", "mkdir -m 1777 /data/pub; touch /data/rootonly"), "", 0)
	expect(t, run(t, "-v", v+":/data", "-v", locker(t), "-u", "1000:1000", alpine, "sh", "-c",
		"touch /data/pub/Case /data/pub/case /data/pub/mine; /t/locker setx /data/pub/mine user.e2e v1; echo x >> /data/rootonly || echo refused"),
		"refused\n", 0)
	expect(t, execIn(t, a, "sh", "-c", "ls /data/pub | grep -ic '^case$'; stat -c '%u:%g' /data/pub/mine; /t/locker getx /data/pub/mine user.e2e"),
		"2\n1000:1000\nv1\n", 0)
}

// TestSharedVolumeAutomatic: a container needing a volume a running one
// has is refused, and the volume becomes shared, so that once the holder
// starts again, both use it at once.
func TestSharedVolumeAutomatic(t *testing.T) {
	v := newVolume(t)
	a := newName(t)
	detached(t, "--name", a, "-v", v+":/data", alpine, "sh", "-c", "echo from-a > /data/a; sleep 300")
	r := run(t, "-v", v+":/data", alpine, "cat", "/data/a")
	if r.code != 125 || !strings.Contains(r.stderr, "volume "+v+" is in use by container "+a) || !strings.Contains(r.stderr, "shared from now on") {
		t.Fatalf("a second container while the first runs: %+v", r)
	}
	if info := inspectVolume(t, v); !info.Shared {
		t.Fatalf("the volume is not shared after the refusal: %+v", info)
	}
	cli(t, "restart", "-t", "0", a)
	waitStatus(t, a, "running", 15*time.Second)
	expect(t, run(t, "-v", v+":/data", alpine, "cat", "/data/a"), "from-a\n", 0)
}

// TestSharedVolumeCreated: containers made one after another on a volume,
// as Docker Compose makes a project's, then started, share it: making the
// second boots the first again, onto the volume's server.
func TestSharedVolumeCreated(t *testing.T) {
	v := newVolume(t)
	a, b := newName(t), newName(t)
	created(t, "--name", a, "-v", v+":/data", alpine, "sh", "-c", "echo from-a > /data/a; sleep 300")
	created(t, "--name", b, "-v", v+":/data", alpine, "sh", "-c", "while [ ! -e /data/a ]; do sleep 0.1; done; cat /data/a; sleep 300")
	if info := inspectVolume(t, v); !info.Shared {
		t.Fatalf("the volume is not shared once two containers were made on it: %+v", info)
	}
	if r := cli(t, "start", a, b); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitLog(t, b, "from-a\n")
}

// TestSharedVolumeServer: a shared volume's server stops once no container
// has used it for its grace; volume rm stops an idle one at once; and a
// container stopped with its files reached by cp is a client too.
func TestSharedVolumeServer(t *testing.T) {
	v := sharedVolume(t)
	vol, err := volume.Open(storeDir(t)).Get(v)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(runwhp(t), runArgs("-v", v+":/data", alpine, "sh", "-c", "echo x > /data/f")...)
	cmd.Env = append(os.Environ(), "RUNWHP_VOLUME_GRACE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	waitFor(t, 15*time.Second, func() bool { _, err := vol.ServerStatus(); return err != nil })

	// cp into a stopped container's volume while another runs on it.
	a, b := newName(t), newName(t)
	detached(t, "--name", a, "-v", v+":/data", alpine, "sleep", "300")
	if r := cli(t, append([]string{"run", "--name", b}, withFlags([]string{"-v", v + ":/data", alpine, "true"})...)...); r.code != 0 {
		t.Fatalf("run %s: %+v", b, r)
	}
	src := filepath.Join(t.TempDir(), "copied")
	os.WriteFile(src, []byte("copied in\n"), 0o644)
	if r := cli(t, "cp", src, b+":/data/copied"); r.code != 0 {
		t.Fatalf("cp into a stopped container's shared volume: %+v", r)
	}
	expect(t, execIn(t, a, "cat", "/data/copied"), "copied in\n", 0)
	cli(t, "delete", "-f", a, b)
	if !vol.StopIdleServer() {
		// Its last client may still be going.
		time.Sleep(time.Second)
		vol.StopIdleServer()
	}
	if _, err := vol.ServerStatus(); err == nil {
		t.Error("the server serves on after being stopped")
	}
}

// TestSharedVolumeServerKilled: a server that dies takes its containers'
// volume with it, which they get I/O errors from; a container started
// again starts a new server, and what was synced is there.
func TestSharedVolumeServerKilled(t *testing.T) {
	v := sharedVolume(t)
	a := newName(t)
	detached(t, "--name", a, "-v", v+":/data", alpine, "sh", "-c", "echo kept > /data/kept; sync; sleep 300")
	waitFor(t, 15*time.Second, func() bool { return execIn(t, a, "test", "-e", "/data/kept").code == 0 })
	vol, err := volume.Open(storeDir(t)).Get(v)
	if err != nil {
		t.Fatal(err)
	}
	st, err := vol.ServerStatus()
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		t.Fatal(err)
	}
	p.Kill()
	p.Wait()
	if r := execIn(t, a, "sh", "-c", "echo lost > /data/lost"); r.code == 0 {
		t.Errorf("a write after the server died succeeded: %+v", r)
	}
	cli(t, "restart", "-t", "0", a)
	waitStatus(t, a, "running", 15*time.Second)
	expect(t, execIn(t, a, "cat", "/data/kept"), "kept\n", 0)
}

// TestComposeSharedVolumes: runwhp compose runs services that mount one
// volume, one of them with replicas, all at once, and scales up a service
// alone on its volume, starting its container again to share it.
func TestComposeSharedVolumes(t *testing.T) {
	s := newOwnStore(t)
	p := newComposeProject(t, s, map[string]string{
		"compose.yaml": composeFile(t, `name: e2eshare
services:
  writer:
    image: "%ALPINE%"
    command: ["sh", "-c", "echo $$HOSTNAME >> /data/log; sleep 1d"]
    volumes: ["shared:/data"]
  reader:
    image: "%ALPINE%"
    command: ["sh", "-c", "echo $$HOSTNAME >> /data/log; sleep 1d"]
    volumes: ["shared:/data"]
    deploy: {replicas: 2}
  alone:
    image: "%ALPINE%"
    command: ["sh", "-c", "echo $$HOSTNAME >> /data/log; sleep 1d"]
    volumes: ["solo:/data"]
volumes: {shared: {}, solo: {}}
`),
	})
	p.ok("up", "-d")
	waitFor(t, 15*time.Second, func() bool {
		return strings.Count(p.run("exec", "writer", "cat", "/data/log").stdout, "\n") == 3
	})
	ls := s.ok("volume", "ls").stdout
	if !regexpMatch(`e2eshare_shared\s+\S+\s+\S+\s+true`, ls) || !regexpMatch(`e2eshare_solo\s+\S+\s+\S+\s+false`, ls) {
		t.Errorf("volume ls:\n%s", ls)
	}
	r := p.ok("up", "-d", "--scale", "alone=2")
	if !strings.Contains(r.stderr, "Container e2eshare-alone-1 Restarting") {
		t.Errorf("scaling up a service alone on its volume:\n%s", r.stderr)
	}
	waitFor(t, 15*time.Second, func() bool {
		return strings.Count(p.run("exec", "alone", "cat", "/data/log").stdout, "\n") == 3
	})
	p.ok("down", "-v", "-t", "0")
	if got := s.ok("volume", "ls", "-q").stdout; got != "" {
		t.Errorf("left after down -v: %q", got)
	}
}

func regexpMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }
