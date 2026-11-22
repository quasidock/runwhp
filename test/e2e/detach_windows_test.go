//go:build windows

package e2e

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// detached runs runwhp run -d with RUNWHP_E2E_RUN_FLAGS, and returns the
// container's ID.
func detached(t *testing.T, args ...string) string {
	t.Helper()
	r := cli(t, append([]string{"run", "-d"}, withFlags(args)...)...)
	if r.code != 0 {
		t.Fatalf("run -d: %+v", r)
	}
	return strings.TrimSpace(r.stdout)
}

// created runs runwhp create with RUNWHP_E2E_RUN_FLAGS.
func created(t *testing.T, args ...string) string {
	t.Helper()
	r := cli(t, append([]string{"create"}, withFlags(args)...)...)
	if r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	return strings.TrimSpace(r.stdout)
}

// waitStatus waits for a container to reach a status.
func waitStatus(t *testing.T, ref, status string, d time.Duration) stateJSON {
	t.Helper()
	var s stateJSON
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		if s = stateOf(t, ref); s.Status == status {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s is %s after %v, want %s", ref, s.Status, d, status)
		}
	}
}

// waitLog waits for a container's log to hold want.
func waitLog(t *testing.T, ref, want string) result {
	t.Helper()
	var r result
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if r = cli(t, "logs", ref); strings.Contains(r.stdout, want) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs of %s never held %q: %+v", ref, want, r)
		}
	}
}

func exitCodeOf(s stateJSON) int {
	if s.ExitCode == nil {
		return -1
	}
	return *s.ExitCode
}

// TestDetached runs a container in the background with its output on
// pipes, which go to its log.
func TestDetached(t *testing.T) {
	name := newName(t)
	begin := time.Now()
	id := detached(t, "--name", name, alpine, "sh", "-c", "echo out; echo err >&2; sleep 2; echo late")
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("run -d took %v", d)
	}
	if len(id) != 64 {
		t.Fatalf("run -d printed %q", id)
	}
	if s := stateOf(t, name); s.Status != "running" || s.Pid == 0 || s.GuestPid == 0 {
		t.Errorf("state while running %+v", s)
	}
	if l := cli(t, "list"); !strings.Contains(l.stdout, "Up ") {
		t.Errorf("list while running: %q", l.stdout)
	}
	r := waitLog(t, name, "out\n")
	if r.stderr != "err\n" {
		t.Errorf("logs stderr %q", r.stderr)
	}
	f := cli(t, "logs", "-f", name)
	if f.code != 0 || f.stdout != "out\nlate\n" || f.stderr != "err\n" {
		t.Errorf("logs -f: %+v", f)
	}
	s := waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 0 || s.Pid != 0 {
		t.Errorf("state after %+v", s)
	}
	// A pipe never reached the container: stdout held only the ID.
	if r := cli(t, "state", name); !strings.Contains(r.stdout, `"output": "log"`) {
		t.Errorf("output: %s", r.stdout)
	}
}

// TestDetachedToFile gives run -d a file for stdout, which the container
// writes to after the ID.
func TestDetachedToFile(t *testing.T) {
	name := newName(t)
	exe := runwhp(t) // before the file is open: it may skip the test
	out := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"run", "-d"}, withFlags([]string{"--name", name, alpine, "echo", "to the file"})...)...)
	cmd.Stdout = f
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	f.Close()
	if err != nil {
		t.Fatalf("run -d: %v, %s", err, stderr.String())
	}
	waitStatus(t, name, "stopped", 10*time.Second)
	data, _ := os.ReadFile(out)
	lines := strings.Split(string(data), "\n")
	if len(lines) != 3 || len(lines[0]) != 64 || lines[1] != "to the file" {
		t.Errorf("the file holds %q", data)
	}
	// stderr was a pipe, so it went to the log, which holds nothing else.
	if r := cli(t, "logs", name); r.stdout != "" || r.code != 0 {
		t.Errorf("logs: %+v", r)
	}
}

func TestDetachedTTY(t *testing.T) {
	name := newName(t)
	detached(t, "-t", "--name", name, alpine, "sh", "-c", "tty >/dev/null && echo tty; stty size; echo err >&2")
	waitStatus(t, name, "stopped", 10*time.Second)
	r := cli(t, "logs", name)
	if r.stdout != "tty\r\n24 80\r\nerr\r\n" || r.stderr != "" {
		t.Errorf("logs: %+v", r)
	}
}

