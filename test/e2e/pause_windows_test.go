//go:build windows

package e2e

import (
	"strings"
	"testing"
	"time"
)

// counter runs a container in the background that prints a count every
// 50 ms, and returns its name.
func counter(t *testing.T, args ...string) string {
	t.Helper()
	name := newName(t)
	detached(t, append([]string{"--name", name}, append(args, alpine, "sh", "-c",
		`trap 'echo got TERM; exit 3' TERM; i=0; while :; do i=$((i+1)); echo $i; sleep 0.05; done`)...)...)
	waitLog(t, name, "3\n")
	return name
}

// lastLine is the last line of a container's log.
func lastLine(t *testing.T, name string) string {
	t.Helper()
	r := cli(t, "logs", name)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	return lines[len(lines)-1]
}

func TestPause(t *testing.T) {
	parallel(t)
	name := counter(t)
	expect(t, cli(t, "pause", name), name+"\n", 0)
	if s := stateOf(t, name); s.Status != "paused" {
		t.Errorf("state: %s, want paused", s.Status)
	}
	if r := cli(t, "list"); !strings.Contains(r.stdout, "(Paused)") {
		t.Errorf("list does not say paused:\n%s", r.stdout)
	}
	before := lastLine(t, name)
	time.Sleep(500 * time.Millisecond)
	if after := lastLine(t, name); after != before {
		t.Errorf("the count went on while paused: %s, then %s", before, after)
	}

	r := execIn(t, name, "true")
	if r.code == 0 || !strings.Contains(r.stderr, "is paused, unpause the container before exec") {
		t.Errorf("exec into a paused container: %+v", r)
	}
	if r := cli(t, "attach", name); r.code == 0 || !strings.Contains(r.stderr, "cannot attach to a paused container") {
		t.Errorf("attach to a paused container: %+v", r)
	}
	if r := cli(t, "delete", name); r.code == 0 || !strings.Contains(r.stderr, "is paused and must be unpaused first") {
		t.Errorf("delete of a paused container: %+v", r)
	}
	if r := cli(t, "pause", name); r.code == 0 || !strings.Contains(r.stderr, "is already paused") {
		t.Errorf("second pause: %+v", r)
	}
	if r := cli(t, "stats", "--no-stream", name); r.code != 0 {
		t.Errorf("stats of a paused container: %+v", r)
	}

	expect(t, cli(t, "unpause", name), name+"\n", 0)
	if s := stateOf(t, name); s.Status != "running" {
		t.Errorf("state after unpause: %s", s.Status)
	}
	before = lastLine(t, name)
	time.Sleep(500 * time.Millisecond)
	if after := lastLine(t, name); after == before {
		t.Errorf("the count did not go on after unpause: still %s", after)
	}
	if r := cli(t, "unpause", name); r.code == 0 || !strings.Contains(r.stderr, "is not paused") {
		t.Errorf("unpause of a running container: %+v", r)
	}
	expect(t, execIn(t, name, "echo", "hi"), "hi\n", 0)
}

// TestPauseSignals checks that kill and stop reach a paused container, as
// they do Docker's: it is thawed once the signal is sent.
func TestPauseSignals(t *testing.T) {
	parallel(t)
	name := counter(t)
	expect(t, cli(t, "pause", name), name+"\n", 0)
	start := time.Now()
	if r := cli(t, "kill", name, "TERM"); r.code != 0 {
		t.Fatalf("kill: %+v", r)
	}
	s := waitStatus(t, name, "stopped", 10*time.Second)
	if exitCodeOf(s) != 3 {
		t.Errorf("exit code %d, want 3 from the TERM trap", exitCodeOf(s))
	}
	waitLog(t, name, "got TERM")
	t.Logf("kill of a paused container took %v", time.Since(start))

	name = counter(t)
	expect(t, cli(t, "pause", name), name+"\n", 0)
	start = time.Now()
	expect(t, cli(t, "stop", name), name+"\n", 0)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stop of a paused container took %v", took)
	}
	if s := stateOf(t, name); s.Status != "stopped" || exitCodeOf(s) != 3 {
		t.Errorf("after stop: %s, exit %d", s.Status, exitCodeOf(s))
	}
	if r := cli(t, "pause", name); r.code == 0 || !strings.Contains(r.stderr, "is not running") {
		t.Errorf("pause of a stopped container: %+v", r)
	}

	// delete -f of a paused container.
	name = counter(t)
	expect(t, cli(t, "pause", name), name+"\n", 0)
	if r := cli(t, "delete", "-f", name); r.code != 0 {
		t.Errorf("delete -f of a paused container: %+v", r)
	}
}

func TestPauseCreated(t *testing.T) {
	parallel(t)
	name := newName(t)
	created(t, "--name", name, alpine, "true")
	if r := cli(t, "pause", name); r.code == 0 || !strings.Contains(r.stderr, "is not running") {
		t.Errorf("pause of a created container: %+v", r)
	}
}
