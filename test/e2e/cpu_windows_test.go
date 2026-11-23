//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// debian has taskset, which alpine's busybox lacks.
const debian = "debian:12"

// cpus returns n, or skips the test if this machine has fewer logical
// processors, which --cpus refuses.
func cpus(t *testing.T, n int) string {
	t.Helper()
	if runtime.NumCPU() < n {
		t.Skipf("needs %d logical processors; this machine has %d", n, runtime.NumCPU())
	}
	return strconv.Itoa(n)
}

// TestCPUCount checks how many processors the guest sees, and that it sees
// them as cores of one package sharing a cache, as runwhp describes them.
func TestCPUCount(t *testing.T) {
	const script = `nproc; grep -c ^processor /proc/cpuinfo
cat /sys/devices/system/cpu/cpu0/topology/package_cpus_list /sys/devices/system/cpu/cpu0/topology/core_cpus_list`
	check := func(r result, n int) {
		t.Helper()
		last := "0"
		if n > 1 {
			last = fmt.Sprintf("0-%d", n-1)
		}
		expect(t, r, fmt.Sprintf("%d\n%d\n%s\n0\n", n, n, last), 0)
	}
	for _, n := range []int{1, 2, 4} {
		check(run(t, "--cpus", cpus(t, n), alpine, "sh", "-c", script), n)
	}
	for _, f := range runArgs() {
		if strings.HasPrefix(strings.TrimLeft(f, "-"), "cpus") {
			return // RUNWHP_E2E_RUN_FLAGS replaces the default
		}
	}
	check(run(t, alpine, "sh", "-c", script), min(runtime.NumCPU(), 4))
}

func TestCPUFlag(t *testing.T) {
	for in, msg := range map[string]string{
		"0":   "between 1 and",
		"1.5": "whole number",
		"65":  "between 1 and",
	} {
		r := run(t, "--cpus", in, alpine, "true")
		if r.code != 125 || !strings.Contains(r.stderr, msg) {
			t.Errorf("--cpus %s: exit %d, stderr %q; want 125 and %q", in, r.code, r.stderr, msg)
		}
	}
}

// TestCPUPinned runs a process on each CPU in turn.
func TestCPUPinned(t *testing.T) {
	r := run(t, "--cpus", cpus(t, 4), debian, "sh", "-c",
		`for c in 0 1 2 3; do taskset -c $c cut -d' ' -f39 /proc/self/stat; done`)
	expect(t, r, "0\n1\n2\n3\n", 0)
}