func TestDetachedStdin(t *testing.T) {
	name := newName(t)
	cmd := exec.Command(runwhp(t), append([]string{"run", "-d", "-i"}, withFlags([]string{"--name", name, alpine, "wc", "-c"})...)...)
	cmd.Stdin = strings.NewReader("12345")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run -d -i: %v, %s", err, out)
	}
	waitStatus(t, name, "stopped", 10*time.Second)
	if r := cli(t, "logs", name); strings.TrimSpace(r.stdout) != "5" {
		t.Errorf("logs: %+v", r)
	}
	// Without -i the container reads nothing.
	name = newName(t)
	detached(t, "--name", name, alpine, "wc", "-c")
	waitStatus(t, name, "stopped", 10*time.Second)
	if r := cli(t, "logs", name); strings.TrimSpace(r.stdout) != "0" {
		t.Errorf("logs without -i: %+v", r)
	}
}

// TestCreateStart walks a container through create, start and kill.
func TestCreateStart(t *testing.T) {
	name := newName(t)
	id := created(t, "--name", name, alpine, "sh", "-c", "touch /started; echo started; sleep 60")
	s := stateOf(t, name)
	if s.ID != id || s.Status != "created" || s.Pid == 0 || s.GuestPid != 0 {
		t.Errorf("created: %+v", s)
	}
	if l := cli(t, "list"); !strings.Contains(l.stdout, "Created") {
		t.Errorf("list: %q", l.stdout)
	}
	if r := cli(t, "start", name); r.code != 0 || r.stdout != name+"\n" {
		t.Fatalf("start: %+v", r)
	}
	if s = stateOf(t, name); s.Status != "running" || s.GuestPid == 0 {
		t.Errorf("started: %+v", s)
	}
	waitLog(t, name, "started")
	// Starting a running container leaves it alone.
	if r := cli(t, "start", name); r.code != 0 {
		t.Errorf("second start: %+v", r)
	}
	if r := cli(t, "kill", name); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	if s = waitStatus(t, name, "stopped", 10*time.Second); exitCodeOf(s) != 143 {
		t.Errorf("after kill: %+v", s)
	}
	if r := cli(t, "kill", name); r.code != 1 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("kill a stopped container: %+v", r)
	}
}

// TestKillCreated ends a created container without starting it.
func TestKillCreated(t *testing.T) {
	name := newName(t)
	created(t, "--name", name, alpine, "touch", "/started")
	if r := cli(t, "kill", name, "SIGUSR1"); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	s := waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 138 {
		t.Errorf("after kill: %+v", s)
	}
	// It never ran: starting it now runs it for the first time.
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	s = waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 0 {
		t.Errorf("after start: %+v", s)
	}

	// A created container is deleted without -f.
	name = newName(t)
	created(t, "--name", name, alpine, "true")
	if r := cli(t, "delete", name); r.code != 0 {
		t.Errorf("delete a created container: %+v", r)
	}
}

func TestKillSignals(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c",
		`trap 'echo usr1' USR1; trap 'echo hup' HUP; trap 'echo term; exit 3' TERM; echo ready; while :; do sleep 0.1; done`)
	waitLog(t, name, "ready")
	// One at a time: signals do not queue, and the shell runs its traps
	// in signal number order.
	want := "ready\n"
	for _, c := range []struct{ sig, echo string }{{"USR1", "usr1"}, {"SIGHUP", "hup"}, {"10", "usr1"}} {
		if r := cli(t, "kill", name, c.sig); r.code != 0 {
			t.Fatalf("kill %s: %+v", c.sig, r)
		}
		want += c.echo + "\n"
		waitLog(t, name, want)
	}
	if r := cli(t, "kill", name, "NOSUCH"); r.code != 2 {
		t.Errorf("kill with a bad signal: %+v", r)
	}
	cli(t, "kill", name)
	if s := waitStatus(t, name, "stopped", 10*time.Second); exitCodeOf(s) != 3 {
		t.Errorf("after TERM: %+v", s)
	}

	// --all reaches every process, not only the main one.
	name = newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", `trap '' TERM; (trap 'echo child; exit 0' TERM; echo ready; while :; do sleep 0.1; done) & wait; sleep 60`)
	waitLog(t, name, "ready")
	cli(t, "kill", "--all", name)
	waitLog(t, name, "child")
	cli(t, "kill", name, "KILL")
	waitStatus(t, name, "stopped", 10*time.Second)
}

