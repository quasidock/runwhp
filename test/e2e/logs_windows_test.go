//go:build windows

package e2e

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// logFiles lists a container's log files with their sizes.
func logFiles(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, e := range ents {
		if e.Name() == "log" || strings.HasPrefix(e.Name(), "log.") {
			fi, err := e.Info()
			if err == nil {
				out[e.Name()] = fi.Size()
			}
		}
	}
	return out
}

func names(m map[string]int64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkNumbers checks that s is the numbers from first to last, a line
// each, and returns first.
func checkNumbers(t *testing.T, what, s string, last int) int {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	first, err := strconv.Atoi(lines[0])
	if err != nil || !strings.HasSuffix(s, "\n") {
		t.Fatalf("%s: starts %q, ends %q", what, lines[0], s[max(0, len(s)-20):])
	}
	for i, l := range lines {
		if l != strconv.Itoa(first+i) {
			t.Fatalf("%s: line %d is %q, want %d", what, i, l, first+i)
		}
	}
	if first+len(lines)-1 != last {
		t.Errorf("%s: ends at %d, want %d", what, first+len(lines)-1, last)
	}
	return first
}

// TestLogRotation writes a million numbered lines, about 6.9 MB, to a log
// rotated at 1 MiB with three files kept: logs prints the last of them in
// order, and logs -f, started before, every one.
func TestLogRotation(t *testing.T) {
	name := newName(t)
	id := created(t, "--name", name, "--log-opt", "max-size=1m", "--log-opt", "max-file=3", alpine, "sh", "-c",
		`for i in $(seq 0 99); do seq $((i*10000+1)) $((i*10000+10000)); sleep 0.05; done`)
	follow := exec.Command(runwhp(t), "logs", "-f", name)
	var followed, followErr bytes.Buffer
	follow.Stdout, follow.Stderr = &followed, &followErr
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	defer follow.Process.Kill()
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	s := waitStatus(t, name, "stopped", 2*time.Minute)
	if exitCodeOf(s) != 0 {
		t.Fatalf("exited %+v", s)
	}
	done := make(chan error, 1)
	go func() { done <- follow.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("logs -f: %v, %s", err, followErr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f did not end with the container")
	}
	checkNumbers(t, "logs -f", followed.String(), 1000000)

	files := logFiles(t, s.Bundle)
	if got := strings.Join(names(files), ","); got != "log,log.1,log.2" {
		t.Errorf("log files %v", files)
	}
	var total int64
	for f, size := range files {
		if size > 1<<20 {
			t.Errorf("%s is %d bytes", f, size)
		}
		total += size
	}
	r := cli(t, "logs", name)
	if first := checkNumbers(t, "logs", r.stdout, 1000000); first < 500000 {
		t.Errorf("logs printed from %d", first)
	}

	// system df counts every file.
	var js struct {
		Containers []struct {
			ID   string
			Size int64
		}
	}
	if err := json.Unmarshal([]byte(cli(t, "system", "df", "--format", "json").stdout), &js); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range js.Containers {
		if strings.HasPrefix(id, c.ID) || strings.HasPrefix(c.ID, id) {
			found = true
			if c.Size < total {
				t.Errorf("system df: %d bytes, the logs alone %d", c.Size, total)
			}
		}
	}
	if !found {
		t.Errorf("system df lists no %s: %+v", id, js)
	}
}

// TestLogDefaultRotation checks that a log rotates at 20 MB without being
// asked to.
func TestLogDefaultRotation(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "yes 0123456789abcdef0123456789abcdef | head -c 31457280")
	s := waitStatus(t, name, "stopped", time.Minute)
	files := logFiles(t, s.Bundle)
	if got := strings.Join(names(files), ","); got != "log,log.1" || files["log.1"] > 20<<20 || files["log.1"] < 19<<20 {
		t.Errorf("log files %v", files)
	}
}

// v1Frame is a frame of a log written before iteration 13.
func v1Frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

// TestLogVersion1 checks that a log written by an older runwhp still
// prints, and that starting the container again keeps it, before the new.
func TestLogVersion1(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "echo run $(date +%s%N)")
	s := waitStatus(t, name, "stopped", time.Minute)
	for f := range logFiles(t, s.Bundle) {
		os.Remove(filepath.Join(s.Bundle, f))
	}
	old := append(v1Frame(1, "old out\n"), v1Frame(2, "old err\n")...)
	if err := os.WriteFile(filepath.Join(s.Bundle, "log"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, "logs", name); r.stdout != "old out\n" || r.stderr != "old err\n" {
		t.Errorf("version 1 log: %+v", r)
	}
	if r := cli(t, "start", name); r.code != 0 {
		t.Fatalf("start: %+v", r)
	}
	waitStatus(t, name, "stopped", time.Minute)
	r := cli(t, "logs", name)
	if !strings.HasPrefix(r.stdout, "old out\nrun ") || strings.Count(r.stdout, "\n") != 2 || r.stderr != "old err\n" {
		t.Errorf("both logs: %+v", r)
	}
	if got := strings.Join(names(logFiles(t, s.Bundle)), ","); got != "log,log.1" {
		t.Errorf("log files %s", got)
	}
}

// TestForegroundLog checks that a container run in the foreground keeps a
// log of what it printed, and that one made with --log-driver none keeps
// none.
func TestForegroundLog(t *testing.T) {
	name := newName(t)
	r := keep(t, "--name", name, alpine, "sh", "-c", "echo out; echo err >&2; printf partial")
	if r.code != 0 || r.stdout != "out\npartial" || r.stderr != "err\n" {
		t.Fatalf("run: %+v", r)
	}
	if l := cli(t, "logs", name); l.stdout != r.stdout || l.stderr != r.stderr || l.code != 0 {
		t.Errorf("logs: %+v", l)
	}

	none := newName(t)
	detached(t, "--name", none, "--log-driver", "none", alpine, "echo", "hi")
	s := waitStatus(t, none, "stopped", time.Minute)
	if l := cli(t, "logs", none); l.stdout != "" || !strings.Contains(l.stderr, "keeps no log") {
		t.Errorf("logs with --log-driver none: %+v", l)
	}
	if files := logFiles(t, s.Bundle); len(files) != 0 {
		t.Errorf("log files %v", files)
	}

	for _, args := range [][]string{
		{"--log-driver", "journald"},
		{"--log-opt", "max-size=1k"},
		{"--log-opt", "compress=true"},
	} {
		r := cli(t, append(runArgs(args...), alpine, "true")...)
		if r.code != 125 || !strings.Contains(r.stderr, "--log-") {
			t.Errorf("run %s: %+v", strings.Join(args, " "), r)
		}
	}
}

// TestLogTimestamps checks logs -t, --since and --until on a container
// that writes a line every 300 ms.
func TestLogTimestamps(t *testing.T) {
	name := newName(t)
	before := time.Now()
	detached(t, "--name", name, alpine, "sh", "-c", "for i in 1 2 3 4 5; do echo line $i; sleep 0.3; done; echo err >&2")
	waitStatus(t, name, "stopped", time.Minute)
	after := time.Now()
	r := cli(t, "logs", "-t", name)
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 5 || !strings.HasSuffix(r.stderr, " err\n") {
		t.Fatalf("logs -t: %+v", r)
	}
	var times []time.Time
	for i, l := range lines {
		ts, rest, _ := strings.Cut(l, " ")
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil || len(ts) != len("2006-01-02T15:04:05.000000000Z") || rest != fmt.Sprintf("line %d", i+1) {
			t.Fatalf("line %q: %v", l, err)
		}
		if at.Before(before) || at.After(after) || (i > 0 && at.Sub(times[i-1]) < 200*time.Millisecond) {
			t.Errorf("line %d at %v; run from %v to %v", i+1, at, before, after)
		}
		times = append(times, at)
	}
	// The middle three lines, by the times printed.
	since := times[1].Add(-time.Millisecond).Format(time.RFC3339Nano)
	until := times[3].Add(time.Millisecond).Format(time.RFC3339Nano)
	if r := cli(t, "logs", "--since", since, "--until", until, name); r.stdout != "line 2\nline 3\nline 4\n" || r.stderr != "" {
		t.Errorf("--since %s --until %s: %+v", since, until, r)
	}
	unix := fmt.Sprintf("%d.%09d", times[4].Unix(), times[4].Nanosecond())
	if r := cli(t, "logs", "--since", unix, name); r.stdout != "line 5\n" || r.stderr != "err\n" {
		t.Errorf("--since %s: %+v", unix, r)
	}
	if r := cli(t, "logs", "--since", "1h", name); !strings.HasPrefix(r.stdout, "line 1\n") {
		t.Errorf("--since 1h: %+v", r)
	}
	if r := cli(t, "logs", "--until", "1h", name); r.stdout != "" || r.stderr != "" {
		t.Errorf("--until 1h: %+v", r)
	}
	if r := cli(t, "logs", "--since", "yesterday", name); r.code != 1 || !strings.Contains(r.stderr, "invalid time") {
		t.Errorf("--since yesterday: %+v", r)
	}
}

// TestLogTail checks logs -n.
func TestLogTail(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "echo one; echo two; sleep 0.3; echo e >&2; sleep 0.3; printf three")
	waitStatus(t, name, "stopped", time.Minute)
	for _, c := range []struct {
		args           []string
		stdout, stderr string
	}{
		{[]string{"-n", "2"}, "three", "e\n"},
		{[]string{"--tail", "1"}, "three", ""},
		{[]string{"-n", "0"}, "", ""},
		{[]string{"--tail", "all"}, "one\ntwo\nthree", "e\n"},
		{[]string{"-n", "10"}, "one\ntwo\nthree", "e\n"},
	} {
		r := cli(t, append(append([]string{"logs"}, c.args...), name)...)
		if r.stdout != c.stdout || r.stderr != c.stderr || r.code != 0 {
			t.Errorf("logs %s: %+v", strings.Join(c.args, " "), r)
		}
	}
	if r := cli(t, "logs", "-n", "-1", name); r.code != 2 && r.code != 1 {
		t.Errorf("logs -n -1: %+v", r)
	}
}

// TestLogFollowTail checks logs -f -n 0, which prints only what comes.
func TestLogFollowTail(t *testing.T) {
	name := newName(t)
	detached(t, "--name", name, alpine, "sh", "-c", "echo early; sleep 1.5; echo late")
	waitLog(t, name, "early")
	r := cli(t, "logs", "-f", "-n", "0", name)
	if r.stdout != "late\n" || r.code != 0 {
		t.Errorf("logs -f -n 0: %+v", r)
	}
}