// TestCPUSpread runs four busy loops, unpinned, and checks that the
// scheduler gave each CPU a fair share of them.
func TestCPUSpread(t *testing.T) {
	const script = `busy() { awk '/^cpu[0-9]/ { print $2 + $3 + $4 }' /proc/stat; }
before=$(busy)
for i in 1 2 3 4; do timeout 2 sh -c 'while :; do :; done' & done; wait
after=$(busy)
echo $before $after`
	r := run(t, "--cpus", cpus(t, 4), alpine, "sh", "-c", script)
	f := strings.Fields(r.stdout)
	if r.code != 0 || len(f) != 8 {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for cpu := range 4 {
		before, _ := strconv.Atoi(f[cpu])
		after, _ := strconv.Atoi(f[4+cpu])
		// /proc/stat counts in USER_HZ, 100 a second, so two seconds are
		// 200 ticks; a CPU that ran a loop for most of them got its share.
		if busy := after - before; busy < 140 {
			t.Errorf("CPU %d was busy for %d ticks of 200", cpu, busy)
		}
	}
}

// TestCPUExit checks the container's exit code and a reset requested from a
// CPU other than the first, which is how Linux reboots.
func TestCPUExit(t *testing.T) {
	expect(t, run(t, "--cpus", cpus(t, 4), debian, "taskset", "-c", "3", "sh", "-c", "echo out; exit 3"), "out\n", 3)

	start := time.Now()
	r := run(t, "--cpus", "4", "python:3.12-slim", "taskset", "-c", "2", "python3", "-c",
		"import ctypes; ctypes.CDLL(None).reboot(0x01234567)")
	if r.code != 125 || !strings.Contains(r.stderr, "guest requested reset") {
		t.Errorf("reboot: exit %d, stderr %q; want 125 and the reset", r.code, r.stderr)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("reboot took %v", d)
	}
}

// TestCPUInterruptDuringBoot sends Ctrl+Break as soon as the VMM is ready,
// while the secondary CPUs still wait to be started, and checks runwhp stops.
func TestCPUInterruptDuringBoot(t *testing.T) {
	n := cpus(t, min(runtime.NumCPU(), 16))
	cmd := exec.Command(runwhp(t), runArgs("--debug", "--cpus", n, alpine, "sleep", "30")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(stderr.String(), "  ready: "); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the VMM never got ready; stderr %q", stderr.String())
		}
	}
	const ctrlBreak = 1
	if r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent").Call(ctrlBreak, uintptr(cmd.Process.Pid)); r == 0 {
		cmd.Process.Kill()
		t.Skipf("cannot send Ctrl+Break here (no shared console?): %v", err)
	}
	start := time.Now()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if code := exitCode(err); code != 130 {
			t.Errorf("exit = %d, want 130", code)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("still running 10 s after Ctrl+Break; stderr tail %q", tail(stderr.String(), 2000))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// TestCPUHotplug takes a CPU offline and brings it back, which goes through
// INIT and SIPI again.
func TestCPUHotplug(t *testing.T) {
	r := run(t, "--cpus", cpus(t, 4), alpine, "sh", "-c", `mount -o remount,rw /sys
echo 0 > /sys/devices/system/cpu/cpu2/online; cat /sys/devices/system/cpu/online
echo 1 > /sys/devices/system/cpu/cpu2/online; cat /sys/devices/system/cpu/online`)
	expect(t, r, "0-1,3\n0-3\n", 0)
}

// TestCPUBindConcurrent writes four files to a bind mount at once.
func TestCPUBindConcurrent(t *testing.T) {
	dir := shared(t, nil)
	r := run(t, "--cpus", cpus(t, 4), "-v", dir+":/s", alpine, "sh", "-c",
		`for i in 1 2 3 4; do (head -c 20971520 /dev/urandom > /s/f$i; sha256sum /s/f$i | cut -d' ' -f1 > /s/f$i.sum) & done; wait`)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for i := 1; i <= 4; i++ {
		sum := sha256.Sum256([]byte(readHost(t, filepath.Join(dir, fmt.Sprintf("f%d", i)))))
		if got := strings.TrimSpace(readHost(t, filepath.Join(dir, fmt.Sprintf("f%d.sum", i)))); got != hex.EncodeToString(sum[:]) {
			t.Errorf("f%d: container wrote %s, host reads %x", i, got, sum)
		}
	}
}

// TestCPUNetworkConcurrent downloads from the host four times at once.
func TestCPUNetworkConcurrent(t *testing.T) {
	runwhp(t)
	data := make([]byte, 20<<20)
	rand.Read(data)
	sum := sha256.Sum256(data)
	port := hostServer(t, data)
	r := run(t, "--cpus", cpus(t, 4), "--add-host", hostGateway, alpine, "sh", "-c",
		`for i in 1 2 3 4; do wget -q -O- http://host.docker.internal:`+port+`/ | sha256sum & done; wait`)
	want := strings.Repeat(hex.EncodeToString(sum[:])+"  -\n", 4)
	expect(t, r, want, 0)
}

// TestCPUStdoutConcurrent writes to stdout from four processes at once.
func TestCPUStdoutConcurrent(t *testing.T) {
	r := run(t, "--cpus", cpus(t, 4), alpine, "sh", "-c",
		`for i in 1 2 3 4; do head -c 5000000 /dev/zero & done; wait`)
	if r.code != 0 || len(r.stdout) != 20000000 || bytes.Count([]byte(r.stdout), []byte{0}) != 20000000 {
		t.Errorf("exit %d, %d bytes of stdout; want 20000000 zeros; stderr %q", r.code, len(r.stdout), r.stderr)
	}
}

// TestMmioFastPath checks that the VMM serves nearly every device register
// access itself rather than through WinHvEmulation.dll, which is several
// times slower: the guest's kernel reaches its devices with plain movs.
func TestMmioFastPath(t *testing.T) {
	r := run(t, "--debug", alpine, "sh", "-c", "head -c 1000000 /dev/zero | wc -c")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "1000000" {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
	fast, emulated := -1, 0
	for _, line := range strings.Split(r.stderr, "\n") {
		_, exits, ok := strings.Cut(line, "exits: ")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(exits) {
			name, n, _ := strings.Cut(f, "=")
			v, _ := strconv.Atoi(n)
			switch name {
			case "mmio-fast":
				fast = v
			case "mmio":
				emulated = v
			}
		}
	}
	if fast < 0 {
		t.Fatalf("no mmio-fast count in the VMM's statistics:\n%s", r.stderr)
	}
	if fast < 1000 || emulated*20 > fast+emulated {
		t.Errorf("%d device accesses served by the VMM, %d by the emulator; want under 5%% emulated", fast, emulated)
	}
}
