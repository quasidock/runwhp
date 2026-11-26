//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/vmm"
)

// A process runs one guest at a time, and the API server none: what boots
// a guest, a build, a save or push, or the files of a stopped container,
// runs in a runwhp __api-job of its own. It reads a jobHeader line from its
// stdin, then the request's body, if any, and writes frames, in the log's
// framing, to its stdout: jobData, the response's bytes; jobMessage, a JSON
// message for a stream of them; and jobResult, last, a jobResult.
const (
	jobData    = 1
	jobMessage = 2
	jobResult  = 3
)

type jobHeader struct {
	Kind string          `json:"kind"`
	Args json.RawMessage `json:"args,omitempty"`
}

type jobResultFrame struct {
	OK     bool            `json:"ok"`
	Status int             `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// jobFunc does a job: it reads args, and the request's body from in, and
// writes what it answers to out. What it returns goes in the result.
// jobSink takes what a job answers: its bytes, and its messages.
type jobSink interface {
	io.Writer
	Message(line []byte) error
}

type jobFunc func(ctx context.Context, root string, args json.RawMessage, in io.Reader, out jobSink) (any, error)

// apiJobs are the jobs runwhp __api-job does, by kind; the files that
// serve each endpoint add theirs.
var apiJobs = map[string]jobFunc{}

// jobOutput writes a job's frames.
type jobOutput struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (o *jobOutput) frame(kind byte, p []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(p) > 0 || kind != jobData {
		n := min(len(p), 1<<20)
		if err := container.WriteFrame(o.w, kind, p[:n]); err != nil {
			return err
		}
		p = p[n:]
		if kind != jobData {
			break
		}
	}
	return o.w.Flush()
}

// Write writes the response's bytes.
func (o *jobOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := o.frame(jobData, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Message writes a JSON message, a line of a stream the server answers
// with.
func (o *jobOutput) Message(line []byte) error { return o.frame(jobMessage, line) }

// cmdAPIJob is runwhp __api-job.
func cmdAPIJob(args []string) int {
	if len(args) != 0 {
		return 2
	}
	// runwhp's own messages go to stderr, which the server logs.
	vmm.Diag = os.Stderr
	out := &jobOutput{w: bufio.NewWriterSize(os.Stdout, 64<<10)}
	finish := func(v any, err error) int {
		res := jobResultFrame{OK: err == nil}
		if err != nil {
			res.Status, res.Error = httpStatus(err), err.Error()
		} else if v != nil {
			data, merr := json.Marshal(v)
			if merr != nil {
				res = jobResultFrame{Status: 500, Error: merr.Error()}
			}
			res.Result = data
		}
		data, _ := json.Marshal(res)
		if out.frame(jobResult, data) != nil {
			return 1
		}
		return 0
	}
	in := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return finish(nil, fmt.Errorf("reading the job: %w", err))
	}
	var h jobHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return finish(nil, fmt.Errorf("reading the job: %w", err))
	}
	f, ok := apiJobs[h.Kind]
	if !ok {
		return finish(nil, fmt.Errorf("this runwhp does not do jobs of kind %q", h.Kind))
	}
	root, err := storeRoot()
	if err != nil {
		return finish(nil, err)
	}
	// The server kills the job when its client goes; nothing else ends it.
	return finish(f(context.Background(), root, h.Args, in, out))
}

// jobRunner starts jobs for the server, in a job object that ends them
// with it.
type jobRunner struct {
	s   *apiServer
	exe string
	job windows.Handle
}

func newJobRunner(s *apiServer) (*jobRunner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	job, err := newContainerdJob()
	if err != nil {
		return nil, err
	}
	return &jobRunner{s: s, exe: exe, job: job}, nil
}

func (j *jobRunner) close() {
	// Closing the last handle ends the jobs still running.
	windows.CloseHandle(j.job)
}

// jobError is a job's failure, with the status it carries.
type jobError struct {
	status int
	msg    string
}

func (e *jobError) Error() string { return e.msg }

// run does a job of kind with args in a runwhp of its own, giving it body,
// if not nil, as its input, and handing each data frame to data and each
// message to message, either of which may be nil. It returns the job's
// result. If ctx ends first, the job is killed.
func (j *jobRunner) run(ctx context.Context, kind string, args any, body io.Reader, data io.Writer, message func([]byte) error) (json.RawMessage, error) {
	argData, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	header, _ := json.Marshal(jobHeader{Kind: kind, Args: argData})
	cmd := exec.Command(j.exe, "__api-job")
	cmd.Dir = j.s.root
	cmd.Stderr = &logWriter{s: j.s, prefix: kind + " job: "}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := startInJob(cmd, j.job); err != nil {
		return nil, fmt.Errorf("starting runwhp for a %s job: %w", kind, err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
		case <-j.s.ctx.Done():
			cmd.Process.Kill()
		case <-done:
		}
	}()
	go func() {
		defer stdin.Close()
		if _, err := stdin.Write(append(header, '\n')); err != nil || body == nil {
			return
		}
		io.Copy(stdin, body)
	}()
	r := bufio.NewReaderSize(stdout, 64<<10)
	var result *jobResultFrame
	var sinkErr error
	for result == nil {
		kind, p, err := container.ReadFrame(r)
		if err != nil {
			break
		}
		switch kind {
		case jobData:
			if data != nil && sinkErr == nil {
				_, sinkErr = data.Write(p)
			}
		case jobMessage:
			if message != nil && sinkErr == nil {
				sinkErr = message(p)
			}
		case jobResult:
			result = &jobResultFrame{}
			if err := json.Unmarshal(p, result); err != nil {
				result = &jobResultFrame{Status: 500, Error: "the job's result is unreadable: " + err.Error()}
			}
		}
		if sinkErr != nil {
			// The client went away: there is no one to answer.
			cmd.Process.Kill()
			break
		}
	}
	werr := cmd.Wait()
	switch {
	case sinkErr != nil:
		return nil, sinkErr
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case result == nil:
		return nil, fmt.Errorf("runwhp's %s job ended without a result (%v); see %s", kind, werr, j.s.logPath())
	case !result.OK:
		status := result.Status
		if status == 0 {
			status = 500
		}
		return nil, &apiError{code: status, msg: result.Error}
	}
	return result.Result, nil
}

func (s *apiServer) logPath() string { return apiDir(s.root) + `\server.log` }

// logWriter writes what a job prints to the server's log, a line at a
// time.
type logWriter struct {
	s      *apiServer
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := indexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.s.logf("%s%s", l.prefix, trimCR(string(l.buf[:i])))
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 {
		l.s.logf("%s%s", l.prefix, string(l.buf))
		l.buf = nil
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func trimCR(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}
