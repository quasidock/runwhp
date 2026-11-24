//go:build windows

package container

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock hands out times a millisecond apart from a fixed start.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func openTestLog(t *testing.T, dir string, opts LogOptions, clock *fakeClock) *LogWriter {
	t.Helper()
	w, err := openLogWriter(dir, opts, func(f string, a ...any) { t.Logf("diag: "+f, a...) }, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// printLog prints the log in dir through f.
func printLog(t *testing.T, dir string, f LogFilter) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	p, err := NewLogPrinter(dir, &stdout, &stderr, f)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Copy(); err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String()
}

var all = LogFilter{Tail: -1}

func TestLogWriterRoundTrip(t *testing.T) {
	dir := testRoot(t)
	w := openTestLog(t, dir, LogOptions{}, newFakeClock())
	out, errs := w.Stream(Stdout), w.Stream(Stderr)
	out.Write([]byte("a\nb"))
	errs.Write([]byte("e\n"))
	out.Write([]byte("c\n"))
	big := bytes.Repeat([]byte("x"), maxFrame+10)
	if n, err := out.Write(big); n != len(big) || err != nil {
		t.Fatalf("big write: %d, %v", n, err)
	}
	w.Close()
	stdout, stderr := printLog(t, dir, all)
	if stdout != "a\nbc\n"+string(big) || stderr != "e\n" {
		t.Errorf("stdout %q... (%d bytes), stderr %q", stdout[:min(len(stdout), 10)], len(stdout), stderr)
	}
	files, _ := LogFiles(dir)
	if len(files) != 1 || files[0].Version != 2 || files[0].Gen != 1 {
		t.Errorf("files %+v", files)
	}
}

func TestLogTimestamps(t *testing.T) {
	dir := testRoot(t)
	clock := newFakeClock()
	w := openTestLog(t, dir, LogOptions{}, clock)
	out := w.Stream(Stdout)
	out.Write([]byte("one\ntw")) // 12:00:00.001
	out.Write([]byte("o\n"))     // .002: the rest of a line started at .001
	w.Stream(Stderr).Write([]byte("err\n"))
	out.Write([]byte("three")) // .004, with no newline
	w.Close()
	stdout, stderr := printLog(t, dir, LogFilter{Tail: -1, Timestamps: true})
	want := "2026-11-24T12:00:00.001000000Z one\n2026-11-24T12:00:00.001000000Z two\n2026-11-24T12:00:00.004000000Z three"
	if stdout != want || stderr != "2026-11-24T12:00:00.003000000Z err\n" {
		t.Errorf("stdout %q\nstderr %q", stdout, stderr)
	}
	at := func(ms int) time.Time { return time.Date(2026, 11, 24, 12, 0, 0, ms*1e6, time.UTC) }
	for _, c := range []struct {
		f              LogFilter
		stdout, stderr string
	}{
		{LogFilter{Tail: 2}, "three", "err\n"},
		{LogFilter{Tail: 1}, "three", ""},
		{LogFilter{Tail: 0}, "", ""},
		{LogFilter{Tail: 10}, "one\ntwo\nthree", "err\n"},
		{LogFilter{Tail: -1, Since: at(2)}, "three", "err\n"},
		{LogFilter{Tail: -1, Until: at(3)}, "one\ntwo\n", "err\n"},
		{LogFilter{Tail: -1, Since: at(3), Until: at(3)}, "", "err\n"},
		{LogFilter{Tail: 1, Until: at(3)}, "", "err\n"},
	} {
		stdout, stderr := printLog(t, dir, c.f)
		if stdout != c.stdout || stderr != c.stderr {
			t.Errorf("%+v: stdout %q, stderr %q; want %q, %q", c.f, stdout, stderr, c.stdout, c.stderr)
		}
	}
}

// writeLines writes lines "line N\n" for N in [from, to), each a write of
// its own.
func writeLines(w io.Writer, from, to int) {
	for i := from; i < to; i++ {
		fmt.Fprintf(w, "line %03d\n", i)
	}
}

// checkLines checks that s is "line N\n" for consecutive N ending at last.
func checkLines(t *testing.T, s string, last int) (first int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if s == "" {
		t.Fatal("no lines")
	}
	if _, err := fmt.Sscanf(lines[0], "line %d", &first); err != nil {
		t.Fatalf("first line %q", lines[0])
	}
	for i, l := range lines {
		if want := fmt.Sprintf("line %03d", first+i); l != want {
			t.Fatalf("line %d is %q, want %q", i, l, want)
		}
	}
	if first+len(lines)-1 != last {
		t.Errorf("ends at line %d, want %d", first+len(lines)-1, last)
	}
	return first
}

func TestLogRotation(t *testing.T) {
	dir := testRoot(t)
	w := openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 3}, newFakeClock())
	writeLines(w.Stream(Stdout), 0, 100)
	w.Close()
	files, err := LogFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, f := range files {
		names = append(names, filepath.Base(f.Path))
		fi, _ := os.Stat(f.Path)
		if fi.Size() > 300 {
			t.Errorf("%s is %d bytes", f.Path, fi.Size())
		}
		if i > 0 && f.Gen != files[i-1].Gen+1 {
			t.Errorf("generations %+v", files)
		}
	}
	if strings.Join(names, ",") != "log.2,log.1,log" {
		t.Errorf("files %v", names)
	}
	stdout, _ := printLog(t, dir, all)
	if first := checkLines(t, stdout, 99); first < 50 {
		t.Errorf("kept from line %d", first)
	}
	stdout, _ = printLog(t, dir, LogFilter{Tail: 5})
	if checkLines(t, stdout, 99) != 95 {
		t.Errorf("tail 5: %q", stdout)
	}

	// Opening it again appends, and keeps the generations going.
	w = openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 3}, newFakeClock())
	writeLines(w.Stream(Stdout), 100, 110)
	w.Close()
	stdout, _ = printLog(t, dir, all)
	checkLines(t, stdout, 109)

	// A lower limit drops the files past it.
	w = openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 1}, newFakeClock())
	writeLines(w.Stream(Stdout), 110, 140)
	w.Close()
	files, _ = LogFiles(dir)
	if len(files) != 1 || files[0].N != 0 {
		t.Errorf("max-file 1 left %+v", files)
	}
	stdout, _ = printLog(t, dir, all)
	checkLines(t, stdout, 139)
}

