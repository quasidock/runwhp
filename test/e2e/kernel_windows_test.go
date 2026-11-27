//go:build windows

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// otherKernel reports whether RUNWHP_E2E_RUN_FLAGS boots another kernel
// than runwhp's.
func otherKernel() bool {
	for _, f := range runArgs() {
		if strings.HasPrefix(strings.TrimLeft(f, "-"), "kernel") {
			return true
		}
	}
	return false
}

// ownKernel skips tests of runwhp's kernel when RUNWHP_E2E_RUN_FLAGS boots
// another one.
func ownKernel(t *testing.T) {
	t.Helper()
	if otherKernel() {
		t.Skip("RUNWHP_E2E_RUN_FLAGS boots another kernel")
	}
}

func TestKernelRelease(t *testing.T) {
	parallel(t)
	ownKernel(t)
	r := run(t, alpine, "uname", "-r")
	if r.code != 0 || !strings.HasSuffix(r.stdout, "-runwhp\n") {
		t.Errorf("uname -r: %q, exit %d, stderr %q", r.stdout, r.code, r.stderr)
	}
}

// TestKernelFeatures probes for what kernel/runwhp.config builds in for
// containers and for later work.
func TestKernelFeatures(t *testing.T) {
	parallel(t)
	ownKernel(t)
	script := `
fail() { echo "$*"; exit 1; }
for ns in cgroup ipc mnt net pid user uts; do
	test -e /proc/self/ns/$ns || fail "no $ns namespace"
done
for fs in cgroup2 overlay tmpfs ext4 fuse 9p virtiofs binfmt_misc; do
	grep -qw $fs /proc/filesystems || fail "no $fs filesystem"
done
grep -q '^Seccomp:' /proc/self/status || fail "no seccomp"
grep -qw vsock /proc/misc || fail "no vsock"
test -e /proc/net/if_inet6 || fail "no IPv6"
ip -6 addr show dev lo | grep -q '::1/128' || fail "no ::1 on lo"
for opt in IKCONFIG_PROC IA32_EMULATION NF_TABLES IP_NF_IPTABLES_LEGACY VIRTIO_BALLOON; do
	zcat /proc/config.gz | grep -qx "CONFIG_$opt=y" || fail "CONFIG_$opt is not built in"
done
zcat /proc/config.gz | grep -qx '# CONFIG_MODULES is not set' || fail "modules are enabled"
unshare -Urpf --mount-proc sh -c 'test $$ = 1 && test "$(id -u)" = 0' || fail "unshare failed"
printf '#!/bin/sh\necho script\n' >/tmp/s && chmod +x /tmp/s && test "$(/tmp/s)" = script || fail "no #! scripts"
# Deleting a lower directory and making it again needs an opaque directory
# on the tmpfs upper layer, which takes xattrs.
rm -rf /etc/apk && mkdir /etc/apk && touch /etc/apk/x && test "$(ls /etc/apk)" = x || fail "no opaque directories"
echo ok
`
	r := run(t, alpine, "sh", "-c", script)
	expect(t, r, "ok\n", 0)
}

// guestClock reads the guest's wall clock, to the microsecond, from what
// busybox's adjtimex prints. busybox's date has whole seconds only.
func guestClock(t *testing.T, r result) time.Time {
	t.Helper()
	var sec, usec int64
	var okSec, okUsec bool
	for _, line := range strings.Split(r.stdout, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "time.tv_sec:":
			sec, _ = strconv.ParseInt(f[1], 10, 64)
			okSec = true
		case "time.tv_usec:":
			usec, _ = strconv.ParseInt(f[1], 10, 64)
			okUsec = true
		}
	}
	if !okSec || !okUsec {
		t.Fatalf("adjtimex: %+v", r)
	}
	return time.Unix(sec, usec*1000)
}

// TestKernelClock checks that the guest's wall clock is the host's, within
// the time the message bringing it takes: the kernel boots with the CMOS
// stub's whole seconds, and the host sends init its time as it starts.
func TestKernelClock(t *testing.T) {
	parallel(t)
	const slack = 50 * time.Millisecond
	before := time.Now()
	r := run(t, alpine, "adjtimex")
	after := time.Now()
	if guest := guestClock(t, r); guest.Before(before.Add(-slack)) || guest.After(after.Add(slack)) {
		t.Errorf("guest clock %v, outside the run's %v to %v", guest.Format(time.StampMicro),
			before.Format(time.StampMicro), after.Format(time.StampMicro))
	}
}

