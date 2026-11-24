//go:build windows

package e2e

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bootCounter counts the container's runs in its writable layer, which
// each run keeps: it prints "boot N", then runs rest with $n set.
func bootCounter(rest string) string {
	return `n=$(cat /boots 2>/dev/null || echo 0); n=$((n+1)); echo $n > /boots; sync /boots; echo boot $n; ` + rest
}

// waitRestarts waits for a container's state to satisfy ok.
func waitState(t *testing.T, ref string, d time.Duration, what string, ok func(stateJSON) bool) stateJSON {
	t.Helper()
	var s stateJSON
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		if s = stateOf(t, ref); ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s never %s: %+v", ref, what, s)
		}
	}
}

// TestRestartOnFailure runs a container that fails, with on-failure:2: it
// runs three times, and stops with its exit code. One that succeeds runs
// once.
func TestRestartOnFailure(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "50ms")
	name := newName(t)
	detached(t, "--name", name, "--restart", "on-failure:2", alpine, "sh", "-c", bootCounter("exit 3"))
	s := waitStatus(t, name, "stopped", time.Minute)
	if exitCodeOf(s) != 3 || s.RestartCount != 2 || s.RestartPolicy != "on-failure:2" {
		t.Errorf("state %+v", s)
	}
	if r := cli(t, "logs", name); r.stdout != "boot 1\nboot 2\nboot 3\n" {
		t.Errorf("logs: %+v", r)
	}
	if r := cli(t, "wait", name); strings.TrimSpace(r.stdout) != "3" {
		t.Errorf("wait: %+v", r)
	}

	ok := newName(t)
	detached(t, "--name", ok, "--restart", "on-failure", alpine, "sh", "-c", bootCounter("exit 0"))
	s = waitStatus(t, ok, "stopped", time.Minute)
	time.Sleep(500 * time.Millisecond)
	if r := cli(t, "logs", ok); r.stdout != "boot 1\n" || exitCodeOf(stateOf(t, ok)) != 0 {
		t.Errorf("exit 0: %+v, %+v", r, s)
	}

	for _, args := range [][]string{{"--restart", "sometimes"}, {"--restart", "always:2"}} {
		if r := cli(t, append(keepArgs(args...), alpine, "true")...); r.code != 125 || !strings.Contains(r.stderr, "--restart") {
			t.Errorf("run %s: %+v", strings.Join(args, " "), r)
		}
	}
	if r := cli(t, append(runArgs("--restart", "always"), alpine, "true")...); r.code != 125 || !strings.Contains(r.stderr, "cannot specify both --restart and --rm") {
		t.Errorf("run --rm --restart: %+v", r)
	}
}

// TestRestartAlways restarts a container that exits 0 until it runs for
// good, then checks that stop and kill end it for good, until start.
func TestRestartAlways(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "50ms")
	name := newName(t)
	detached(t, "--name", name, "--restart", "always", alpine, "sh", "-c",
		bootCounter(`if [ $n -ge 3 ] && [ $n != 5 ]; then exec sleep 600; fi; exit 0`))
	waitLog(t, name, "boot 3")
	s := waitStatus(t, name, "running", 30*time.Second)
	if s.RestartCount != 2 {
		t.Errorf("after three runs: %+v", s)
	}
	quiet := func(what, not string) {
		t.Helper()
		time.Sleep(time.Second)
		if s := stateOf(t, name); s.Status != "stopped" {
			t.Errorf("%s: %+v", what, s)
		}
		if r := cli(t, "logs", name); strings.Contains(r.stdout, not) {
			t.Errorf("%s: started again: %q", what, r.stdout)
		}
	}

	// stop: no fourth run.
	if r := cli(t, "stop", "-t", "1", name); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	quiet("after stop", "boot 4")

	// start: a new runwhp, its restarts counted from 0. The fourth run
	// stays up.
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitLog(t, name, "boot 4")
	if s := waitStatus(t, name, "running", 30*time.Second); s.RestartCount != 0 {
		t.Errorf("after start: %+v", s)
	}
	// kill, by any signal: no fifth run.
	if r := cli(t, "kill", name, "KILL"); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	waitStatus(t, name, "stopped", 30*time.Second)
	quiet("after kill", "boot 5")

	// A signal the container handles leaves it running, but its next end
	// is by hand.
	trap := newName(t)
	detached(t, "--name", trap, "--restart", "always", alpine, "sh", "-c",
		bootCounter(`trap "echo hup" HUP; i=0; while [ $i -lt 30 ]; do sleep 0.1; i=$((i+1)); done; exit 7`))
	waitLog(t, trap, "boot 1")
	if r := cli(t, "kill", trap, "HUP"); r.code != 0 {
		t.Fatalf("kill HUP: %+v", r)
	}
	waitLog(t, trap, "hup")
	if s := stateOf(t, trap); s.Status != "running" {
		t.Errorf("after HUP: %+v", s)
	}
	s = waitStatus(t, trap, "stopped", 30*time.Second)
	time.Sleep(500 * time.Millisecond)
	if r := cli(t, "logs", trap); strings.Contains(r.stdout, "boot 2") || exitCodeOf(s) != 7 {
		t.Errorf("after HUP and exit 7: %+v, %+v", r, s)
	}
}

