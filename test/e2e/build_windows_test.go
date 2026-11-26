//go:build windows

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildDir makes a build context of files, a Dockerfile among them.
func buildDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// imageID is the ID image inspect gives ref.
func (s *ownStore) imageID(ref string) string {
	s.t.Helper()
	var out []struct{ Id string }
	if err := json.Unmarshal([]byte(s.ok("image", "inspect", ref).stdout), &out); err != nil || len(out) != 1 {
		s.t.Fatalf("image inspect %s: %v", ref, err)
	}
	return out[0].Id
}

// TestBuild builds an image as docker build does, again from the cache,
// and again with a file changed.
func TestBuild(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	dir := buildDir(t, map[string]string{
		"Dockerfile": fmt.Sprintf(`FROM %s
ARG GREETING=hi
ENV G=$GREETING
RUN echo "$G from run" > /greeting && hostname > /host
WORKDIR /app
COPY hello.txt sub ./
USER nobody
RUN id -un > /tmp/whoami
CMD ["cat", "/greeting"]
`, base),
		"hello.txt": "hello\n", "sub/x": "x", "skipped": "no",
		".dockerignore": "skipped\n",
	})
	r := s.ok("build", "-t", "built:1", dir)
	for _, want := range []string{"[1/5] FROM ", "[2/5] RUN echo", "[3/5] WORKDIR /app", "[4/5] COPY hello.txt sub ./", "naming to index.docker.io/library/built:1 done"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("progress lacks %q:\n%s", want, r.stderr)
		}
	}
	if got := s.ok("run", "--rm", "built:1").stdout; got != "hi from run\n" {
		t.Errorf("run: %q", got)
	}
	got := s.ok("run", "--rm", "built:1", "sh", "-c", "cat /host /tmp/whoami; pwd; ls /app; stat -c '%U %a' /app /app/hello.txt").stdout
	if want := "buildkitsandbox\nnobody\n/app\nhello.txt\nx\nroot 755\nroot 755\n"; got != want {
		t.Errorf("the image's files:\n got %q\nwant %q", got, want)
	}
	hist := s.ok("history", "--no-trunc", "built:1").stdout
	for _, want := range []string{`CMD ["cat" "/greeting"]`, "RUN |1 GREETING=hi /bin/sh -c id -un > /tmp/whoami # buildkit", "COPY hello.txt sub ./ # buildkit", "WORKDIR /app", "buildkit.dockerfile.v0"} {
		if !strings.Contains(hist, want) {
			t.Errorf("history lacks %q:\n%s", want, hist)
		}
	}
	id := s.imageID("built:1")

	// Again: every step from the cache, and the same image.
	r = s.ok("build", "-t", "built:1", dir)
	if n := strings.Count(r.stderr, "CACHED"); n != 4 || s.imageID("built:1") != id {
		t.Errorf("a rebuild: %d steps cached, ID %s against %s\n%s", n, s.imageID("built:1"), id, r.stderr)
	}
	// A touched file keeps the cache; a changed one reruns from its COPY.
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "hello.txt"), later, later)
	if q := strings.TrimSpace(s.ok("build", "-q", dir).stdout); q != id {
		t.Errorf("-q after a touch printed %q, want %s", q, id)
	}
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("changed\n"), 0o644)
	r = s.ok("build", "--iidfile", filepath.Join(dir, "..", "iid"), "-t", "built:2", dir)
	if n := strings.Count(r.stderr, "CACHED"); n != 2 {
		t.Errorf("after a change, %d steps cached:\n%s", n, r.stderr)
	}
	iid, _ := os.ReadFile(filepath.Join(dir, "..", "iid"))
	if string(iid) != s.imageID("built:2") || string(iid) == id {
		t.Errorf("--iidfile %q", iid)
	}
	if r := s.ok("build", "--no-cache", dir); strings.Contains(r.stderr, "#2 CACHED") {
		t.Errorf("--no-cache took from the cache:\n%s", r.stderr)
	}
	if r := s.ok("images"); !strings.Contains(r.stdout, "<none>") {
		t.Errorf("an untagged build is not listed as <none>:\n%s", r.stdout)
	}
}