// TestClockResync checks that the guest's clock is brought back to the
// host's while the container runs, and is left alone when
// RUNWHP_CLOCK_SYNC=0 turns that off.
func TestClockResync(t *testing.T) {
	offset := func(name string) time.Duration {
		t.Helper()
		before := time.Now()
		r := execIn(t, name, "adjtimex")
		after := time.Now()
		return guestClock(t, r).Sub(before.Add(after.Sub(before) / 2))
	}
	setEnv(t, "RUNWHP_CLOCK_SYNC", "1")
	name := running(t)
	if r := execIn(t, name, "date", "-s", "2020-01-01 00:00:00"); r.code != 0 {
		t.Fatalf("date -s: %+v", r)
	}
	var d time.Duration
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if d = offset(name); d > -500*time.Millisecond && d < 500*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest clock still %v off the host's 5 s after it was set back", d)
		}
	}

	setEnv(t, "RUNWHP_CLOCK_SYNC", "0")
	name = running(t)
	execIn(t, name, "date", "-s", "2020-01-01 00:00:00")
	time.Sleep(2 * time.Second)
	if d := offset(name); d > -24*time.Hour {
		t.Errorf("guest clock set back with syncing off was brought to %v off the host's", d)
	}
}

// TestACPIPowerOff checks that the kernel takes the ACPI tables' soft-off:
// the sleep registers and \_S5. The container cannot power off itself, so
// the kernel's boot message is the test.
func TestACPIPowerOff(t *testing.T) {
	parallel(t)
	ownKernel(t)
	r := run(t, "--debug", alpine, "true")
	if r.code != 0 || !strings.Contains(r.stderr, "ACPI: PM: (supports S0 S5)") {
		t.Errorf("exit %d; the kernel does not offer S5:\n%s", r.code, r.stderr)
	}
}

// TestKernelFetch starts from an empty store: runwhp fetches its kernel,
// does not fetch it under --pull never, and runwhp pull brings it back.
func TestKernelFetch(t *testing.T) {
	parallel(t)
	ownKernel(t)
	if os.Getenv("RUNWHP_KERNEL") != "" {
		t.Skip("RUNWHP_KERNEL boots a local kernel")
	}
	root := t.TempDir()
	in := func(args ...string) result {
		t.Helper()
		cmd := command(t, args...)
		cmd.Env = append(cmd.Env, "RUNWHP_ROOT="+root)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return result{stdout.String(), stderr.String(), code}
	}
	kernels := filepath.Join(root, "kernels")

	img := mirrored(t, alpine)
	r := in(runArgs(img, "echo", "hi")...)
	expect(t, r, "hi\n", 0)
	if !strings.Contains(r.stderr, "pulling kernel ") || !strings.HasPrefix(r.stderr, "pulling") {
		t.Errorf("stderr: %q", r.stderr)
	}
	found, _ := filepath.Glob(filepath.Join(kernels, "sha256", "*", "bzImage"))
	if len(found) != 1 {
		t.Errorf("store holds kernels %v", found)
	}
	if _, err := os.Stat(filepath.Join(root, "modules")); err == nil {
		t.Error("the store has a modules directory")
	}

	if err := os.RemoveAll(kernels); err != nil {
		t.Fatal(err)
	}
	r = in(runArgs("--pull", "never", img, "true")...)
	if r.code != 125 || !strings.Contains(r.stderr, "--pull=never") || !strings.Contains(r.stderr, "runwhp pull") {
		t.Errorf("--pull never without a kernel: exit %d, stderr %q", r.code, r.stderr)
	}

	r = in("pull", img)
	if r.code != 0 || !strings.HasPrefix(r.stdout, "sha256:") || !strings.Contains(r.stderr, "pulled kernel") {
		t.Errorf("pull: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	expect(t, in(runArgs("--pull", "never", img, "true")...), "", 0)
}
