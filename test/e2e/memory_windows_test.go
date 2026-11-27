//go:build windows

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/vm"
	"github.com/quasidock/runwhp/internal/whp"
)

// The memory tests run in parallel: what they measure, a guest's memory and
// what its own runwhp has committed, is the container's alone.

// defaultMemory is what runwhp gives a guest without -m: half this
// machine's memory.
func defaultMemory() uint64 { return max(vm.MaxMemory()/2&^(2<<20-1), 512<<20) }

// memTotal reads MemTotal, in bytes, from /proc/meminfo output.
func memTotal(t *testing.T, out string) uint64 {
	t.Helper()
	m := regexp.MustCompile(`MemTotal:\s+(\d+) kB`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no MemTotal in %q", out)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n << 10
}

// TestMemoryAboveHole boots a guest with RAM above 4 GiB and moves data
// through it. The kernel fills its Normal zone, the RAM above 4 GiB,
// before the RAM below, so a tmpfs file of 512 MiB lands there.
func TestMemoryAboveHole(t *testing.T) {
	parallel(t)
	const size = 512 << 20
	script := fmt.Sprintf(`
grep MemTotal /proc/meminfo
grep "System RAM" /proc/iomem
normal() { awk '/zone +Normal/ { z = 1 } z && /pages free/ { print $3; exit }' /proc/zoneinfo; }
echo before $(normal)
head -c %d /dev/urandom | tee /t/f | md5sum
md5sum </t/f
echo after $(normal)`, size)
	r := run(t, "-m", "6g", "--tmpfs", "/t:size=1g", alpine, "sh", "-c", script)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if total := memTotal(t, r.stdout); total < 6<<30*96/100 || total > 6<<30 {
		t.Errorf("MemTotal is %d MiB with -m 6g", total>>20)
	}
	if !regexp.MustCompile(`(?m)^100000000-[0-9a-f]+ : System RAM$`).MatchString(r.stdout) {
		t.Errorf("no System RAM from 4 GiB in /proc/iomem:\n%s", r.stdout)
	}
	sums := regexp.MustCompile(`(?m)^([0-9a-f]{32})  -$`).FindAllStringSubmatch(r.stdout, -1)
	if len(sums) != 2 || sums[0][1] != sums[1][1] {
		t.Errorf("checksums of what went in and what came out: %q", sums)
	}
	var before, after uint64
	for _, l := range strings.Split(r.stdout, "\n") {
		fmt.Sscanf(l, "before %d", &before)
		fmt.Sscanf(l, "after %d", &after)
	}
	if before < after || (before-after)*4096 < size*3/4 {
		t.Errorf("the Normal zone went from %d to %d free pages: the file is not above 4 GiB", before, after)
	}
}

// TestMemoryDefault checks that a guest gets half this machine's memory
// unless -m says otherwise, and that a kept container records it.
func TestMemoryDefault(t *testing.T) {
	parallel(t)
	want := defaultMemory()
	r := run(t, alpine, "grep", "MemTotal", "/proc/meminfo")
	if total := memTotal(t, r.stdout); total < want*95/100 || total > want {
		t.Errorf("MemTotal is %d MiB; want about %d MiB", total>>20, want>>20)
	}

	name := newName(t)
	if r := cli(t, keepArgs("--name", name, alpine, "true")...); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	if got := stateOf(t, name).Memory; got != want {
		t.Errorf("state records %d bytes of memory; want %d", got, want)
	}
}

func TestMemoryTooLarge(t *testing.T) {
	parallel(t)
	r := run(t, "-m", fmt.Sprintf("%dm", vm.MaxMemory()>>20+2), alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "this machine's memory") {
		t.Errorf("more memory than the machine has: %+v", r)
	}
}

// TestMemoryCommittedLazily checks that a guest takes from Windows only
// what it touches: its kernel's page descriptors, 1/64 of its RAM, and
// what it uses besides.
func TestMemoryCommittedLazily(t *testing.T) {
	parallel(t)
	r := run(t, "--debug", alpine, "true")
	m := regexp.MustCompile(`memory: (\d+) MiB, committed peak (\d+) MiB`).FindStringSubmatch(r.stderr)
	if r.code != 0 || m == nil {
		t.Fatalf("exit %d, no memory statistics in:\n%s", r.code, r.stderr)
	}
	total, _ := strconv.ParseUint(m[1], 10, 64)
	peak, _ := strconv.ParseUint(m[2], 10, 64)
	limit := total/64 + 128
	if otherKernel() {
		limit += 64 // WSL's kernel, for one, takes more of its own
	}
	if peak > limit {
		t.Errorf("a %d MiB guest committed %d MiB to run true; want at most %d", total, peak, limit)
	}
	if !strings.Contains(r.stderr, "memfault=") {
		t.Error("no memory faults counted")
	}
}

// commitOf reports the memory Windows has committed to process pid.
func commitOf(t *testing.T, pid int) uint64 {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening the container's runwhp, pid %d: %v", pid, err)
	}
	defer windows.CloseHandle(h)
	info, err := whp.ProcessMemory(syscall.Handle(h))
	if err != nil {
		t.Fatal(err)
	}
	return info.Commit
}