// TestBuildMultiStage copies from stages and images, and builds only what
// the target needs.
// TestBuildOnEmptyLayer builds on an image with an empty layer, as
// mcr.microsoft.com/devcontainers/base has: the layer is no file in the
// store, and a step's guest boots without it.
func TestBuildOnEmptyLayer(t *testing.T) {
	s := newOwnStore(t)
	base := pushImage(t, "e2e/empty-layer:1", tarLayer(t, nil), tarLayer(t, map[string]string{"etc/marker": "base\n"}))
	dir := buildDir(t, map[string]string{
		"Dockerfile": fmt.Sprintf("FROM %s\nCOPY f /f\nRUN cat /etc/marker /f > /both\n", base),
		"f":          "copied\n",
	})
	s.ok("build", "-t", "onempty:1", dir)
	if r := s.ok(runArgs("onempty:1", "cat", "/both")...); r.stdout != "base\ncopied\n" {
		t.Errorf("run: %q", r.stdout)
	}
}

func TestBuildMultiStage(t *testing.T) {
	s := newOwnStore(t)
	base, bb := mirrored(t, alpine), mirrored(t, busybox)
	dir := buildDir(t, map[string]string{"Dockerfile": fmt.Sprintf(`FROM %[1]s AS never
RUN exit 1
FROM %[1]s AS build
RUN mkdir -p /out/d && echo built > /out/d/f && chown -R 5:6 /out && chmod 750 /out/d
FROM %[1]s
COPY --from=build /out /app
COPY --from=%[2]s /bin/busybox /bb
`, base, bb)})
	s.ok("build", "-t", "multi:1", dir)
	if got := s.ok("run", "--rm", "multi:1", "sh", "-c", "cat /app/d/f; stat -c '%u:%g %a' /app /app/d /app/d/f; test -s /bb && echo bb").stdout; got != "built\n0:0 755\n5:6 750\n5:6 644\nbb\n" {
		t.Errorf("the copies: %q", got)
	}
	r := s.ok("build", "--target", "build", "-t", "stage:1", dir)
	if strings.Contains(r.stderr, "exit 1") || strings.Contains(r.stderr, "COPY") {
		t.Errorf("--target build ran other stages:\n%s", r.stderr)
	}
	if r := s.cli("", "build", "--target", "nope", dir); r.code == 0 || !strings.Contains(r.stderr, `target stage "nope" could not be found`) {
		t.Errorf("an unknown target: %d %s", r.code, r.stderr)
	}
}

