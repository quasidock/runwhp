//go:build windows

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/vm"
)

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
	r := run(t, "-m", fmt.Sprintf("%dm", vm.MaxMemory()>>20+2), alpine, "true")
	if r.code != 125 || !strings.Contains(r.stderr, "this machine's memory") {
		t.Errorf("more memory than the machine has: %+v", r)
	}
}

// TestMemoryCommittedLazily checks that a guest takes from Windows only
// what it touches: its kernel's page descriptors, 1/64 of its RAM, and
// what it uses besides.
func TestMemoryCommittedLazily(t *testing.T) {
	r := run(t, "--debug", alpine, "true")
	m := regexp.MustCompile(`memory: (\d+) MiB, committed peak (\d+) MiB`).FindStringSubmatch(r.stderr)
	if r.code != 0 || m == nil {
		t.Fatalf("exit %d, no memory statistics in:\n%s", r.code, r.stderr)
	}
	total, _ := strconv.ParseUint(m[1], 10, 64)
	peak, _ := strconv.ParseUint(m[2], 10, 64)
	if limit := total/64 + 128; peak > limit {
		t.Errorf("a %d MiB guest committed %d MiB to run true; want at most %d", total, peak, limit)
	}
	if !strings.Contains(r.stderr, "memfault=") {
		t.Error("no memory faults counted")
	}
}
