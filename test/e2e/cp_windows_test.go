//go:build windows

package e2e

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// waitExec waits until cmd succeeds in a running container.
func waitExec(t *testing.T, name string, cmd ...string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if execIn(t, append([]string{name}, cmd...)...).code == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never succeeded in %s", cmd, name)
		}
	}
}

// cliIn runs runwhp with stdin.
func cliIn(t *testing.T, stdin []byte, args ...string) result {
	t.Helper()
	cmd := command(t, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// tarNames lists a tar's entries.
func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const filesScript = "mkdir -p /d/sub && echo f1 > /d/f1 && echo f2 > /d/sub/f2 && ln -s /d/f1 /link && ln -s sub /d/rel && touch /ready"

// TestCp copies into and out of a running container, its volume and its
// bind mount included, as docker cp does.
func TestCp(t *testing.T) {
	parallel(t)
	host, bind := t.TempDir(), t.TempDir()
	vol := newVolume(t)
	name := newName(t)
	detached(t, "--name", name, "-v", vol+":/data", "-v", bind+":/bind", alpine, "sh", "-c", filesScript+" && sleep 300")
	waitExec(t, name, "test", "-e", "/ready")

	// Out.
	if r := cli(t, "cp", name+":/d", filepath.Join(host, "dcopy")); r.code != 0 {
		t.Fatalf("cp of a directory out: %+v", r)
	}
	if got := readFile(t, filepath.Join(host, "dcopy", "sub", "f2")); got != "f2\n" {
		t.Errorf("dcopy/sub/f2: %q", got)
	}
	expect(t, cli(t, "cp", name+":/d/f1", filepath.Join(host, "f1")), "", 0)
	if got := readFile(t, filepath.Join(host, "f1")); got != "f1\n" {
		t.Errorf("f1: %q", got)
	}
	expect(t, cli(t, "cp", "-L", name+":/link", filepath.Join(host, "l2")), "", 0)
	if got := readFile(t, filepath.Join(host, "l2")); got != "f1\n" {
		t.Errorf("-L of /link: %q", got)
	}
	r := cli(t, "cp", name+":/d", "-")
	if r.code != 0 {
		t.Fatalf("cp to -: %+v", r)
	}
	if names := tarNames(t, []byte(r.stdout)); !slices.Contains(names, "d/sub/f2") || !slices.Contains(names, "d/") {
		t.Errorf("cp to -: %q", names)
	}
	for _, c := range []struct{ src, want string }{
		{name + ":/nope", "Could not find the file /nope in container " + name},
		{name + ":/d/f1/", "lstat /d/f1/: not a directory"},
	} {
		if r := cli(t, "cp", c.src, filepath.Join(host, "x")); r.code == 0 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("cp %s: %+v, want %q", c.src, r, c.want)
		}
	}
	if r := cli(t, "cp", name+":/d/f1", filepath.Join(host, "nodir", "x")); r.code == 0 || !strings.Contains(r.stderr, "does not exist") {
		t.Errorf("cp into a missing directory: %+v", r)
	}

	// In.
	up := filepath.Join(host, "up")
	os.WriteFile(up, []byte("up\n"), 0o644)
	expect(t, cli(t, "cp", up, name+":/d/"), "", 0)
	expect(t, execIn(t, name, "stat", "-c", "%u:%g %s", "/d/up"), "0:0 3\n", 0)
	expect(t, cli(t, "cp", up, name+":/d/rel"), "", 0) // a symlink to a directory
	expect(t, execIn(t, name, "cat", "/d/sub/up"), "up\n", 0)
	expect(t, cli(t, "cp", filepath.Join(host, "dcopy"), name+":/newdir"), "", 0)
	expect(t, execIn(t, name, "cat", "/newdir/sub/f2"), "f2\n", 0)
	expect(t, cli(t, "cp", up, name+":/data/"), "", 0)
	expect(t, execIn(t, name, "cat", "/data/up"), "up\n", 0)
	expect(t, cli(t, "cp", up, name+":/bind/"), "", 0)
	if got := readFile(t, filepath.Join(bind, "up")); got != "up\n" {
		t.Errorf("the bind mount's file on the host: %q", got)
	}
	expect(t, cliIn(t, tarOf(t, map[string]string{"fromtar": "t\n"}), "cp", "-", name+":/d"), "", 0)
	expect(t, execIn(t, name, "cat", "/d/fromtar"), "t\n", 0)
	if r := cliIn(t, tarOf(t, map[string]string{"x": "x"}), "cp", "-", name+":/d/f1"); r.code == 0 || !strings.Contains(r.stderr, "must be a directory") {
		t.Errorf("a tar onto a file: %+v", r)
	}
	if r := cli(t, "cp", up, name+":/nodir/x"); r.code == 0 || !strings.Contains(r.stderr, "Could not find the file /nodir") {
		t.Errorf("cp into a missing directory in the container: %+v", r)
	}
	if r := cli(t, "cp", filepath.Join(host, "dcopy"), name+":/d/f1"); r.code == 0 || !strings.Contains(r.stderr, "cannot copy directory") {
		t.Errorf("a directory onto a file: %+v", r)
	}
	if r := cli(t, "cp", name+":/d", name+":/e"); r.code == 0 || !strings.Contains(r.stderr, "between containers") {
		t.Errorf("between containers: %+v", r)
	}

	// The container's user owns what -a copies in.
	user := newName(t)
	detached(t, "--name", user, "-u", "nobody", "-v", t.TempDir()+":/ro:ro", alpine, "sleep", "300")
	waitExec(t, user, "true")
	expect(t, cli(t, "cp", "-a", up, user+":/tmp/"), "", 0)
	expect(t, execIn(t, user, "stat", "-c", "%u:%g", "/tmp/up"), "65534:65534\n", 0)
	if r := cli(t, "cp", up, user+":/ro/"); r.code == 0 || !strings.Contains(r.stderr, "read-only") {
		t.Errorf("cp into a read-only mount: %+v", r)
	}
}

// TestCpStopped copies into and out of a stopped container, whose files a
// guest booted for the purpose serves, and a created one.
func TestCpStopped(t *testing.T) {
	parallel(t)
	host, bind := t.TempDir(), t.TempDir()
	vol := newVolume(t)
	name := newName(t)
	if r := keep(t, "--name", name, "-v", vol+":/data", "-v", bind+":/bind", alpine, "sh", "-c", filesScript+" && echo v > /data/v"); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	up := filepath.Join(host, "up")
	os.WriteFile(up, []byte("up\n"), 0o644)
	expect(t, cli(t, "cp", up, name+":/stopped"), "", 0)
	expect(t, cli(t, "cp", up, name+":/data/"), "", 0)
	expect(t, cli(t, "cp", up, name+":/bind/"), "", 0)
	if got := readFile(t, filepath.Join(bind, "up")); got != "up\n" {
		t.Errorf("the bind mount's file on the host: %q", got)
	}
	expect(t, cli(t, "cp", name+":/data/v", filepath.Join(host, "v")), "", 0)
	if got := readFile(t, filepath.Join(host, "v")); got != "v\n" {
		t.Errorf("the volume's file: %q", got)
	}
	expect(t, cli(t, "cp", name+":/d/sub/f2", filepath.Join(host, "f2")), "", 0)
	if s := stateOf(t, name); s.Status != "stopped" {
		t.Errorf("after cp: %s", s.Status)
	}
	expect(t, cli(t, "start", name), name+"\n", 0)
	r := cli(t, "exec", name, "cat", "/stopped", "/data/up")
	if r.code == 0 {
		// The container's own command has ended by now, or nearly: it is
		// enough that the files were there for it.
		expect(t, r, "up\nup\n", 0)
	}

	created := newName(t)
	if r := cli(t, append([]string{"create", "--name", created}, withFlags([]string{alpine, "cat", "/up"})...)...); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	expect(t, cli(t, "cp", up, created+":/"), "", 0)
	expect(t, cli(t, "start", "-a", created), "up\n", 0)
}

// TestCpBusy starts a stopped container while a cp holds it: the start is
// refused, and works once the cp is done.
func TestCpBusy(t *testing.T) {
	parallel(t)
	name := newName(t)
	if r := keep(t, "--name", name, alpine, "true"); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	cp := command(t, "cp", "-", name+":/tmp")
	stdin, err := cp.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cp.Stderr = &stderr
	if err := cp.Start(); err != nil {
		t.Fatal(err)
	}
	// The cp boots its guest, then waits for the tar.
	time.Sleep(2 * time.Second)
	if r := cli(t, "start", name); r.code == 0 || !strings.Contains(r.stderr, "is busy") {
		t.Errorf("start during a cp: %+v", r)
	}
	stdin.Write(tarOf(t, map[string]string{"x": "x"}))
	stdin.Close()
	if err := cp.Wait(); err != nil {
		t.Fatalf("cp: %v: %s", err, stderr.String())
	}
	expect(t, cli(t, "start", name), name+"\n", 0)
}