// TestBuildCopy copies files with BuildKit's rules: owners, modes,
// destinations, archives and URLs.
func TestBuildCopy(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)

	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "t/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 123, Gid: 456})
	tw.WriteHeader(&tar.Header{Name: "t/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Uid: 123, Gid: 456})
	tw.Write([]byte("x"))
	tw.Close()
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
		w.Write([]byte("url"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	sum := sha256.Sum256([]byte("url"))

	dir := buildDir(t, map[string]string{"Dockerfile": fmt.Sprintf(`FROM %s
RUN mkdir -m 0700 /existing && chown 1:1 /existing && mkdir /real && ln -s /real /link
COPY f1 /a/b/
COPY --chown=nobody:nogroup f1 /p/q/
COPY --chown=1000 f1 /own/
COPY --chmod=640 f1 /m1
COPY d /dst
COPY d /existing
COPY f* /wild/
COPY f1 /link/
COPY --link f1 /linked/
ADD a.tar.gz /tar/
ADD --chown=nobody a.tar.gz /tar2/
ADD --checksum=sha256:%s http://127.0.0.1:%s/u.txt /u
COPY <<EOF /hdoc
hello
EOF
`, base, hex.EncodeToString(sum[:]), port),
		"f1": "a", "f2": "bb", "d/c": "c", "d/sub/x": "x", "a.tar.gz": tgz.String()})
	s.ok("build", "-t", "copy:1", dir)
	got := s.ok("run", "--rm", "copy:1", "sh", "-c", `for p in /a /a/b /a/b/f1 /p /p/q /p/q/f1 /own /own/f1 /m1 /dst /dst/c /dst/sub/x /existing /existing/c /wild/f1 /wild/f2 /real/f1 /linked/f1 /tar/t/x /tar2/t/x /u /hdoc; do stat -c "%n %u:%g %a" $p; done; cat /u /hdoc; date -u -r /u +%Y`).stdout
	want := `/a 0:0 755
/a/b 0:0 755
/a/b/f1 0:0 755
/p 65534:65533 755
/p/q 65534:65533 755
/p/q/f1 65534:65533 755
/own 1000:1000 755
/own/f1 1000:1000 755
/m1 0:0 640
/dst 0:0 755
/dst/c 0:0 755
/dst/sub/x 0:0 755
/existing 1:1 700
/existing/c 0:0 755
/wild/f1 0:0 755
/wild/f2 0:0 755
/real/f1 0:0 755
/linked/f1 0:0 755
/tar/t/x 123:456 644
/tar2/t/x 65534:65534 644
/u 0:0 600
/hdoc 0:0 644
urlhello
2025
`
	if got != want {
		t.Errorf("the copies:\n got %q\nwant %q", got, want)
	}

	bad := buildDir(t, map[string]string{"Dockerfile": fmt.Sprintf("FROM %s\nADD --checksum=sha256:%s http://127.0.0.1:%s/u.txt /u\n", base, strings.Repeat("0", 64), port)})
	if r := s.cli("", "build", bad); r.code == 0 || !strings.Contains(r.stderr, "digest mismatch") {
		t.Errorf("a wrong checksum: %d %s", r.code, r.stderr)
	}
	missing := buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nCOPY missing /x\n"})
	if r := s.cli("", "build", missing); r.code != 1 || !strings.Contains(r.stderr, `"/missing": not found`) || !strings.Contains(r.stderr, ">>> COPY missing /x") {
		t.Errorf("a missing source: %d %s", r.code, r.stderr)
	}
}

// TestBuildRun runs heredocs, mounts and failing commands.
func TestBuildRun(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("hush"), 0o600)
	df := fmt.Sprintf(`FROM %s
RUN <<EOF
echo one > /hd1
echo two >> /hd1
EOF
RUN <<EOF
#!/bin/sh
echo shebang > /hd2
EOF
RUN --mount=type=cache,target=/cache,uid=1000 ls -lnd /cache > /cache.ls && cat /cache/count 2>/dev/null > /count; echo x >> /cache/count
RUN --mount=type=secret,id=tok cat /run/secrets/tok > /seen && test ! -e /run/secrets/tok.copy
RUN --mount=type=secret,id=tok,env=TOKEN sh -c 'echo $TOKEN > /seen-env'
RUN --mount=type=tmpfs,target=/scratch touch /scratch/gone
RUN --mount=type=bind,source=ctx.txt,target=/ctx.txt cp /ctx.txt /ctx.copy
`, base)
	dir := buildDir(t, map[string]string{"Dockerfile": df, "ctx.txt": "from the context"})
	s.ok("build", "--secret", "id=tok,src="+secret, "-t", "run:1", dir)
	got := s.ok("run", "--rm", "run:1", "sh", "-c", "cat /hd1 /hd2 /seen /seen-env /ctx.copy; awk '{print $3}' /cache.ls; for d in /cache /scratch /run/secrets; do test -e $d && echo $d left; done; true").stdout
	if want := "one\ntwo\nshebang\nhushhush\nfrom the context1000\n"; got != want {
		t.Errorf("the image:\n got %q\nwant %q", got, want)
	}
	// The cache mount keeps what the first build left.
	s.ok("build", "--no-cache", "--secret", "id=tok,src="+secret, "-t", "run:2", dir)
	if got := s.ok("run", "--rm", "run:2", "cat", "/count").stdout; got != "x\n" {
		t.Errorf("the cache mount's file, second time: %q", got)
	}

	fail := buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nRUN echo out; exit 3\n"})
	r := s.cli("", "build", fail)
	if r.code != 1 || !strings.Contains(r.stderr, `process "/bin/sh -c echo out; exit 3" did not complete successfully: exit code: 3`) || !strings.Contains(r.stderr, " out\n") {
		t.Errorf("a failing RUN: %d\n%s", r.code, r.stderr)
	}
	if r := s.cli("", "build", buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nRUN --mount=type=ssh true\n"})); r.code != 1 || !strings.Contains(r.stderr, "ssh is not supported") {
		t.Errorf("an ssh mount: %d %s", r.code, r.stderr)
	}
}

// TestBuildNetwork reaches the host from RUN, or nothing without a
// network.
func TestBuildNetwork(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("host")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	dir := buildDir(t, map[string]string{"Dockerfile": fmt.Sprintf("FROM %s\nRUN wget -qO /got http://hg:%s/\n", base, port)})
	s.ok("build", "--add-host", "hg:host-gateway", "-t", "net:1", dir)
	if got := s.ok("run", "--rm", "net:1", "cat", "/got").stdout; got != "host" {
		t.Errorf("fetched %q", got)
	}
	if r := s.cli("", "build", "--no-cache", "--network", "none", "--add-host", "hg:10.0.2.2", dir); r.code != 1 {
		t.Errorf("without a network, the fetch worked: %d", r.code)
	}
}

// TestBuildStdin reads the Dockerfile from stdin, with and without a
// context.
func TestBuildStdin(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	if r := s.cli("FROM "+base+"\nRUN echo stdin > /f\n", "build", "-t", "stdin:1", "-"); r.code != 0 {
		t.Fatalf("build -: %d %s", r.code, r.stderr)
	}
	if got := s.ok("run", "--rm", "stdin:1", "cat", "/f").stdout; got != "stdin\n" {
		t.Errorf("build -: %q", got)
	}
	if r := s.cli("FROM "+base+"\nCOPY f /f\n", "build", "-"); r.code != 1 || !strings.Contains(r.stderr, `"/f": not found`) {
		t.Errorf("COPY with no context: %d %s", r.code, r.stderr)
	}
	dir := buildDir(t, map[string]string{"f": "file"})
	if r := s.cli("FROM "+base+"\nCOPY f /f\n", "build", "-f", "-", "-t", "stdin:2", dir); r.code != 0 {
		t.Fatalf("build -f -: %d %s", r.code, r.stderr)
	}
	if got := s.ok("run", "--rm", "stdin:2", "cat", "/f").stdout; got != "file" {
		t.Errorf("build -f -: %q", got)
	}
}

// TestBuilderPrune counts the build cache in system df, and removes it:
// what no image uses, then all of it, leaving the images' layers.
func TestBuilderPrune(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	dir := buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nRUN echo a > /a\nRUN --mount=type=cache,target=/c echo b > /b\n"})
	s.ok("build", "-t", "pruned:1", dir)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\nRUN echo other > /a\n"), 0o644)
	other := strings.TrimSpace(s.ok("build", "-q", dir).stdout)
	// pruned:1's layers are now the cache's alone.
	s.ok("rmi", "pruned:1")
	cacheEntries := func() int {
		var df struct {
			Summary []struct {
				Type  string
				Total int
			}
		}
		json.Unmarshal([]byte(s.ok("system", "df", "--format", "json").stdout), &df)
		for _, row := range df.Summary {
			if row.Type == "Build Cache" {
				return row.Total
			}
		}
		return -1
	}
	if n := cacheEntries(); n != 3 {
		t.Errorf("system df counts %d build cache entries", n)
	}
	r := s.ok("builder", "prune", "-f")
	if !strings.Contains(r.stdout, "Deleted build cache objects:") || !strings.Contains(r.stdout, "Total reclaimed space:") {
		t.Errorf("builder prune:\n%s", r.stdout)
	}
	if n := cacheEntries(); n != 1 {
		t.Errorf("after builder prune, %d build cache entries", n)
	}
	s.ok("builder", "prune", "-a", "-f")
	if got := s.ok("run", "--rm", other, "cat", "/a").stdout; got != "other\n" {
		t.Errorf("the untagged image after builder prune -a: %q", got)
	}
	if got := s.ok("run", "--rm", "--pull", "never", base, "true"); got.code != 0 {
		t.Errorf("the base image after builder prune -a: %+v", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(s.root, "buildcache", "entries", "*.json")); len(entries) != 0 {
		t.Errorf("%d entries left", len(entries))
	}
	if vols, _ := filepath.Glob(filepath.Join(s.root, "buildcache", "volumes", "*")); len(vols) != 0 {
		t.Errorf("cache volumes left: %q", vols)
	}
}

// TestBuildInterrupt stops a build with Ctrl+C during a RUN: it exits 130,
// leaves no step's disk behind, and the steps done stay in the cache.
func TestBuildInterrupt(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	dir := buildDir(t, map[string]string{"Dockerfile": "FROM " + base + "\nRUN echo first > /f\nRUN echo ready; sleep 300\n"})
	cmd := exec.Command(runwhp(t), "build", dir)
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+s.root)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(stderr.String(), " ready\n"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the RUN never got ready:\n%s", stderr.String())
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
			t.Errorf("exit %d after Ctrl+C:\n%s", code, stderr.String())
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the build did not stop after Ctrl+C")
	}
	if disks, _ := filepath.Glob(filepath.Join(s.root, "scratch", "*")); len(disks) != 0 {
		t.Errorf("step disks left: %q", disks)
	}
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\nRUN echo first > /f\nRUN true\n"), 0o644)
	if r := s.ok("build", dir); !strings.Contains(r.stderr, "#2 CACHED") {
		t.Errorf("the step done before Ctrl+C was not cached:\n%s", r.stderr)
	}
}