// TestRestarting checks what a container waiting to start again allows.
func TestRestarting(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "3s")
	name := newName(t)
	detached(t, "--name", name, "--restart", "always", alpine, "sh", "-c", bootCounter("exit 1"))
	s := waitState(t, name, 30*time.Second, "restarting", func(s stateJSON) bool { return s.Status == "restarting" })
	if exitCodeOf(s) != 1 || s.RestartCount != 1 {
		t.Errorf("restarting: %+v", s)
	}
	r := cli(t, "list")
	if !regexp.MustCompile(`Restarting \(1\) (Less than a second|\d+ seconds?) ago`).MatchString(r.stdout) {
		t.Errorf("list:\n%s", r.stdout)
	}
	for _, args := range [][]string{
		{"exec", name, "true"},
		{"attach", name},
		{"delete", name},
	} {
		if r := cli(t, args...); r.code == 0 || !strings.Contains(r.stderr, "restarting") {
			t.Errorf("%s: %+v", args[0], r)
		}
	}
	if r := cli(t, "container", "prune", "-f"); r.code != 0 || strings.Contains(r.stdout, s.ID) {
		t.Errorf("container prune: %+v", r)
	}
	if r := cli(t, "start", name); r.code != 0 || stateOf(t, name).Status != "restarting" {
		t.Errorf("start while restarting: %+v", r)
	}
	// wait waits through the delay for the next run's end.
	start := time.Now()
	if r := cli(t, "wait", name); strings.TrimSpace(r.stdout) != "1" || time.Since(start) < time.Second {
		t.Errorf("wait: %+v after %v", r, time.Since(start))
	}
	waitState(t, name, 30*time.Second, "restarting again", func(s stateJSON) bool { return s.Status == "restarting" && s.RestartCount == 2 })
	start = time.Now()
	if r := cli(t, "stop", name); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	if s := stateOf(t, name); s.Status != "stopped" || exitCodeOf(s) != 1 || time.Since(start) > 2*time.Second {
		t.Errorf("stop took %v: %+v", time.Since(start), s)
	}
	time.Sleep(3500 * time.Millisecond)
	if r := cli(t, "logs", name); strings.Contains(r.stdout, "boot 3") {
		t.Errorf("ran after stop: %q", r.stdout)
	}

	gone := newName(t)
	detached(t, "--name", gone, "--restart", "always", alpine, "sh", "-c", "exit 1")
	waitState(t, gone, 30*time.Second, "restarting", func(s stateJSON) bool { return s.Status == "restarting" })
	if r := cli(t, "delete", "-f", gone); r.code != 0 {
		t.Fatalf("delete -f: %+v", r)
	}
	if r := cli(t, "state", gone); r.code == 0 {
		t.Errorf("still there: %+v", r)
	}
}

// TestRestartPortTaken restarts a container whose published port someone
// else has taken: each boot fails, and is tried again until it is free.
func TestRestartPortTaken(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "200ms,400ms")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	name := newName(t)
	detached(t, "--name", name, "--restart", "always", "-p", fmt.Sprintf("%d:80", port), alpine, "sh", "-c",
		bootCounter("sleep 1; exit 1"))
	waitLog(t, name, "boot 1")
	// Take the port as the first run's VM lets it go.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if l, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the port never came free: %v", err)
		}
	}
	s := waitState(t, name, 30*time.Second, "failed to boot twice", func(s stateJSON) bool {
		return s.RestartCount >= 3 && strings.Contains(s.Error, strconv.Itoa(port))
	})
	if r := cli(t, "logs", name); strings.Contains(r.stdout, "boot 2") {
		t.Errorf("booted with the port taken: %+v, %+v", r, s)
	}
	l.Close()
	waitLog(t, name, "boot 2")
	if r := cli(t, "stop", name); r.code != 0 {
		t.Errorf("stop: %+v", r)
	}
}

