//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/ext4"
)

// newName returns a container name of the test's own, and deletes the
// container by that name when the test ends.
func newName(t *testing.T) string {
	t.Helper()
	var b [6]byte
	rand.Read(b[:])
	name := "e2e-" + hex.EncodeToString(b[:])
	t.Cleanup(func() { cli(t, "delete", "-f", name) })
	return name
}

// keep runs a container that is kept after it stops.
func keep(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(runwhp(t), keepArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// stateJSON is the part of runwhp state the tests read.
type stateJSON struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Pid        int      `json:"pid"`
	Bundle     string   `json:"bundle"`
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	Command    []string `json:"command"`
	ExitCode   *int     `json:"exitCode"`
	Error      string   `json:"error"`
	GuestPid   int      `json:"guestPid"`
	AutoRemove bool     `json:"autoRemove"`
	Volumes    []string `json:"volumes"`
}

func stateOf(t *testing.T, ref string) stateJSON {
	t.Helper()
	r := cli(t, "state", ref)
	if r.code != 0 {
		t.Fatalf("state %s: %+v", ref, r)
	}
	var s stateJSON
	if err := json.Unmarshal([]byte(r.stdout), &s); err != nil {
		t.Fatalf("state %s: %v in %q", ref, err, r.stdout)
	}
	return s
}

// layerState reads the ext4 state of a stopped container's writable layer.
func layerState(t *testing.T, s stateJSON) ext4.State {
	t.Helper()
	f, err := os.Open(filepath.Join(s.Bundle, "rw.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := ext4.ReadState(f)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestKeptContainer(t *testing.T) {
	name := newName(t)
	r := keep(t, "--name", name, alpine, "sh", "-c", "echo kept > /kept; sync; exit 3")
	expect(t, r, "", 3)

	s := stateOf(t, name)
	if s.Status != "stopped" || s.ExitCode == nil || *s.ExitCode != 3 || s.Pid != 0 || s.Name != name ||
		s.Image != alpine || len(s.ID) != 64 || s.AutoRemove || s.Command[0] != "sh" {
		t.Errorf("state %+v", s)
	}
	// Prefixes of the ID name it too.
	if p := stateOf(t, s.ID[:8]); p.ID != s.ID {
		t.Errorf("state by prefix: %s", p.ID)
	}
	if st := layerState(t, s); !st.Clean || !st.Journal || st.JournalStart != 0 || st.Errors {
		t.Errorf("writable layer after the run: %+v", st)
	}

	l := cli(t, "list")
	if l.code != 0 || !strings.Contains(l.stdout, s.ID[:12]) || !strings.Contains(l.stdout, "Exited (3)") || !strings.Contains(l.stdout, name) {
		t.Errorf("list: %+v", l)
	}
	if q := cli(t, "list", "-q"); !strings.Contains(q.stdout, s.ID+"\n") {
		t.Errorf("list -q: %q", q.stdout)
	}
	var all []stateJSON
	if j := cli(t, "list", "--format", "json"); json.Unmarshal([]byte(j.stdout), &all) != nil {
		t.Errorf("list --format json: %q", j.stdout)
	}
	found := false
	for _, c := range all {
		found = found || c.ID == s.ID
	}
	if !found {
		t.Errorf("list --format json lacks %s", s.ID)
	}

	if d := cli(t, "delete", name); d.code != 0 || d.stdout != name+"\n" {
		t.Errorf("delete: %+v", d)
	}
	if _, err := os.Stat(s.Bundle); !os.IsNotExist(err) {
		t.Errorf("the container's directory is still there: %v", err)
	}
	if d := cli(t, "delete", name); d.code != 1 || !strings.Contains(d.stderr, "no such container") {
		t.Errorf("second delete: %+v", d)
	}
	if st := cli(t, "state", name); st.code != 1 {
		t.Errorf("state after delete: %+v", st)
	}
}

func TestContainerNames(t *testing.T) {
	a, b := newName(t), newName(t)
	expect(t, keep(t, "--name", a, alpine, "true"), "", 0)
	r := keep(t, "--name", strings.ToUpper(a), alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "already in use") {
		t.Errorf("a second container with the same name: %+v", r)
	}
	if r := keep(t, "--name", "-bad", alpine, "true"); r.code != 125 || !strings.Contains(r.stderr, "invalid container name") {
		t.Errorf("a bad name: %+v", r)
	}
	id := stateOf(t, a).ID
	if r := cli(t, "rename", a, b); r.code != 0 {
		t.Fatalf("rename: %+v", r)
	}
	if s := stateOf(t, b); s.ID != id || s.Name != b {
		t.Errorf("after rename: %+v", s)
	}
	if r := cli(t, "state", a); r.code != 1 {
		t.Errorf("the old name still resolves: %+v", r)
	}
	// The old name is free again.
	expect(t, keep(t, "--name", a, alpine, "true"), "", 0)
	if r := cli(t, "rename", a, b); r.code != 1 || !strings.Contains(r.stderr, "already in use") {
		t.Errorf("rename to a name in use: %+v", r)
	}
	if r := cli(t, "state", "e2e-no-such-container"); r.code != 1 || !strings.Contains(r.stderr, "no such container") {
		t.Errorf("state of a missing container: %+v", r)
	}
	if r := cli(t, "delete"); r.code != 2 {
		t.Errorf("delete with no container: %+v", r)
	}
}

// TestEtcFiles checks that runwhp binds /etc/hostname, /etc/hosts and
// /etc/resolv.conf over the image's, as Docker does, rather than writing
// them into the writable layer.
func TestEtcFiles(t *testing.T) {
	r := run(t, "--hostname", "box", alpine, "sh", "-c",
		`cat /etc/hostname; grep -c box /etc/hosts; grep -c nameserver /etc/resolv.conf
		 grep -cE ' /etc/(hostname|hosts|resolv.conf) ' /proc/mounts
		 sed -i 's/box/other/' /etc/hosts 2>/dev/null && echo replaced || echo busy
		 echo '10.9.9.9 extra' >> /etc/hosts && grep -c extra /etc/hosts`)
	expect(t, r, "box\n1\n1\n3\nbusy\n1\n", 0)

	// A volume over /etc still gets runwhp's files, on top of the image's
	// copy in it.
	v := newVolume(t)
	r = run(t, "--hostname", "box", "-v", v+":/etc", alpine, "sh", "-c", "cat /etc/hostname; test -f /etc/passwd && echo copied")
	expect(t, r, "box\ncopied\n", 0)
	r = run(t, "--hostname", "box2", "-v", v+":/etc", alpine, "cat", "/etc/hostname")
	expect(t, r, "box2\n", 0)

	// A host file mounted over /etc/hosts wins.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hosts"), []byte("1.2.3.4 mine\n"), 0o644)
	r = run(t, "-v", filepath.Join(dir, "hosts")+":/etc/hosts:ro", alpine, "cat", "/etc/hosts")
	expect(t, r, "1.2.3.4 mine\n", 0)

}

// TestVolumeKeptByContainer checks that a volume a stopped container
// mounts cannot be removed, as Docker refuses to.
func TestVolumeKeptByContainer(t *testing.T) {
	v, name := newVolume(t), newName(t)
	expect(t, keep(t, "--name", name, "-v", v+":/data", alpine, "true"), "", 0)
	if r := volumeCmd(t, nil, "", "rm", v); r.code != 1 || !strings.Contains(r.stderr, "in use by container "+name) {
		t.Errorf("volume rm: %+v", r)
	}
	if s := stateOf(t, name); len(s.Volumes) != 1 || s.Volumes[0] != v {
		t.Errorf("state volumes %v", s.Volumes)
	}
	if r := cli(t, "delete", name); r.code != 0 {
		t.Fatalf("delete: %+v", r)
	}
	if r := volumeCmd(t, nil, "", "rm", v); r.code != 0 {
		t.Errorf("volume rm after delete: %+v", r)
	}
}

// TestFsyncKept checks that fsync in a kept container's writable layer
// reaches the host, as it would under Docker.
func TestFsyncKept(t *testing.T) {
	name := newName(t)
	r := keep(t, "--name", name, "--debug", alpine, "sh", "-c", "for i in $(seq 20); do echo $i > /f$i; sync /f$i; done; echo done")
	if r.code != 0 || !strings.Contains(r.stdout, "done") {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
	flushed := false
	for _, line := range strings.Split(r.stderr, "\n") {
		if strings.Contains(line, `blk "scratch"`) && !strings.Contains(line, " 0 flushes") {
			flushed = true
		}
	}
	if !flushed {
		t.Errorf("the writable layer was never flushed:\n%s", r.stderr)
	}
}