func TestLogFollow(t *testing.T) {
	dir := testRoot(t)
	w := openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 3}, newFakeClock())
	var stdout, stderr bytes.Buffer
	p, err := NewLogPrinter(dir, &stdout, &stderr, all)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Reading as it is written, and falling behind by a rotation or two,
	// which the files kept still hold.
	for i, step := range []int{1, 3, 10, 20, 5, 25} {
		from := 0
		if i > 0 {
			from = len(strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n"))
		}
		writeLines(w.Stream(Stdout), from, from+step)
		if _, err := p.Copy(); err != nil {
			t.Fatal(err)
		}
		checkLines(t, stdout.String(), from+step-1)
	}
	if p.Skipped() != 0 {
		t.Errorf("skipped %d", p.Skipped())
	}
	// Falling behind by more than the files kept skips the ones gone.
	writeLines(w.Stream(Stdout), 64, 200)
	if _, err := p.Copy(); err != nil {
		t.Fatal(err)
	}
	if p.Skipped() == 0 {
		t.Error("nothing skipped")
	}
	if !strings.HasSuffix(stdout.String(), "line 199\n") {
		t.Errorf("ends %q", stdout.String()[stdout.Len()-20:])
	}
}

func TestLogVersion1(t *testing.T) {
	dir := testRoot(t)
	var old []byte
	old = AppendFrame(old, Stdout, []byte("old out\n"))
	old = AppendFrame(old, Stderr, []byte("old err\n"))
	if err := os.WriteFile(filepath.Join(dir, "log"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := printLog(t, dir, LogFilter{Tail: -1, Timestamps: true})
	if stdout != "old out\n" || stderr != "old err\n" {
		t.Errorf("version 1: %q %q", stdout, stderr)
	}
	// A writer moves it aside and starts version 2 after it.
	w := openTestLog(t, dir, LogOptions{MaxSize: DefaultLogMaxSize, MaxFiles: DefaultLogMaxFiles}, newFakeClock())
	w.Stream(Stdout).Write([]byte("new\n"))
	w.Close()
	files, _ := LogFiles(dir)
	if len(files) != 2 || files[0].N != 1 || files[0].Version != 1 || files[1].Gen != 1 {
		t.Errorf("files %+v", files)
	}
	stdout, _ = printLog(t, dir, all)
	if stdout != "old out\nnew\n" {
		t.Errorf("both: %q", stdout)
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if stdout, stderr := printLog(t, dir, LogFilter{Tail: -1, Since: since}); stdout != "new\n" || stderr != "" {
		t.Errorf("since leaves out version 1: %q %q", stdout, stderr)
	}
}

// TestLogCutShort checks that a frame its writer died writing is dropped
// when the log is opened again, and reading a log does not stop at one.
func TestLogCutShort(t *testing.T) {
	dir := testRoot(t)
	w := openTestLog(t, dir, LogOptions{}, newFakeClock())
	w.Stream(Stdout).Write([]byte("whole\n"))
	w.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_APPEND, 0)
	f.Write(appendFrameV2(nil, Stdout, time.Now(), []byte("cut short"))[:20])
	f.Close()
	if stdout, _ := printLog(t, dir, all); stdout != "whole\n" {
		t.Errorf("with a frame cut short: %q", stdout)
	}
	w = openTestLog(t, dir, LogOptions{}, newFakeClock())
	w.Stream(Stdout).Write([]byte("after\n"))
	w.Close()
	if stdout, _ := printLog(t, dir, all); stdout != "whole\nafter\n" {
		t.Errorf("after: %q", stdout)
	}
	os.WriteFile(filepath.Join(dir, "log"), []byte("not a log at all, but long enough to look at"), 0o644)
	p, _ := NewLogPrinter(dir, io.Discard, io.Discard, all)
	defer p.Close()
	if _, err := p.Copy(); err == nil {
		t.Error("garbage read without an error")
	}
}

// TestLogWriteError checks that a log that cannot be written drops output
// rather than failing the container's.
func TestLogWriteError(t *testing.T) {
	dir := testRoot(t)
	var diags []string
	w, err := openLogWriter(dir, LogOptions{}, func(f string, a ...any) { diags = append(diags, fmt.Sprintf(f, a...)) }, newFakeClock().now)
	if err != nil {
		t.Fatal(err)
	}
	w.f.Close() // writes now fail
	for range 3 {
		if n, err := w.Stream(Stdout).Write([]byte("lost\n")); n != 5 || err != nil {
			t.Errorf("write: %d, %v", n, err)
		}
	}
	if len(diags) != 1 {
		t.Errorf("diags %q", diags)
	}
}

// TestLogRotationSplitLines writes each line in two writes, so that files
// start in the middle of lines: a reader starting at such a file skips the
// rest of the line, whose start is gone, rather than print it as a line.
func TestLogRotationSplitLines(t *testing.T) {
	dir := testRoot(t)
	w := openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 2}, newFakeClock())
	out := w.Stream(Stdout)
	split := func(from, to int) {
		for i := from; i < to; i++ {
			fmt.Fprintf(out, "line ")
			fmt.Fprintf(out, "%03d\n", i)
		}
	}
	var followed bytes.Buffer
	p, err := NewLogPrinter(dir, &followed, io.Discard, all)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	split(0, 3)
	p.Copy()
	split(3, 100) // the follower falls behind by more than two files
	if _, err := p.Copy(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if p.Skipped() == 0 {
		t.Error("nothing skipped")
	}
	// What the follower printed is whole lines, in order, ending at the
	// last: the ones in the file it was reading, then a gap, then those
	// in the files still there.
	prev := -1
	for _, l := range strings.Split(strings.TrimSuffix(followed.String(), "\n"), "\n") {
		var n int
		if _, err := fmt.Sscanf(l, "line %d", &n); err != nil || l != fmt.Sprintf("line %03d", n) || n <= prev {
			t.Fatalf("followed %q after line %d", l, prev)
		}
		prev = n
	}
	if prev != 99 || !strings.HasPrefix(followed.String(), "line 000\nline 001\nline 002\n") {
		t.Errorf("followed %q", followed.String())
	}
	stdout, _ := printLog(t, dir, all)
	checkLines(t, stdout, 99)

	// A line over three files, whose start is in the one dropped: the two
	// kept say they start in the middle of it, and the rest is skipped.
	w = openTestLog(t, dir, LogOptions{MaxSize: 300, MaxFiles: 2}, newFakeClock())
	w.Stream(Stdout).Write([]byte("start of a line, "))
	w.Stream(Stdout).Write(bytes.Repeat([]byte("x"), 250))
	w.Stream(Stdout).Write([]byte(" end\nnext\n"))
	w.Close()
	files, _ := LogFiles(dir)
	if len(files) != 2 || files[0].Partial != 1<<Stdout || files[1].Partial != 1<<Stdout {
		t.Errorf("files %+v", files)
	}
	if stdout, _ := printLog(t, dir, all); stdout != "next\n" {
		t.Errorf("both files: %q", stdout)
	}
}