// needBalloon skips a test of giving memory back when the guest's kernel
// has no balloon driver, as WSL's has not.
func needBalloon(t *testing.T, name string) {
	t.Helper()
	if r := execIn(t, name, "test", "-d", "/sys/bus/virtio/drivers/virtio_balloon"); r.code != 0 {
		t.Skip("the guest's kernel has no virtio balloon driver, so it cannot give memory back")
	}
}

// TestMemoryGivenBack fills 1.5 GiB of a running container's memory and
// frees it, and checks that its runwhp process takes the memory from
// Windows and gives it back.
func TestMemoryGivenBack(t *testing.T) {
	parallel(t)
	name := newName(t)
	detached(t, "--name", name, "-m", "3g", "--tmpfs", "/t:size=2g", alpine, "sleep", "300")
	needBalloon(t, name)
	pid := stateOf(t, name).Pid
	before := commitOf(t, pid)
	if r := execIn(t, name, "sh", "-c", "head -c 1610612736 /dev/zero >/t/f"); r.code != 0 {
		t.Fatalf("filling memory: %+v", r)
	}
	full := commitOf(t, pid)
	if full < before+1400<<20 {
		t.Fatalf("commit went from %d to %d MiB filling 1.5 GiB", before>>20, full>>20)
	}
	if r := execIn(t, name, "rm", "/t/f"); r.code != 0 {
		t.Fatalf("freeing memory: %+v", r)
	}
	start := time.Now()
	var now uint64
	for now = commitOf(t, pid); now > full-1200<<20; now = commitOf(t, pid) {
		if time.Since(start) > 60*time.Second {
			t.Fatalf("commit was %d MiB before, %d full, and is still %d a minute after freeing it", before>>20, full>>20, now>>20)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("commit %d MiB before, %d full, %d %.1f s after freeing", before>>20, full>>20, now>>20, time.Since(start).Seconds())
}

// TestMemoryReclaimedWhenIdle writes 1 GiB to a container's writable
// layer, which leaves it in the guest's page cache, and checks that an
// idle guest gives the cache back, unless RUNWHP_RECLAIM_IDLE=0 says not
// to. The two containers run at once.
func TestMemoryReclaimedWhenIdle(t *testing.T) {
	parallel(t)
	cached := func(t *testing.T, name string) uint64 {
		t.Helper()
		r := execIn(t, name, "grep", "^Cached:", "/proc/meminfo")
		var kb uint64
		if _, err := fmt.Sscanf(r.stdout, "Cached: %d kB", &kb); err != nil {
			t.Fatalf("reading Cached: %v in %+v", err, r)
		}
		return kb << 10
	}
	start := func(t *testing.T, idle string) (string, int) {
		t.Helper()
		setEnv(t, "RUNWHP_RECLAIM_IDLE", idle)
		name := newName(t)
		detached(t, "--name", name, "-m", "3g", alpine, "sleep", "300")
		needBalloon(t, name)
		if r := execIn(t, name, "sh", "-c", "head -c 1073741824 /dev/zero >/root/f && sync"); r.code != 0 {
			t.Fatalf("writing 1 GiB: %+v", r)
		}
		return name, stateOf(t, name).Pid
	}

	concurrently(t,
		subtest{"reclaimed", func(t *testing.T) {
			name, pid := start(t, "2")
			full := commitOf(t, pid)
			deadline := time.Now().Add(60 * time.Second)
			for c := cached(t, name); c > 400<<20; c = cached(t, name) {
				if time.Now().After(deadline) {
					t.Fatalf("the guest still caches %d MiB after a minute idle", c>>20)
				}
				time.Sleep(500 * time.Millisecond)
			}
			for now := commitOf(t, pid); now > full-500<<20; now = commitOf(t, pid) {
				if time.Now().After(deadline) {
					t.Fatalf("commit was %d MiB with the cache full and is still %d", full>>20, now>>20)
				}
				time.Sleep(500 * time.Millisecond)
			}
		}},
		subtest{"off", func(t *testing.T) {
			name, _ := start(t, "0")
			time.Sleep(15 * time.Second)
			if c := cached(t, name); c < 900<<20 {
				t.Errorf("with RUNWHP_RECLAIM_IDLE=0, the cache went down to %d MiB", c>>20)
			}
		}},
	)
}
