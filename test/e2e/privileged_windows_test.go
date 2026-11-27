//go:build windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCgroupView checks the container's cgroup as Docker shows it: the
// root of a cgroup namespace of its own at /sys/fs/cgroup, read-only, for
// the container's process and an exec's alike.
func TestCgroupView(t *testing.T) {
	r := run(t, alpine, "sh", "-c", `cat /proc/self/cgroup; grep " /sys/fs/cgroup " /proc/self/mountinfo | cut -d' ' -f4,6; touch /sys/fs/cgroup/x 2>/dev/null && echo writable; mount | grep -c " /sys .*ro,"; ls /dev/fuse 2>/dev/null; true`)
	if r.code != 0 || r.stdout != "0::/\n/ ro,nosuid,nodev,noexec,relatime\n1\n" {
		t.Errorf("an unprivileged container's cgroup: %+v", r)
	}
	name := newName(t)
	detached(t, "--name", name, alpine, "sleep", "1d")
	if r := cli(t, "exec", name, "cat", "/proc/self/cgroup"); r.stdout != "0::/\n" {
		t.Errorf("exec's cgroup: %+v", r)
	}
}

// TestPrivileged checks --privileged: the cgroup read-write with the
// controllers to delegate, /sys read-write, FUSE, TUN and kmsg; and, once
// the container has moved its processes below its cgroup, as docker:dind
// does, execs starting in the container process's cgroup, and pause
// freezing them all.
func TestPrivileged(t *testing.T) {
	name := newName(t)
	detached(t, "--privileged", "--name", name, alpine, "sh", "-c", `
cat /sys/fs/cgroup/cgroup.controllers
mkdir /sys/fs/cgroup/init && echo $$ > /sys/fs/cgroup/init/cgroup.procs
echo "+memory +pids" > /sys/fs/cgroup/cgroup.subtree_control && cat /sys/fs/cgroup/cgroup.subtree_control
ls /dev/fuse /dev/net/tun /dev/kmsg
mount | grep " /sys " | grep -q "(rw," && echo sys-rw
echo ready
i=0; while :; do i=$((i+1)); echo $i > /tmp/count; sleep 0.05; done`)
	waitLog(t, name, "ready\n")
	want := "cpuset cpu io memory pids\nmemory pids\n/dev/fuse\n/dev/kmsg\n/dev/net/tun\nsys-rw\nready\n"
	if r := cli(t, "logs", name); r.stdout != want {
		t.Errorf("the privileged container printed %q, want %q", r.stdout, want)
	}
	if r := cli(t, "exec", name, "cat", "/proc/self/cgroup"); r.code != 0 || r.stdout != "0::/init\n" {
		t.Errorf("an exec once the container's cgroup delegates: %+v", r)
	}
	expect(t, cli(t, "pause", name), name+"\n", 0)
	count := func() string {
		dst := filepath.Join(t.TempDir(), "count")
		if r := cli(t, "cp", name+":/tmp/count", dst); r.code != 0 {
			t.Fatalf("cp: %+v", r)
		}
		data, _ := os.ReadFile(dst)
		return strings.TrimSpace(string(data))
	}
	before := count()
	time.Sleep(300 * time.Millisecond)
	if after := count(); after != before {
		t.Errorf("paused, but the count went from %q to %q", before, after)
	}
	expect(t, cli(t, "unpause", name), name+"\n", 0)
}
