//go:build windows

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const busybox = "busybox:1.36"

// ownStore is a store of the test's own, so that removing images and
// pruning leave the user's alone. The kernels in the user's store are
// copied in, so that only images are fetched.
type ownStore struct {
	t    *testing.T
	root string
}

func newOwnStore(t *testing.T) *ownStore {
	t.Helper()
	runwhp(t)
	s := &ownStore{t, t.TempDir()}
	kernels, _ := filepath.Glob(filepath.Join(storeDir(t), "kernels", "sha256", "*", "bzImage"))
	for _, k := range kernels {
		rel, _ := filepath.Rel(storeDir(t), k)
		data, err := os.ReadFile(k)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(filepath.Join(s.root, rel)), 0o755)
		if err := os.WriteFile(filepath.Join(s.root, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Containers left running would hold the store's files.
	t.Cleanup(func() {
		for _, id := range strings.Fields(s.cli("", "list", "-q").stdout) {
			s.cli("", "delete", "-f", id)
		}
	})
	return s
}

// cli runs runwhp on the store with exactly args, with stdin.
func (s *ownStore) cli(stdin string, args ...string) result {
	s.t.Helper()
	cmd := exec.Command(runwhp(s.t), args...)
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+s.root)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// ok runs runwhp on the store and fails the test unless it exits 0.
func (s *ownStore) ok(args ...string) result {
	s.t.Helper()
	r := s.cli("", args...)
	if r.code != 0 {
		s.t.Fatalf("runwhp %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// layers lists the layer files in the store.
func (s *ownStore) layers() []string {
	found, _ := filepath.Glob(filepath.Join(s.root, "layers", "sha256", "*.ext4"))
	for i, p := range found {
		found[i] = "sha256:" + strings.TrimSuffix(filepath.Base(p), ".ext4")
	}
	return found
}

// configDigest asks the registry for an image's configuration digest, which
// is its ID.
func configDigest(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(r, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	d, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

// TestImages lists and inspects pulled images.
func TestImages(t *testing.T) {
	s := newOwnStore(t)
	s.ok("pull", alpine)
	s.ok("pull", busybox)
	alpineID, busyboxID := configDigest(t, alpine), configDigest(t, busybox)

	r := s.ok("images")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "REPOSITORY") {
		t.Fatalf("images:\n%s", r.stdout)
	}
	for _, want := range [][]string{{"alpine", "3.20", alpineID[7:19]}, {"busybox", "1.36", busyboxID[7:19]}} {
		found := false
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			found = found || len(f) > 3 && f[0] == want[0] && f[1] == want[1] && f[2] == want[2]
		}
		if !found {
			t.Errorf("images lists no %v:\n%s", want, r.stdout)
		}
	}
	if r := s.ok("images", "-q"); strings.Count(r.stdout, "\n") != 2 {
		t.Errorf("images -q: %q", r.stdout)
	}
	if r := s.ok("image", "ls", "-q", "busybox"); r.stdout != busyboxID[7:19]+"\n" {
		t.Errorf("image ls -q busybox: %q", r.stdout)
	}

	var rows []struct {
		ID, Repository, Tag string
		Size                int64
	}
	if err := json.Unmarshal([]byte(s.ok("images", "--format", "json").stdout), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("images --format json: %v, %+v", err, rows)
	}
	for _, row := range rows {
		if row.Size < 1<<20 || row.ID != map[string]string{"alpine": alpineID, "busybox": busyboxID}[row.Repository] {
			t.Errorf("json row %+v", row)
		}
	}

	var infos []struct {
		Id       string
		RepoTags []string
		Os       string
		Config   struct{ Cmd []string }
	}
	if err := json.Unmarshal([]byte(s.ok("image", "inspect", busybox, alpineID[7:15]).stdout), &infos); err != nil || len(infos) != 2 {
		t.Fatalf("image inspect: %v", err)
	}
	if i := infos[0]; i.Id != busyboxID || len(i.RepoTags) != 1 || i.RepoTags[0] != busybox || i.Os != "linux" || len(i.Config.Cmd) == 0 {
		t.Errorf("inspect busybox: %+v", i)
	}
	if infos[1].Id != alpineID {
		t.Errorf("inspect by ID prefix: %+v", infos[1])
	}
}

// TestRmi removes images, an image a container was made from with -f, and
// that container's layers once it is gone.
func TestRmi(t *testing.T) {
	s := newOwnStore(t)
	s.ok("pull", alpine)
	s.ok("pull", busybox)
	before := len(s.layers())

	r := s.ok("rmi", busybox)
	if !strings.HasPrefix(r.stdout, "Untagged: "+busybox+"\nDeleted: sha256:") || len(s.layers()) != before-1 {
		t.Errorf("rmi %s: %q; %d layers, before %d", busybox, r.stdout, len(s.layers()), before)
	}
	if r := s.cli("", runArgs("--pull", "never", busybox, "true")...); r.code != 125 {
		t.Errorf("run of a removed image: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := s.cli("", "rmi", busybox); r.code != 1 || !strings.Contains(r.stderr, "no such image") {
		t.Errorf("rmi again: exit %d, stderr %q", r.code, r.stderr)
	}

	s.ok(keepArgs("--name", "user", alpine, "echo", "hi")...)
	if r := s.cli("", "rmi", alpine); r.code != 1 || !strings.Contains(r.stderr, "used by container user") {
		t.Errorf("rmi of an image a container uses: exit %d, stderr %q", r.code, r.stderr)
	}
	// With -f, the record goes; the container keeps its layers, and starts.
	if r := s.ok("rmi", "-f", alpine); r.stdout != "Untagged: "+alpine+"\n" || len(s.layers()) != before-1 {
		t.Errorf("rmi -f: %q; %d layers", r.stdout, len(s.layers()))
	}
	if r := s.ok("start", "-a", "user"); r.stdout != "hi\n" {
		t.Errorf("start -a once its image is gone: %q", r.stdout)
	}
	if r := s.ok("image", "prune", "-f"); r.stdout != "Total reclaimed space: 0B\n" {
		t.Errorf("prune with the container there: %q", r.stdout)
	}
	s.ok("delete", "user")
	if r := s.ok("image", "prune", "-f"); !strings.HasPrefix(r.stdout, "Deleted Images:\ndeleted: sha256:") || len(s.layers()) != 0 {
		t.Errorf("prune once the container is gone: %q; layers %v", r.stdout, s.layers())
	}
}

// TestImagePrune removes the images no container uses, and keeps the one a
// running container uses.
func TestImagePrune(t *testing.T) {
	s := newOwnStore(t)
	s.ok("pull", alpine)
	s.ok("pull", busybox)
	id := strings.TrimSpace(s.ok(append([]string{"run", "-d"}, withFlags([]string{alpine, "sleep", "60"})...)...).stdout)

	if r := s.cli("n\n", "image", "prune", "-a"); r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "Are you sure") {
		t.Errorf("prune declined: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	if r := s.ok("images", "-q"); strings.Count(r.stdout, "\n") != 2 {
		t.Errorf("images after declining: %q", r.stdout)
	}
	r := s.cli("y\n", "image", "prune", "-a")
	if r.code != 0 || !strings.Contains(r.stdout, "untagged: "+busybox+"\ndeleted: sha256:") || strings.Contains(r.stdout, alpine) {
		t.Errorf("prune -a: exit %d, %q", r.code, r.stdout)
	}
	if r := s.ok("images"); !strings.Contains(r.stdout, "alpine") || strings.Contains(r.stdout, "busybox") {
		t.Errorf("images after prune -a:\n%s", r.stdout)
	}
	if r := s.ok("exec", id, "echo", "alive"); r.stdout != "alive\n" {
		t.Errorf("the running container: %q", r.stdout)
	}
}

// TestPruneRace prunes over and over while an image is pulled and while
// containers are run: the lock keeps a prune from removing layers a pull
// has stored but not yet recorded, or that a run has resolved but not yet
// recorded its container with.
func TestPruneRace(t *testing.T) {
	s := newOwnStore(t)
	prune := func(stop chan struct{}, args ...string) *sync.WaitGroup {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if r := s.cli("", args...); r.code != 0 {
					t.Errorf("runwhp %s: exit %d, %s", strings.Join(args, " "), r.code, r.stderr)
				}
			}
		}()
		return &wg
	}

	// Layers nothing lists yet dangle, until the pull records its image.
	stop := make(chan struct{})
	wg := prune(stop, "image", "prune", "-f")
	r := s.cli("", "pull", "python:3.12-slim")
	close(stop)
	wg.Wait()
	if r.code != 0 {
		t.Fatalf("pull: exit %d, %s", r.code, r.stderr)
	}
	if r := s.cli("", runArgs("--pull", "never", "python:3.12-slim", "python3", "-c", "print(1)")...); r.code != 0 || r.stdout != "1\n" {
		t.Errorf("the pulled image: exit %d, %q, %s", r.code, r.stdout, r.stderr)
	}

	// An image no container uses yet is unused, until the run records its
	// container.
	stop = make(chan struct{})
	wg = prune(stop, "image", "prune", "-a", "-f")
	var runs sync.WaitGroup
	for range 4 {
		runs.Add(1)
		go func() {
			defer runs.Done()
			for range 3 {
				if r := s.cli("", runArgs("--pull", "always", alpine, "true")...); r.code != 0 {
					t.Errorf("run: exit %d, %s", r.code, r.stderr)
				}
			}
		}()
	}
	runs.Wait()
	close(stop)
	wg.Wait()
}

// TestContainerPrune removes a stopped container and a created one, whose
// VM is stopped, and keeps a running one.
func TestContainerPrune(t *testing.T) {
	s := newOwnStore(t)
	id := func(args ...string) string { return strings.TrimSpace(s.ok(args...).stdout) }
	s.ok(keepArgs("--name", "stopped", alpine, "true")...)
	stopped := id("list", "-q")
	created := id(append([]string{"create", "--name", "created"}, withFlags([]string{alpine, "sleep", "60"})...)...)
	running := id(append([]string{"run", "-d", "--name", "running"}, withFlags([]string{alpine, "sleep", "60"})...)...)
	var st struct{ Pid int }
	json.Unmarshal([]byte(s.ok("state", created).stdout), &st)
	if st.Pid == 0 {
		t.Fatal("the created container has no runwhp")
	}

	if r := s.cli("n\n", "container", "prune"); r.code != 0 || r.stdout != "" || len(strings.Fields(s.ok("list", "-q").stdout)) != 3 {
		t.Errorf("declined: exit %d, %q", r.code, r.stdout)
	}
	r := s.ok("container", "prune", "-f")
	if !strings.HasPrefix(r.stdout, "Deleted Containers:\n") || !strings.Contains(r.stdout, stopped+"\n") || !strings.Contains(r.stdout, created+"\n") ||
		strings.Contains(r.stdout, running) || strings.Contains(r.stdout, "Total reclaimed space: 0B") {
		t.Errorf("prune -f:\n%s", r.stdout)
	}
	if left := strings.Fields(s.ok("list", "-q").stdout); len(left) != 1 || left[0] != running {
		t.Errorf("left: %v", left)
	}
	if r := s.ok("exec", "running", "echo", "alive"); r.stdout != "alive\n" {
		t.Errorf("the running container: %q", r.stdout)
	}
	// The created container's runwhp is gone with it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", st.Pid), "/NH").Output()
		if !strings.Contains(string(out), "runwhp") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the created container's runwhp, %d, is still running", st.Pid)
		}
	}
}

// TestSystemDf counts a container's writable layer and its image.
func TestSystemDf(t *testing.T) {
	s := newOwnStore(t)
	s.ok(keepArgs("--name", "big", alpine, "sh", "-c", "head -c 52428800 /dev/urandom > /big && sync")...)
	r := s.ok("system", "df")
	var containers []string
	for _, l := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(l, "Containers") {
			containers = strings.Fields(l)
		}
	}
	if len(containers) < 5 || containers[1] != "1" || containers[2] != "0" || !strings.HasSuffix(containers[3], "MiB") {
		t.Errorf("system df:\n%s", r.stdout)
	}
	if r := s.ok("system", "df", "-v"); !strings.Contains(r.stdout, "Containers space usage:") || !hasRow(r.stdout, "alpine", "3.20", "1") {
		t.Errorf("system df -v:\n%s", r.stdout)
	}
	var js struct {
		Containers []struct {
			Name string
			Size int64
		}
		Images []struct{ Containers int }
	}
	if err := json.Unmarshal([]byte(s.ok("system", "df", "--format", "json").stdout), &js); err != nil ||
		len(js.Containers) != 1 || js.Containers[0].Size < 50<<20 || len(js.Images) != 1 || js.Images[0].Containers != 1 {
		t.Errorf("system df --format json: %v, %+v", err, js)
	}
}

// hasRow reports whether a line of out starts with repo and tag and ends
// with last.
func hasRow(out, repo, tag, last string) bool {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 3 && f[0] == repo && f[1] == tag && f[len(f)-1] == last {
			return true
		}
	}
	return false
}

