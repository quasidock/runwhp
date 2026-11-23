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
	ownKernel(t)
	r := run(t, alpine, "uname", "-r")
	if r.code != 0 || !strings.HasSuffix(r.stdout, "-runwhp\n") {
		t.Errorf("uname -r: %q, exit %d, stderr %q", r.stdout, r.code, r.stderr)
	}
}

// TestKernelFeatures probes for what kernel/runwhp.config builds in for
// containers and for later work.
func TestKernelFeatures(t *testing.T) {
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

// TestKernelClock checks that the guest's wall clock starts at the host's,
// which the kernel reads from the VMM's CMOS stub.
func TestKernelClock(t *testing.T) {
	r := run(t, alpine, "date", "+%s")
	guest, err := strconv.ParseInt(strings.TrimSpace(r.stdout), 10, 64)
	if r.code != 0 || err != nil {
		t.Fatalf("date: %q, exit %d, stderr %q", r.stdout, r.code, r.stderr)
	}
	if d := time.Now().Unix() - guest; d < -5 || d > 5 {
		t.Errorf("guest clock is %d s off the host's", d)
	}
}

// TestKernelFetch starts from an empty store: runwhp fetches its kernel,
// does not fetch it under --pull never, and runwhp pull brings it back.
func TestKernelFetch(t *testing.T) {
	ownKernel(t)
	if os.Getenv("RUNWHP_KERNEL") != "" {
		t.Skip("RUNWHP_KERNEL boots a local kernel")
	}
	root := t.TempDir()
	in := func(args ...string) result {
		t.Helper()
		cmd := exec.Command(runwhp(t), args...)
		cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+root)
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

	r := in(runArgs(alpine, "echo", "hi")...)
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
	r = in(runArgs("--pull", "never", alpine, "true")...)
	if r.code != 125 || !strings.Contains(r.stderr, "--pull=never") || !strings.Contains(r.stderr, "runwhp pull") {
		t.Errorf("--pull never without a kernel: exit %d, stderr %q", r.code, r.stderr)
	}

	r = in("pull", alpine)
	if r.code != 0 || !strings.HasPrefix(r.stdout, "sha256:") || !strings.Contains(r.stderr, "pulled kernel") {
		t.Errorf("pull: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	expect(t, in(runArgs("--pull", "never", alpine, "true")...), "", 0)
}
