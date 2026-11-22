package container

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestLogRoundTrip(t *testing.T) {
	var file bytes.Buffer
	l := NewLog(&file)
	out, errs := l.Stream(Stdout), l.Stream(Stderr)
	out.Write([]byte("one\n"))
	errs.Write([]byte("oops\n"))
	out.Write([]byte("two\n"))
	big := bytes.Repeat([]byte("x"), maxFrame+10)
	out.Write(big)

	var stdout, stderr bytes.Buffer
	n, err := NewLogReader(bytes.NewReader(file.Bytes())).Copy(&stdout, &stderr)
	if err != nil || n != int64(4+5+4+len(big)) {
		t.Errorf("Copy: %d, %v", n, err)
	}
	if !strings.HasPrefix(stdout.String(), "one\ntwo\nxxx") || stdout.Len() != 8+len(big) || stderr.String() != "oops\n" {
		t.Errorf("stdout %d bytes, stderr %q", stdout.Len(), stderr.String())
	}
}

// TestLogFollow reads a log as it is written, a byte at a time, as logs -f
// does.
func TestLogFollow(t *testing.T) {
	var file bytes.Buffer
	l := NewLog(&file)
	l.Stream(Stdout).Write([]byte("hello "))
	l.Stream(Stderr).Write([]byte("err"))
	l.Stream(Stdout).Write([]byte("world"))
	data := file.Bytes()
	pr, pw := io.Pipe()
	lr := NewLogReader(&oneByte{pr})
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pw.Write(data)
		pw.Close()
	}()
	if _, err := lr.Copy(&stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if stdout.String() != "hello world" || stderr.String() != "err" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	// A frame cut short waits for the rest.
	lr = NewLogReader(bytes.NewReader(data[:10]))
	stdout.Reset()
	if n, err := lr.Copy(&stdout, &stderr); err != nil || n != 0 {
		t.Errorf("partial frame: %d, %v", n, err)
	}
	lr.r = bytes.NewReader(data[10:])
	if _, err := lr.Copy(&stdout, &stderr); err != nil || stdout.String() != "hello world" {
		t.Errorf("after the rest: %q, %v", stdout.String(), err)
	}

	if _, err := NewLogReader(strings.NewReader("not a log at all")).Copy(&stdout, &stderr); err == nil {
		t.Error("read garbage as a log")
	}
}

type oneByte struct{ r io.Reader }

func (o *oneByte) Read(p []byte) (int, error) { return o.r.Read(p[:min(1, len(p))]) }