// TestSystemPrune removes a stopped container, an image no container uses,
// an old kernel and a volume no container uses, and keeps a running
// container with its image and volume.
func TestSystemPrune(t *testing.T) {
	s := newOwnStore(t)
	old := filepath.Join(s.root, "kernels", "sha256", strings.Repeat("a", 64))
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "bzImage"), make([]byte, 1<<20), 0o644)
	s.ok("volume", "create", "used")
	s.ok("volume", "create", "free")
	running := strings.TrimSpace(s.ok(append([]string{"run", "-d", "-v", "used:/data"}, withFlags([]string{alpine, "sleep", "60"})...)...).stdout)
	s.ok(keepArgs("--name", "stopped", busybox, "true")...)

	r := s.cli("", "system", "prune")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "all dangling images") || strings.Contains(r.stderr, "volumes") {
		t.Errorf("declined: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	r = s.ok("system", "prune", "-a", "--volumes", "-f")
	for _, want := range []string{"Deleted Containers:\n", "untagged: " + busybox + "\ndeleted: sha256:", "Deleted Kernels:\nsha256:" + strings.Repeat("a", 64) + "\n", "Deleted Volumes:\nfree\n", "Total reclaimed space: "} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("system prune lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, alpine) || strings.Contains(r.stdout, running) {
		t.Errorf("system prune removed what the running container uses:\n%s", r.stdout)
	}
	if left := strings.Fields(s.ok("list", "-q").stdout); len(left) != 1 || left[0] != running {
		t.Errorf("containers left: %v", left)
	}
	if r := s.ok("images"); !strings.Contains(r.stdout, "alpine") || strings.Contains(r.stdout, "busybox") {
		t.Errorf("images left:\n%s", r.stdout)
	}
	if r := s.ok("volume", "ls", "-q"); r.stdout != "used\n" {
		t.Errorf("volumes left: %q", r.stdout)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old kernel: %v", err)
	}

	// What went is pulled again when it is needed.
	s.ok("delete", "-f", running)
	expect(t, s.ok(runArgs(busybox, "echo", "back")...), "back\n", 0)
}
