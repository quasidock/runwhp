//go:build windows

package e2e

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/quasidock/runwhp/internal/ext4"
)

// newVolume returns a volume name no other run uses, and removes the volume
// when the test ends.
func newVolume(t *testing.T) string {
	t.Helper()
	var b [4]byte
	rand.Read(b[:])
	v := "e2e-" + hex.EncodeToString(b[:])
	t.Cleanup(func() { volumeCmd(t, nil, "", "rm", v) })
	return v
}

// volumeCmd runs runwhp volume with extra environment variables and stdin.
func volumeCmd(t *testing.T, env []string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(runwhp(t), append([]string{"volume"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

func volumeState(t *testing.T, v string) ext4.State {
	t.Helper()
	f, err := os.Open(filepath.Join(storeDir(t), "volumes", v, "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := ext4.ReadState(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVolumePersists(t *testing.T) {
	v := newVolume(t)
	expect(t, run(t, "-v", v+":/data", alpine, "sh", "-c", "ls -A /data; stat -c '%a %u:%g' /data; echo kept > /data/f"), "755 0:0\n", 0)
	expect(t, run(t, "-v", v+":/elsewhere", alpine, "cat", "/elsewhere/f"), "kept\n", 0)
	if r := volumeCmd(t, nil, "", "ls", "-q"); !strings.Contains(r.stdout, v+"\n") {
		t.Errorf("volume ls -q does not list %s: %q", v, r.stdout)
	}
	if s := volumeState(t, v); !s.Clean || !s.Journal || s.MountCount != 2 {
		t.Errorf("after two runs the volume is %+v, want clean, mounted twice", s)
	}
}

// richImage is alpine with a /seed directory holding every kind of file a
// volume must copy from an image, with owners and modes.
func richImage(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, body)
	}
	add(&tar.Header{Name: "seed/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 70, Gid: 71}, "")
	add(&tar.Header{Name: "seed/file", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1000, Gid: 1001}, "data")
	add(&tar.Header{Name: "seed/suid", Typeflag: tar.TypeReg, Mode: 0o4755}, "x")
	add(&tar.Header{Name: "seed/link", Typeflag: tar.TypeSymlink, Linkname: "file", Mode: 0o777}, "")
	add(&tar.Header{Name: "seed/hard", Typeflag: tar.TypeLink, Linkname: "seed/file"}, "")
	add(&tar.Header{Name: "seed/pipe", Typeflag: tar.TypeFifo, Mode: 0o600}, "")
	add(&tar.Header{Name: "seed/sub/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 5, Gid: 5}, "")
	add(&tar.Header{Name: "seed/sub/deep", Typeflag: tar.TypeReg, Mode: 0o644}, "deep")
	tw.Close()
	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return pushImage(t, "e2e/seed:1", layer)
}

// pushImage serves alpine with layers added from a registry on localhost,
// and returns the image's reference.
func pushImage(t *testing.T, repo string, layers ...v1.Layer) string {
	t.Helper()
	runwhp(t)
	base, err := remote.Image(name.MustParseReference(alpine), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, layers...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.Replace(strings.TrimPrefix(srv.URL, "http://"), "127.0.0.1", "localhost", 1) + "/" + repo
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	// The registry goes with the test, and so does the image.
	t.Cleanup(func() { cli(t, "rmi", "-f", ref) })
	return ref
}

// TestVolumeFilledFromImage mounts a new volume where the image has files:
// the volume gets a copy of them, as with Docker, and keeps its own content
// from then on.
func TestVolumeFilledFromImage(t *testing.T) {
	img := richImage(t)
	v := newVolume(t)
	script := `cd /seed
		for f in . file suid link pipe sub sub/deep; do stat -c "%n %a %u:%g %F" $f; done
		[ $(stat -c %i file) = $(stat -c %i hard) ] && echo hard linked
		readlink link; cat sub/deep; echo; echo $(ls -A)
		echo changed > file`
	expect(t, run(t, "--pull", "always", "-v", v+":/seed", img, "sh", "-c", script), `. 750 70:71 directory
file 640 1000:1001 regular file
suid 4755 0:0 regular file
link 777 0:0 symbolic link
pipe 600 0:0 fifo
sub 700 5:5 directory
sub/deep 644 0:0 regular file
hard linked
file
deep
file hard link pipe sub suid
`, 0)
	expect(t, run(t, "--pull", "never", "-v", v+":/seed", img, "cat", "/seed/file"), "changed\n", 0)

	// nocopy, in both spellings, leaves a new volume empty.
	expect(t, run(t, "--pull", "never", "--mount", "type=volume,source="+newVolume(t)+",target=/seed,volume-nocopy", img, "ls", "-A", "/seed"), "", 0)
	expect(t, run(t, "--pull", "never", "-v", newVolume(t)+":/seed:nocopy", img, "ls", "-A", "/seed"), "", 0)

	// Under a bind mount, the volume still gets the image's files, not the
	// host's; where the image has nothing, it stays empty.
	dir := shared(t, map[string]string{"sub/host": "h"})
	expect(t, run(t, "--pull", "never", "-v", dir+":/seed", "-v", newVolume(t)+":/seed/sub", "-v", newVolume(t)+":/seed2", img,
		"sh", "-c", "ls /seed/sub; ls -A /seed2"), "deep\n", 0)
}

func TestVolumeReadOnlyAndTwice(t *testing.T) {
	v := newVolume(t)
	expect(t, run(t, "-v", v+":/a", "-v", strings.ToUpper(v)+":/b", alpine, "sh", "-c", "echo x > /a/f; cat /b/f"), "x\n", 0)
	expect(t, run(t, "-v", v+":/r:ro", alpine, "sh", "-c", "cat /r/f; if touch /r/g 2>/dev/null; then echo wrote; else echo refused; fi"), "x\nrefused\n", 0)
}

func TestVolumeInUse(t *testing.T) {
	v := newVolume(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, "-v", v+":/d", alpine, "sh", "-c", "echo ready; sleep 30")
	defer cmd.Process.Kill()

	r := run(t, "-v", v+":/x", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "volume "+v+" is in use") {
		t.Errorf("a second container: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := volumeCmd(t, nil, "", "rm", v); r.code != 1 || !strings.Contains(r.stderr, "in use") {
		t.Errorf("volume rm of a volume in use: exit %d, stderr %q", r.code, r.stderr)
	}
	var info []struct{ InUse bool }
	if r := volumeCmd(t, nil, "", "inspect", v); json.Unmarshal([]byte(r.stdout), &info) != nil || len(info) != 1 || !info[0].InUse {
		t.Errorf("volume inspect of a volume in use: %q", r.stdout)
	}
	interrupt()
	if code := exitCode(cmd.Wait()); code != 130 {
		t.Errorf("exit %d after Ctrl+Break, want 130", code)
	}
	expect(t, run(t, "-v", v+":/x", alpine, "true"), "", 0)
}

// TestVolumeAfterHardStop stops the VM at once, in the middle of writes:
// the next run replays the journal, and what was synced is there.
func TestVolumeAfterHardStop(t *testing.T) {
	v := newVolume(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, "-v", v+":/d", alpine, "sh", "-c",
		`trap "" INT; for i in $(seq 50); do echo $i > /d/f$i; done; sync; echo ready; while :; do echo x >> /d/log; done`)
	interrupt()
	time.Sleep(500 * time.Millisecond)
	interrupt()
	if code := exitCode(cmd.Wait()); code != 130 {
		t.Fatalf("exit %d, want 130; stderr %q", code, stderr.String())
	}
	t.Logf("after a hard stop: %+v", volumeState(t, v))
	expect(t, run(t, "-v", v+":/d", alpine, "sh", "-c", "cat /d/f50; ls /d | grep -c '^f'"), "50\n50\n", 0)
	if s := volumeState(t, v); !s.Clean {
		t.Errorf("after the next run the volume is %+v, want clean", s)
	}
}

func TestAnonymousVolume(t *testing.T) {
	dir := shared(t, map[string]string{"main.js": "1"})
	r := run(t, "-v", dir+":/app", "-v", "/app/node_modules", alpine, "sh", "-c",
		"echo x > /app/node_modules/dep; ls /app/node_modules; ls /app | tr '\n' ' '")
	expect(t, r, "dep\nmain.js node_modules ", 0)
	ents, err := os.ReadDir(filepath.Join(dir, "node_modules"))
	if err != nil || len(ents) != 0 {
		t.Errorf("the anonymous volume's files reached the host: %v, %v", ents, err)
	}
	// From the image, and gone with the run.
	expect(t, run(t, "-v", "/etc", alpine, "sh", "-c", "cat /etc/alpine-release >/dev/null && echo seen; echo y > /etc/new"), "seen\n", 0)
	expect(t, run(t, "-v", "/etc", alpine, "ls", "/etc/new"), "", 1)
	expect(t, run(t, "--mount", "type=volume,target=/usr/share,volume-nocopy", alpine, "sh", "-c", "ls -A /usr/share | wc -l"), "0\n", 0)
}

func TestTmpfs(t *testing.T) {
	expect(t, run(t, "--tmpfs", "/t", alpine, "sh", "-c", "cp /bin/busybox /t/bb && /t/bb true 2>/dev/null; echo $?; grep ' /t ' /proc/mounts | grep -o 'nosuid,nodev,noexec'"),
		"126\nnosuid,nodev,noexec\n", 0)
	expect(t, run(t, "--tmpfs", "/t:size=1m", alpine, "sh", "-c", "dd if=/dev/zero of=/t/f bs=1k count=2048 2>/dev/null || echo full"), "full\n", 0)
	expect(t, run(t, "--tmpfs", "/t:exec", alpine, "sh", "-c", "cp /bin/busybox /t/busybox && /t/busybox echo ran"), "ran\n", 0)
	expect(t, run(t, "--mount", "type=tmpfs,target=/t,tmpfs-mode=700", alpine, "stat", "-c", "%a", "/t"), "700\n", 0)
	r := run(t, "--tmpfs", "/t:huge=1", alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "not supported") {
		t.Errorf("a bad tmpfs option: exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestMountsNested mounts one of each kind inside another.
func TestMountsNested(t *testing.T) {
	dir := shared(t, nil)
	v := newVolume(t)
	expect(t, run(t, "-v", dir+":/a", "-v", v+":/a/v", "--tmpfs", "/a/v/t", "-v", "/a/anon", alpine, "sh", "-c",
		"echo h > /a/h; echo v > /a/v/v; echo t > /a/v/t/t; echo n > /a/anon/n; cat /a/h /a/v/v /a/v/t/t /a/anon/n"), "h\nv\nt\nn\n", 0)
	if got := readHost(t, filepath.Join(dir, "h")); got != "h\n" {
		t.Errorf("host file holds %q", got)
	}
	for _, sub := range []string{"v", "anon"} {
		if ents, _ := os.ReadDir(filepath.Join(dir, sub)); len(ents) != 0 {
			t.Errorf("the host's %s holds %d entries, want an empty mount point", sub, len(ents))
		}
	}
	expect(t, run(t, "-v", v+":/x", alpine, "sh", "-c", "ls /x | tr '\n' ' '; ls -A /x/t"), "t v ", 0)
}

func TestTooManyVolumes(t *testing.T) {
	args := []string{}
	for i := range 60 {
		args = append(args, "-v", fmt.Sprintf("e2e-never-made-%d:/v%d", i, i))
	}
	r := run(t, append(args, alpine, "true")...)
	if r.code != 125 || !strings.Contains(r.stderr, "too many devices") || !strings.Contains(r.stderr, "60 volumes") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(storeDir(t), "volumes", "e2e-never-made-0")); !os.IsNotExist(err) {
		t.Errorf("a volume was made for a run that could not start: %v", err)
	}
}

func TestVolumeCommands(t *testing.T) {
	v := newVolume(t)
	expect(t, volumeCmd(t, nil, "", "create", v), v+"\n", 0)
	if r := volumeCmd(t, nil, "", "create", v); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Errorf("creating it again: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := volumeCmd(t, nil, "", "create", "_bad"); r.code != 1 || !strings.Contains(r.stderr, "invalid volume name") {
		t.Errorf("creating _bad: exit %d, stderr %q", r.code, r.stderr)
	}
	small := newVolume(t)
	expect(t, volumeCmd(t, nil, "", "create", "--opt", "size=2g", small), small+"\n", 0)
	if r := volumeCmd(t, nil, "", "ls"); !strings.HasPrefix(r.stdout, "NAME") || !strings.Contains(r.stdout, small) || r.code != 0 {
		t.Errorf("volume ls: exit %d, stdout %q", r.code, r.stdout)
	}
	var info []struct {
		Name   string
		Size   int64
		OnDisk int64
		InUse  bool
	}
	r := volumeCmd(t, nil, "", "inspect", v, small)
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil || len(info) != 2 || info[0].Size != 64<<30 || info[1].Size != 2<<30 ||
		info[0].InUse || info[0].OnDisk > 8<<20 { // NTFS allocates sparse files 64 KiB at a time
		t.Errorf("volume inspect: %+v, %v", info, err)
	}
	expect(t, volumeCmd(t, nil, "", "rm", v, small), v+"\n"+small+"\n", 0)
	if r := volumeCmd(t, nil, "", "inspect", v); r.code != 1 || !strings.Contains(r.stderr, "no such volume") {
		t.Errorf("inspecting a removed volume: exit %d, stderr %q", r.code, r.stderr)
	}

	// prune, in a store of its own, so the user's volumes are safe.
	env := []string{"RUNWHP_ROOT=" + t.TempDir()}
	volumeCmd(t, env, "", "create", "one")
	volumeCmd(t, env, "", "create", "two")
	if r := volumeCmd(t, env, "", "prune"); r.code != 0 || !strings.Contains(r.stdout, "go with their containers") {
		t.Errorf("prune without -a: exit %d, stdout %q", r.code, r.stdout)
	}
	expect(t, volumeCmd(t, env, "n\n", "prune", "-a"), "", 0)
	if r := volumeCmd(t, env, "", "ls", "-q"); r.stdout != "one\ntwo\n" {
		t.Errorf("after declining prune: %q", r.stdout)
	}
	if r := volumeCmd(t, env, "y\n", "prune", "-a"); r.code != 0 || !strings.Contains(r.stdout, "Deleted Volumes:\none\ntwo\n") {
		t.Errorf("prune -a: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := volumeCmd(t, env, "", "ls", "-q"); r.stdout != "" {
		t.Errorf("after prune -a: %q", r.stdout)
	}
	if r := volumeCmd(t, nil, "", "frobnicate"); r.code != 2 {
		t.Errorf("an unknown volume command: exit %d", r.code)
	}
}