// TestStop stops a container that ignores SIGTERM, which takes SIGKILL
// after the timeout, and one that heeds it.
func TestStop(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", `trap '' TERM; echo ready; sleep 60 & wait; sleep 60`)
	waitLog(t, name, "ready")
	begin := time.Now()
	if r := cli(t, "stop", "-t", "1", name); r.code != 0 || r.stdout != name+"\n" {
		t.Fatalf("stop: %+v", r)
	}
	if d := time.Since(begin); d < time.Second || d > 4*time.Second {
		t.Errorf("stop -t 1 took %v", d)
	}
	if s := stateOf(t, name); s.Status != "stopped" || exitCodeOf(s) != 137 {
		t.Errorf("after stop: %+v", s)
	}

	name = newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", `trap 'exit 5' TERM; echo ready; while :; do sleep 0.1; done`)
	waitLog(t, name, "ready")
	begin = time.Now()
	if r := cli(t, "stop", name); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("stop of a container that heeds SIGTERM took %v", d)
	}
	if s := stateOf(t, name); exitCodeOf(s) != 5 {
		t.Errorf("after stop: %+v", s)
	}
	// Stopping a stopped container does nothing.
	if r := cli(t, "stop", name); r.code != 0 {
		t.Errorf("second stop: %+v", r)
	}
	if st := layerState(t, stateOf(t, name)); !st.Clean || st.JournalStart != 0 {
		t.Errorf("writable layer after stop: %+v", st)
	}
}

// TestRestart starts a stopped container again, in a new VM on the same
// writable layer, with the same hostname.
func TestRestart(t *testing.T) {
	name := newName(t)
	script := `n=$(cat /boots 2>/dev/null || echo 0); n=$((n+1)); echo $n > /boots; echo boot $n $(hostname); trap 'exit 0' TERM; while :; do sleep 0.1; done`
	detached(t, "--name", name, alpine, "sh", "-c", script)
	waitLog(t, name, "boot 1")
	host := strings.Fields(waitLog(t, name, "boot 1").stdout)[2]
	if r := cli(t, "restart", "-t", "2", name); r.code != 0 {
		t.Fatalf("restart: %+v", r)
	}
	waitLog(t, name, "boot 2 "+host)
	cli(t, "stop", name)
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitLog(t, name, "boot 3 "+host)
	cli(t, "stop", name)

	// A --rm container is gone once stopped, so it cannot restart.
	rm := newName(t)
	detached(t, "--rm", "--name", rm, alpine, "sleep", "60")
	if r := cli(t, "restart", rm); r.code != 1 || !strings.Contains(r.stderr, "--rm") {
		t.Errorf("restart of a --rm container: %+v", r)
	}
	cli(t, "kill", rm, "KILL")
	for deadline := time.Now().Add(10 * time.Second); cli(t, "state", rm).code == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the --rm container is still there after it stopped")
		}
	}
}

func TestDeleteRunning(t *testing.T) {
	name := newName(t)
	id := detached(t, "--name", name, alpine, "sleep", "60")
	if r := cli(t, "delete", name); r.code != 1 || !strings.Contains(r.stderr, "-f") {
		t.Errorf("delete a running container: %+v", r)
	}
	begin := time.Now()
	if r := cli(t, "delete", "-f", name); r.code != 0 {
		t.Fatalf("delete -f: %+v", r)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("delete -f took %v", d)
	}
	if _, err := os.Stat(filepath.Join(storeDir(t), "containers", id)); !os.IsNotExist(err) {
		t.Errorf("the container's directory is still there: %v", err)
	}

	// A --rm container removes itself as it stops, which delete -f is
	// content with.
	rm := newName(t)
	detached(t, "--rm", "--name", rm, alpine, "sleep", "60")
	if r := cli(t, "delete", "-f", rm); r.code != 0 {
		t.Errorf("delete -f of a --rm container: %+v", r)
	}
}