// TestRestartSupervisorKilled kills the runwhp of a container waiting to
// start again: the container is stopped, with how it last ended.
func TestRestartSupervisorKilled(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "1h")
	name := newName(t)
	detached(t, "--name", name, "--restart", "always", alpine, "sh", "-c", "exit 7")
	s := waitState(t, name, 30*time.Second, "restarting", func(s stateJSON) bool { return s.Status == "restarting" })
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(s.Pid))
	if err != nil {
		t.Fatal(err)
	}
	windows.TerminateProcess(h, 1)
	windows.CloseHandle(h)
	s = waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 7 || !strings.Contains(s.Error, "waited to be started again") {
		t.Errorf("after the kill: %+v", s)
	}
}

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

func handleCount(t *testing.T, pid int) uint32 {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var n uint32
	if r, _, err := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&n))); r == 0 {
		t.Fatal(err)
	}
	return n
}

// TestRestartLeak boots a container 50 times in one runwhp, and checks
// that what the runwhp holds does not grow with them.
func TestRestartLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("50 boots")
	}
	t.Setenv("RUNWHP_RESTART_BACKOFF", "10ms,10ms")
	name := newName(t)
	detached(t, "--name", name, "--restart", "always", "-m", "512m", alpine, "true")
	at := func(n int) (stateJSON, uint32, uint64) {
		s := waitState(t, name, 2*time.Minute, fmt.Sprintf("restarted %d times", n), func(s stateJSON) bool { return s.RestartCount >= n })
		return s, handleCount(t, s.Pid), commitOf(t, s.Pid)
	}
	s10, h10, c10 := at(10)
	s50, h50, c50 := at(50)
	t.Logf("after 10 restarts: %d handles, %d MiB committed; after 50: %d handles, %d MiB", h10, c10>>20, h50, c50>>20)
	if s50.Pid != s10.Pid {
		t.Fatalf("another runwhp: %d, then %d", s10.Pid, s50.Pid)
	}
	if h50 > h10+50 {
		t.Errorf("handles grew from %d to %d", h10, h50)
	}
	if c50 > c10+64<<20 {
		t.Errorf("commit grew from %d MiB to %d MiB", c10>>20, c50>>20)
	}
	if r := cli(t, "stop", name); r.code != 0 {
		t.Errorf("stop: %+v", r)
	}
}

// TestRestartForeground runs a container in the foreground with a restart
// policy: runwhp exits with the first run's exit code, and a runwhp in the
// background runs it again, its output going to the log after the first
// run's.
func TestRestartForeground(t *testing.T) {
	t.Setenv("RUNWHP_RESTART_BACKOFF", "50ms")
	name := newName(t)
	start := time.Now()
	r := keep(t, "--name", name, "--restart", "always", alpine, "sh", "-c",
		bootCounter(`[ $n -ge 2 ] && exec sleep 600; exit 4`))
	if r.code != 4 || r.stdout != "boot 1\n" || time.Since(start) > 15*time.Second {
		t.Fatalf("run: %+v after %v", r, time.Since(start))
	}
	waitLog(t, name, "boot 2")
	s := waitStatus(t, name, "running", 30*time.Second)
	if s.Output != "log" || s.RestartCount != 1 || s.Pid == 0 {
		t.Errorf("state %+v", s)
	}
	if l := cli(t, "logs", name); l.stdout != "boot 1\nboot 2\n" {
		t.Errorf("logs: %+v", l)
	}
	if r := cli(t, "stop", "-t", "1", name); r.code != 0 {
		t.Errorf("stop: %+v", r)
	}

	// A run interrupted is not handed over.
	stopped := newName(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd, interrupt := interruptibleArgv(t, &stdout, &stderr, keepArgs("--name", stopped, "--restart", "always", alpine, "sh", "-c",
		bootCounter(`echo ready; sleep 600`)))
	interrupt()
	cmd.Wait()
	time.Sleep(time.Second)
	if s := stateOf(t, stopped); s.Status != "stopped" {
		t.Errorf("after Ctrl+Break: %+v", s)
	}
	if l := cli(t, "logs", stopped); strings.Contains(l.stdout, "boot 2") {
		t.Errorf("ran again: %+v", l)
	}
}
