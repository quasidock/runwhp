//go:build windows

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// storeDir is runwhp's store, as runwhp finds it.
func storeDir(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("RUNWHP_ROOT"); root != "" {
		return root
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "runwhp")
}

// scratchFiles lists the scratch disks in the store.
func scratchFiles(t *testing.T) map[string]bool {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(storeDir(t), "scratch"))
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// newScratchFile waits for a scratch disk that is not in before to appear.
func newScratchFile(t *testing.T, before map[string]bool) string {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for name := range scratchFiles(t) {
			if !before[name] {
				return name
			}
		}
	}
	t.Fatal("no new scratch disk appeared")
	return ""
}

func waitGone(t *testing.T, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); scratchFiles(t)[name]; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("scratch disk %s still there", name)
		}
	}
}

// TestWritableLayerBeyondMemory writes 1 GiB into the container's root with
// 256 MiB of guest memory and reads it back.
func TestWritableLayerBeyondMemory(t *testing.T) {
	h := sha256.New()
	zeros := make([]byte, 1<<20)
	for range 1024 {
		h.Write(zeros)
	}
	want := hex.EncodeToString(h.Sum(nil))
	r := run(t, "-m", "256m", alpine, "sh", "-c",
		"dd if=/dev/zero of=/big bs=1M count=1024 2>/dev/null && sync /big && echo 3 > /proc/sys/vm/drop_caches; sha256sum /big | cut -d' ' -f1")
	expect(t, r, want+"\n", 0)
}

// dfKiB returns the size and free space of the container's root, in KiB.
func dfKiB(t *testing.T, args ...string) (size, avail int64) {
	t.Helper()
	r := run(t, append(args, alpine, "sh", "-c", "df -k / | awk 'NR==2 {print $2, $4}'")...)
	f := strings.Fields(r.stdout)
	if r.code != 0 || len(f) != 2 {
		t.Fatalf("df: %+v", r)
	}
	size, _ = strconv.ParseInt(f[0], 10, 64)
	avail, _ = strconv.ParseInt(f[1], 10, 64)
	return size, avail
}

func TestWritableLayerSize(t *testing.T) {
	if size, _ := dfKiB(t); size < 60<<20 || size > 64<<20 {
		t.Errorf("root is %d KiB by default, want about 64 GiB", size)
	}
	size, _ := dfKiB(t, "--storage-opt", "size=1g")
	if size < 900<<10 || size > 1<<20 {
		t.Errorf("root is %d KiB with size=1g", size)
	}
	r := run(t, "--storage-opt", "size=1g", alpine, "sh", "-c",
		"if dd if=/dev/zero of=/big bs=1M count=1100 2>/dev/null; then echo fit; else echo full; fi")
	expect(t, r, "full\n", 0)

	for _, c := range []struct{ opt, msg string }{
		{"size=10m", "out of range"},
		{"quota=1g", "unknown option"},
	} {
		r := run(t, "--storage-opt", c.opt, alpine, "true")
		if r.code != 125 || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("--storage-opt %s: exit %d, stderr %q", c.opt, r.code, r.stderr)
		}
	}
}

// TestFsyncCheap checks that fsync in the container's root does not wait
// for the host's disk: the scratch disk offers no cache flush, and the
// overlay is volatile.
func TestFsyncCheap(t *testing.T) {
	r := run(t, "--debug", alpine, "sh", "-c", "for i in $(seq 200); do echo $i > /f$i; sync /f$i; done; echo done")
	if r.code != 0 || !strings.Contains(r.stdout, "done") {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
	for _, line := range strings.Split(r.stderr, "\n") {
		if strings.Contains(line, `blk "scratch"`) && !strings.Contains(line, " 0 flushes") {
			t.Errorf("the scratch disk was flushed: %s", line)
		}
	}
}

func TestScratchRemovedAfterRun(t *testing.T) {
	runwhp(t)
	leftover := filepath.Join(storeDir(t), "scratch", "leftover.ext4")
	os.MkdirAll(filepath.Dir(leftover), 0o755)
	if err := os.WriteFile(leftover, []byte("from a run the machine stopped under"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := scratchFiles(t)
	delete(before, "leftover.ext4")
	expect(t, run(t, alpine, "true"), "", 0)
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("a scratch disk left by an earlier run was not removed: %v", err)
	}
	after := scratchFiles(t)
	for name := range after {
		if !before[name] {
			t.Errorf("scratch disk %s left after the run", name)
		}
	}
}

// TestScratchRemovedAfterHardStop stops the VM at once with a second
// Ctrl+Break.
func TestScratchRemovedAfterHardStop(t *testing.T) {
	before := scratchFiles(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptible(t, &stdout, &stderr, alpine, "sh", "-c", `trap "" INT; echo ready; sleep 30`)
	name := newScratchFile(t, before)
	interrupt()
	time.Sleep(500 * time.Millisecond)
	interrupt()
	if code := exitCode(cmd.Wait()); code != 130 {
		t.Errorf("exit %d, want 130; stderr %q", code, stderr.String())
	}
	waitGone(t, name)
}

// TestScratchRemovedWhenKilled kills runwhp, as Task Manager would.
func TestScratchRemovedWhenKilled(t *testing.T) {
	before := scratchFiles(t)
	var stdout syncBuffer
	cmd := exec.Command(runwhp(t), runArgs(alpine, "sh", "-c", "echo ready; sleep 30")...)
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(stdout.String(), "ready"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("container never got ready")
		}
	}
	name := newScratchFile(t, before)
	cmd.Process.Kill()
	cmd.Wait()
	waitGone(t, name)
}
