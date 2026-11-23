//go:build windows

package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runWithin is run with a deadline, for containers that might stop their VM
// in a way runwhp would not notice: past it, runwhp is killed and the test
// fails.
func runWithin(t *testing.T, limit time.Duration, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, runwhp(t), runArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("still running after %v; stdout %q, stderr %q", limit, stdout.String(), stderr.String())
	}
	return result{stdout.String(), stderr.String(), exitCode(err)}
}

// capSysBoot reports, from a container, whether CAP_SYS_BOOT (bit 22) is
// in its shell's bounding set.
const capSysBoot = `echo bounding $(( 0x$(awk '/^CapBnd/{print $2}' /proc/self/status) >> 22 & 1 ))`

// TestNoReboot checks that the container cannot reset, halt or power off
// its VM itself, as a Docker container cannot: it lacks CAP_SYS_BOOT, and
// each fails with EPERM while the container carries on.
func TestNoReboot(t *testing.T) {
	script := `
for c in 'reboot -f' 'halt -f' 'poweroff -f'; do
	out=$($c 2>&1); echo "$c: $? $out"
done
` + capSysBoot + `
exit 3`
	r := runWithin(t, 30*time.Second, alpine, "sh", "-c", script)
	if r.code != 3 {
		t.Errorf("exit %d, want the container's own 3; stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for _, c := range []string{"reboot -f", "halt -f", "poweroff -f"} {
		if !strings.Contains(r.stdout, c+": 1 ") || !strings.Contains(r.stdout, "not permitted") {
			t.Errorf("%s did not fail with EPERM:\n%s", c, r.stdout)
		}
	}
	if !strings.Contains(r.stdout, "bounding 0\n") {
		t.Errorf("the container has CAP_SYS_BOOT:\n%s", r.stdout)
	}

	// Nor does a process runwhp exec starts.
	name := running(t)
	expect(t, execIn(t, name, "sh", "-c", capSysBoot), "bounding 0\n", 0)
}

// TestSignalInit checks that what the container sends to PID 1, init,
// leaves it running: busybox's reboot, halt and poweroff without -f signal
// it, and SIGTERM used to end it and panic the kernel.
func TestSignalInit(t *testing.T) {
	script := `
for s in TERM INT HUP USR1 USR2 PWR; do kill -$s 1 || echo "kill -$s failed"; done
reboot; halt; poweroff
sleep 1
echo alive`
	r := runWithin(t, 30*time.Second, alpine, "sh", "-c", script)
	expect(t, r, "alive\n", 0)
}
