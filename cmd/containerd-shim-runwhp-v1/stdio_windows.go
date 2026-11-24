//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
)

// A process's standard streams come from containerd as paths:
//
//   - a named pipe, \\.\pipe\..., which the client (nerdctl, ctr) serves
//     and the shim dials: a foreground run's, and exec's;
//   - binary:///C:/path/to/logger.exe?key=value, a logging binary the
//     shim starts, handing it pipes of its own to read stdout and stderr
//     from, as containerd's logging protocol on Windows has it: nerdctl
//     run -d logs this way, through nerdctl itself;
//   - file:///C:/path, a file the output is appended to.
//
// An empty path is no stream.

const (
	dialWait   = 10 * time.Second // to dial the client's pipes
	loggerWait = 10 * time.Second // for a logging binary to start, and to end
)

// processIO is a process's standard streams as the shim opened them.
type processIO struct {
	stdin          *stdinReader // nil without input
	stdout, stderr io.Writer
	closers        []io.Closer
	logger         *exec.Cmd
	closeOnce      sync.Once
}

// openIO opens the streams of process id (a task's, or an exec's) of
// namespace ns. With a terminal, stdout carries the output, and stderr is
// unused.
func openIO(ns, id, stdin, stdout, stderr string, terminal bool) (_ *processIO, err error) {
	p := &processIO{stdout: io.Discard, stderr: io.Discard}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	if stdin != "" {
		r, err := openPath(stdin)
		if err != nil {
			return nil, fmt.Errorf("opening stdin %s: %w", stdin, err)
		}
		p.closers = append(p.closers, r)
		p.stdin = &stdinReader{r: r}
	}
	if u, ok := binaryURI(stdout); ok {
		if err := p.startLogger(ns, id, u); err != nil {
			return nil, err
		}
		return p, nil
	}
	if stdout != "" {
		w, err := openPath(stdout)
		if err != nil {
			return nil, fmt.Errorf("opening stdout %s: %w", stdout, err)
		}
		p.closers = append(p.closers, w)
		p.stdout = w
	}
	if stderr != "" && !terminal {
		w, err := openPath(stderr)
		if err != nil {
			return nil, fmt.Errorf("opening stderr %s: %w", stderr, err)
		}
		p.closers = append(p.closers, w)
		p.stderr = w
	}
	return p, nil
}

// openPath opens a named pipe the client serves, or a file:// URI.
func openPath(p string) (io.ReadWriteCloser, error) {
	if strings.HasPrefix(p, `\\.\pipe\`) || strings.HasPrefix(p, `//./pipe/`) {
		timeout := dialWait
		return winio.DialPipe(p, &timeout)
	}
	u, err := url.Parse(p)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "npipe":
		timeout := dialWait
		return winio.DialPipe(`\\.\pipe\`+strings.TrimPrefix(u.Path, "/"), &timeout)
	case "file":
		path, err := uriPath(u)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	return nil, fmt.Errorf("unsupported stream %q", p)
}

// binaryURI reports whether p names a logging binary.
func binaryURI(p string) (*url.URL, bool) {
	u, err := url.Parse(p)
	if err != nil || (u.Scheme != "binary" && u.Scheme != "binary-v2") {
		return nil, false
	}
	return u, true
}

// uriPath is the Windows path of a file or binary URI, whose path comes
// as /C:/x.
func uriPath(u *url.URL) (string, error) {
	p := filepath.Clean(strings.TrimPrefix(u.Path, "/"))
	if strings.HasPrefix(p, `\\`) || !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s: not a local absolute path", u.String())
	}
	return p, nil
}

// loggerArgs are a logging binary's arguments: each query key, followed by
// its value if it has one.
func loggerArgs(u *url.URL) []string {
	var args []string
	for k, vs := range u.Query() {
		args = append(args, k)
		if len(vs) > 0 && vs[0] != "" {
			args = append(args, vs[0])
		}
	}
	return args
}

// startLogger starts the logging binary u names, with pipes for stdout and
// stderr that it reads, and waits until it says it is ready by writing to
// the third, as containerd's logging protocol has it.
func (p *processIO) startLogger(ns, id string, u *url.URL) error {
	path, err := uriPath(u)
	if err != nil {
		return err
	}
	pipes := map[string]*servedPipe{}
	for _, name := range []string{"stdout", "stderr", "wait"} {
		sp, err := servePipe(fmt.Sprintf(`\\.\pipe\runwhp-binary-%s-%s-%s`, ns, id, name))
		if err != nil {
			return err
		}
		pipes[name] = sp
		if name != "wait" {
			p.closers = append(p.closers, sp)
		}
	}
	defer pipes["wait"].Close()
	cmd := exec.Command(path, loggerArgs(u)...)
	cmd.Env = append(os.Environ(),
		"CONTAINER_ID="+id,
		"CONTAINER_NAMESPACE="+ns,
		"CONTAINER_STDOUT="+pipes["stdout"].path,
		"CONTAINER_STDERR="+pipes["stderr"].path,
		"CONTAINER_WAIT="+pipes["wait"].path,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the logging binary %s: %w", path, err)
	}
	p.logger = cmd
	ready := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := pipes["wait"].Read(b[:])
		if errors.Is(err, io.EOF) {
			err = nil
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("the logging binary %s did not start: %w", path, err)
		}
	case <-time.After(loggerWait):
		return fmt.Errorf("the logging binary %s did not start within %v", path, loggerWait)
	}
	p.stdout, p.stderr = pipes["stdout"], pipes["stderr"]
	return nil
}

// close ends the streams: the client's reads of the output end, and a
// logging binary, once it has read the last of it, exits, or is killed
// after loggerWait.
func (p *processIO) close() {
	p.closeOnce.Do(func() {
		for _, c := range p.closers {
			c.Close()
		}
		if p.logger != nil {
			done := make(chan struct{})
			go func() { p.logger.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(loggerWait):
				diagf("the logging binary had not exited %v after its streams closed; killing it", loggerWait)
				p.logger.Process.Kill()
			}
		}
	})
}

// stdinReader is a process's input, which CloseIO ends before the client
// does.
type stdinReader struct {
	r      io.ReadCloser
	mu     sync.Mutex
	closed bool
}

func (s *stdinReader) Read(b []byte) (int, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	n, err := s.r.Read(b)
	s.mu.Lock()
	if s.closed {
		n, err = 0, io.EOF
	}
	s.mu.Unlock()
	return n, err
}

// close ends the input: a read under way returns end of file.
func (s *stdinReader) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.r.Close()
}

// servedPipe is a named pipe the shim serves to one client, a logging
// binary: reads and writes wait for it to connect.
type servedPipe struct {
	path      string
	l         net.Listener
	connected chan struct{}
	conn      net.Conn
	err       error
}

func servePipe(path string) (*servedPipe, error) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, err
	}
	p := &servedPipe{path: path, l: l, connected: make(chan struct{})}
	go func() {
		p.conn, p.err = l.Accept()
		close(p.connected)
	}()
	return p, nil
}

func (p *servedPipe) Read(b []byte) (int, error) {
	<-p.connected
	if p.err != nil {
		return 0, p.err
	}
	return p.conn.Read(b)
}

func (p *servedPipe) Write(b []byte) (int, error) {
	<-p.connected
	if p.err != nil {
		return 0, p.err
	}
	return p.conn.Write(b)
}

func (p *servedPipe) Close() error {
	p.l.Close()
	<-p.connected
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}