// TestSupervisorKilled kills the runwhp running a container. The record
// says how it ended, and the next start replays the writable layer's
// journal, keeping what was synced.
func TestSupervisorKilled(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c",
		`if [ -f /data ]; then echo found $(cat /data); exit 0; fi; echo synced > /data; sync /data; echo ready; while :; do date > /scratch; sleep 0.05; done`)
	waitLog(t, name, "ready")
	s := stateOf(t, name)
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(s.Pid))
	if err != nil {
		t.Fatal(err)
	}
	windows.TerminateProcess(h, 1)
	windows.CloseHandle(h)
	s = waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 255 || !strings.Contains(s.Error, "ended without") {
		t.Errorf("after the kill: %+v", s)
	}
	if st := layerState(t, s); !st.NeedsRecovery {
		t.Logf("the writable layer did not need recovery: %+v", st)
	}
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitLog(t, name, "found synced")
	s = waitStatus(t, name, "stopped", 10*time.Second)
	if st := layerState(t, s); !st.Clean || st.NeedsRecovery {
		t.Errorf("writable layer after the next run: %+v", st)
	}
}

// TestConcurrentStart starts a stopped container from three runwhps at
// once: one runs it, and all three succeed.
func TestConcurrentStart(t *testing.T) {
	name := newName(t)
	expect(t, keep(t, "--name", name, alpine, "true"), "", 0)
	stopFile := "/stop-" + name
	_ = stopFile
	var wg sync.WaitGroup
	results := make([]result, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = cli(t, "start", name)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.code != 0 {
			t.Errorf("start %d: %+v", i, r)
		}
	}
	waitStatus(t, name, "stopped", 10*time.Second)
}

// TestKillForeground signals a container a foreground run is running,
// from another runwhp.
func TestKillForeground(t *testing.T) {
	name := newName(t)
	var stdout syncBuffer
	var stderr bytes.Buffer
	cmd := exec.Command(runwhp(t), keepArgs("--name", name, alpine, "sh", "-c", "trap 'echo usr2; exit 4' USR2; echo ready; while :; do sleep 0.1; done")...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(stdout.String(), "ready"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("container never got ready")
		}
	}
	if s := stateOf(t, name); s.Status != "running" {
		t.Errorf("state %+v", s)
	}
	if r := cli(t, "kill", name, "USR2"); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	if code := exitCode(cmd.Wait()); code != 4 || stdout.String() != "ready\nusr2\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if s := stateOf(t, name); s.Status != "stopped" || exitCodeOf(s) != 4 {
		t.Errorf("state %+v", s)
	}
}

// TestDetachedOutlivesConsole runs run -d on a pseudo console and closes
// the console, as closing a terminal window does: the container goes on.
func TestDetachedOutlivesConsole(t *testing.T) {
	name := newName(t)
	p := startPTYArgv(t, 24, 80, append([]string{"run", "-d"}, withFlags([]string{"--name", name, alpine, "sleep", "60"})...))
	if code := p.wait(); code != 0 {
		t.Fatalf("run -d exited %d; the terminal shows %q", code, p.text())
	}
	p.close()
	time.Sleep(time.Second)
	if s := stateOf(t, name); s.Status != "running" {
		t.Errorf("after the console closed: %+v", s)
	}
}

// TestPublishOnRestart checks that a container's published ports are bound
// again when it starts again, and that a port taken meanwhile fails the
// start.
func TestPublishOnRestart(t *testing.T) {
	name := newName(t)
	port := freePort(t)
	detached(t, "--name", name, "-p", port+":8080", alpine, "sh", "-c",
		`echo "boot $(cut -c1-8 /proc/sys/kernel/random/boot_id)" > /msg; exec nc -lk -p 8080 -e cat /msg`)
	get := func() string {
		var line string
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			c, err := net.Dial("tcp", "127.0.0.1:"+port)
			if err != nil {
				continue
			}
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			b, _ := io.ReadAll(c)
			c.Close()
			if line = string(b); strings.HasPrefix(line, "boot ") {
				return line
			}
		}
		t.Fatalf("nothing served on port %s: %q", port, line)
		return ""
	}
	first := get()
	if r := cli(t, "restart", "-t", "1", name); r.code != 0 {
		t.Fatalf("restart: %+v", r)
	}
	if second := get(); second == first {
		t.Errorf("served %q before and after the restart", first)
	}
	cli(t, "stop", "-t", "1", name)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if r := cli(t, "start", name); r.code != 1 || !strings.Contains(r.stderr, port) {
		t.Errorf("start with the port taken: %+v", r)
	}
}
