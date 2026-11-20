//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitImage has git in it, for the ownership checks git makes.
const gitImage = "alpine/git:v2.47.2"

// shared is a new host directory holding files.
func shared(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readHost(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBindReadWrite(t *testing.T) {
	dir := shared(t, map[string]string{"in.txt": "from the host\n", "sub/deep.txt": "deep\n"})
	r := run(t, "-v", dir+":/src", alpine, "sh", "-c",
		"cat /src/in.txt /src/sub/deep.txt && echo from the container > /src/out.txt && mkdir -p /src/new/dir && ls /src")
	expect(t, r, "from the host\ndeep\nin.txt\nnew\nout.txt\nsub\n", 0)
	if got := readHost(t, filepath.Join(dir, "out.txt")); got != "from the container\n" {
		t.Errorf("host reads %q", got)
	}
	if st, err := os.Stat(filepath.Join(dir, "new", "dir")); err != nil || !st.IsDir() {
		t.Errorf("new/dir on the host: %v", err)
	}
}

func TestBindLargeFiles(t *testing.T) {
	data := make([]byte, 20<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	dir := shared(t, map[string]string{"in": string(data)})
	r := run(t, "-v", dir+":/s", alpine, "sh", "-c",
		"sha256sum /s/in | cut -d' ' -f1; head -c 20971520 /dev/urandom > /s/out; sha256sum /s/out | cut -d' ' -f1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Fields(r.stdout)
	if len(lines) != 2 || lines[0] != hex.EncodeToString(sum[:]) {
		t.Fatalf("container read %q, want %x", r.stdout, sum)
	}
	out := sha256.Sum256([]byte(readHost(t, filepath.Join(dir, "out"))))
	if lines[1] != hex.EncodeToString(out[:]) {
		t.Errorf("container wrote %s, host reads %x", lines[1], out)
	}
}

func TestBindReadOnly(t *testing.T) {
	dir := shared(t, map[string]string{"f": "data"})
	r := run(t, "-v", dir+":/s:ro", alpine, "sh", "-c", "cat /s/f; echo; touch /s/new")
	expect(t, r, "data\n", 1)
	if !strings.Contains(r.stderr, "Read-only file system") {
		t.Errorf("stderr %q", r.stderr)
	}
	expect(t, run(t, "--mount", "type=bind,source="+dir+",target=/s,readonly", alpine, "rm", "/s/f"), "", 1)
	if _, err := os.Stat(filepath.Join(dir, "f")); err != nil {
		t.Errorf("a read-only mount lost its file: %v", err)
	}
}

func TestBindFile(t *testing.T) {
	dir := shared(t, map[string]string{"app.conf": "key=1\n", "hosts": "1.2.3.4 custom\n"})
	r := run(t,
		"-v", filepath.Join(dir, "app.conf")+":/new/dir/app.conf",
		"-v", filepath.Join(dir, "hosts")+":/etc/hosts:ro",
		alpine, "sh", "-c", "cat /new/dir/app.conf /etc/hosts; echo key=2 > /new/dir/app.conf")
	// The user's /etc/hosts wins over the one runwhp writes.
	expect(t, r, "key=1\n1.2.3.4 custom\n", 0)
	if got := readHost(t, filepath.Join(dir, "app.conf")); got != "key=2\n" {
		t.Errorf("host reads %q", got)
	}
	if got := readHost(t, filepath.Join(dir, "hosts")); got != "1.2.3.4 custom\n" {
		t.Errorf("runwhp wrote into the host's file: %q", got)
	}
}

func TestBindNested(t *testing.T) {
	outer := shared(t, map[string]string{"o": "outer\n"})
	inner := shared(t, map[string]string{"i": "inner\n"})
	// Given inner first: parents are mounted first whatever the order.
	r := run(t, "-v", inner+":/x/y", "-v", outer+":/x", alpine, "sh", "-c", "cat /x/o /x/y/i")
	expect(t, r, "outer\ninner\n", 0)
	if st, err := os.Stat(filepath.Join(outer, "y")); err != nil || !st.IsDir() {
		t.Errorf("the inner mount point was not made in the outer share: %v", err)
	}
}

func TestBindOwner(t *testing.T) {
	dir := shared(t, map[string]string{"f": ""})
	r := run(t, "-u", "1000:1001", "-v", dir+":/s", "-w", "/s", alpine, "sh", "-c",
		"stat -c '%u:%g %a' . f; echo x > mine; stat -c '%u:%g %a' mine")
	expect(t, r, "1000:1001 755\n1000:1001 755\n1000:1001 644\n", 0)
	// Without -u, alpine runs as root.
	expect(t, run(t, "-v", dir+":/s", alpine, "stat", "-c", "%u:%g", "/s/f"), "0:0\n", 0)
}

func TestBindGitOwnership(t *testing.T) {
	dir := shared(t, nil)
	r := run(t, "-u", "1000", "-e", "HOME=/tmp", "-v", dir+":/repo", "-w", "/repo", "--entrypoint", "sh", gitImage, "-c",
		"git init -q && git -c user.email=e@x -c user.name=e commit -q --allow-empty -m x && git status --short && echo ok")
	expect(t, r, "ok\n", 0)
}

func TestBindMetadataPersists(t *testing.T) {
	dir := shared(t, map[string]string{"script": "#!/bin/sh\necho ran\n", "owned": ""})
	expect(t, run(t, "-v", dir+":/s", alpine, "sh", "-c",
		"chmod 700 /s/script && chown 999:998 /s/owned && chmod 640 /s/owned && ln -s script /s/link && mkfifo /s/fifo"), "", 0)
	r := run(t, "-v", dir+":/s", alpine, "sh", "-c",
		"stat -c '%a %u:%g %F' /s/script /s/owned /s/link /s/fifo; readlink /s/link; /s/link")
	expect(t, r, "700 0:0 regular file\n640 999:998 regular empty file\n777 0:0 symbolic link\n644 0:0 fifo\nscript\nran\n", 0)
}

func TestBindRenameAndDelete(t *testing.T) {
	dir := shared(t, map[string]string{"a": "new", "b": "old", "c": "kept open"})
	r := run(t, "-v", dir+":/s", alpine, "sh", "-c",
		"mv /s/a /s/b && cat /s/b && echo && exec 3</s/c && rm /s/c && cat <&3 && echo && ls /s")
	expect(t, r, "new\nkept open\nb\n", 0)
	if _, err := os.Stat(filepath.Join(dir, "c")); !os.IsNotExist(err) {
		t.Errorf("c is still on the host: %v", err)
	}
}

func TestBindSeesHostChanges(t *testing.T) {
	dir := shared(t, map[string]string{"f": "before"})
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(runwhp(t), runArgs("-v", dir+":/s", alpine, "sh", "-c",
		`cat /s/f; echo; while [ "$(cat /s/f)" != after ]; do sleep 0.1; done; echo seen`)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(stdout.String(), "before") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	changed := time.Now()
	os.WriteFile(filepath.Join(dir, "f"), []byte("after"), 0o644)
	select {
	case err := <-done:
		if err != nil || stdout.String() != "before\nseen\n" {
			t.Fatalf("%v: %q %q", err, stdout.String(), stderr.String())
		}
		if took := time.Since(changed); took > 3*time.Second {
			t.Errorf("the change took %v to show", took)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("the container never saw the change: %q", stdout.String())
	}
}

func TestBindJunctionIsNotFollowed(t *testing.T) {
	outside := shared(t, map[string]string{"secret": "do not read"})
	dir := shared(t, nil)
	link := filepath.Join(dir, "out")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}
	r := run(t, "-v", dir+":/s", alpine, "sh", "-c", "stat -c %F /s/out; cat /s/out/secret")
	if r.code == 0 || !strings.HasPrefix(r.stdout, "symbolic link\n") || strings.Contains(r.stdout, "do not read") {
		t.Errorf("exit %d, stdout %q", r.code, r.stdout)
	}
}

func TestBindMountPoints(t *testing.T) {
	dir := shared(t, map[string]string{"f": "x"})
	// alpine's /var/run is a link to /run: the mount lands where it leads.
	expect(t, run(t, "-v", dir+":/var/run/app", alpine, "ls", "/run/app"), "f\n", 0)
	// The working directory may be in a share, and is made there.
	expect(t, run(t, "-v", dir+":/s", "-w", "/s/made/here", alpine, "pwd"), "/s/made/here\n", 0)
	if _, err := os.Stat(filepath.Join(dir, "made", "here")); err != nil {
		t.Errorf("working directory not made on the host: %v", err)
	}
}

func TestBindSources(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "made", "by", "v")
	expect(t, run(t, "-v", missing+":/m", alpine, "ls", "/m"), "", 0)
	if st, err := os.Stat(missing); err != nil || !st.IsDir() {
		t.Errorf("-v did not make its source: %v", err)
	}
	r := run(t, "--mount", "type=bind,source="+filepath.Join(parent, "nope")+",target=/m", alpine, "true")
	expect(t, r, "", 125)
	if !strings.Contains(r.stderr, "does not exist") {
		t.Errorf("stderr %q", r.stderr)
	}
	r = run(t, "-v", "data:/m", alpine, "true")
	expect(t, r, "", 125)
	if !strings.Contains(r.stderr, "named volumes") {
		t.Errorf("stderr %q", r.stderr)
	}
}

// TestBindManyFiles makes and lists a tree of many small files, which is
// mostly lookups and attribute requests.
func TestBindManyFiles(t *testing.T) {
	dir := shared(t, nil)
	r := run(t, "-v", dir+":/s", "-w", "/s", alpine, "sh", "-c", `
		for d in $(seq 20); do mkdir d$d; for f in $(seq 50); do echo $d$f > d$d/f$f; done; done
		find . -type f | wc -l; cat d7/f13; du -s . >/dev/null && echo ok`)
	expect(t, r, "1000\n713\nok\n", 0)
	ents, err := os.ReadDir(filepath.Join(dir, "d20"))
	if err != nil || len(ents) != 50 {
		t.Errorf("host sees %d files in d20: %v", len(ents), err)
	}
	if t.Failed() {
		fmt.Println(r.stderr)
	}
}
