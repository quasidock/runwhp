package vmm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// guestPair gives the forwarder a connection as init's would reach it, and
// returns the other end, init's.
func guestPair(t *testing.T, f *pipeForwarder) *net.TCPConn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		c, err := l.Accept()
		close(accepted)
		if err == nil {
			f.serve(c.(*net.TCPConn))
		}
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Not before the accept: closing a listener resets what it has not
	// accepted.
	<-accepted
	l.Close()
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return c.(*net.TCPConn)
}

func hello(t *testing.T, c net.Conn, token string, mount int) {
	t.Helper()
	b, _ := json.Marshal(guestspec.SocketHello{Token: token, Mount: mount})
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
	l.mu.Unlock()
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func testPipe(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\runwhp-test-%d-%s`, os.Getpid(), strings.ReplaceAll(t.Name(), "/", "-"))
}

// echoServer answers each connection with "got:" and what it read until
// the client's end of input, then half-closes, and in message mode reads
// on to the client's close.
func echoServer(t *testing.T, name string, message bool) {
	t.Helper()
	l, err := winio.ListenPipe(name, &winio.PipeConfig{MessageMode: message})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if !message {
					// No half-close in byte mode: a line is the request.
					line, err := bufio.NewReader(c).ReadString('\n')
					if err == nil {
						io.WriteString(c, "got:"+line)
					}
					return
				}
				data, err := io.ReadAll(c)
				if err != nil {
					return
				}
				io.WriteString(c, "got:"+string(data))
				c.(interface{ CloseWrite() error }).CloseWrite()
			}()
		}
	}()
}

func TestPipeForwarder(t *testing.T) {
	msg, byteMode := testPipe(t)+"-msg", testPipe(t)+"-byte"
	echoServer(t, msg, true)
	echoServer(t, byteMode, false)
	var log logLines
	mounts := []guestspec.Mount{
		{Kind: guestspec.MountSocket, Target: "/var/run/docker.sock", Pipe: msg},
		{Kind: guestspec.MountTmpfs, Target: "/tmp"},
		{Kind: guestspec.MountSocket, Target: "/run/agent.sock", Pipe: byteMode},
		{Kind: guestspec.MountSocket, Target: "/run/none.sock", Pipe: `\\.\pipe\runwhp_engine-absent-test`},
	}
	f := newPipeForwarder("t0ken", mounts, log.logf)

	t.Run("message mode, half-closed both ways", func(t *testing.T) {
		c := guestPair(t, f)
		hello(t, c, "t0ken", 0)
		io.WriteString(c, "ping")
		c.CloseWrite()
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "\x00got:ping" {
			t.Errorf("read %q, %v", got, err)
		}
	})
	t.Run("byte mode", func(t *testing.T) {
		c := guestPair(t, f)
		hello(t, c, "t0ken", 2)
		io.WriteString(c, "ping\n")
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "\x00got:ping\n" {
			t.Errorf("read %q, %v", got, err)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := guestPair(t, f)
				hello(t, c, "t0ken", 0)
				fmt.Fprintf(c, "n%d", i)
				c.CloseWrite()
				if got, _ := io.ReadAll(c); string(got) != fmt.Sprintf("\x00got:n%d", i) {
					t.Errorf("connection %d read %q", i, got)
				}
			}()
		}
		wg.Wait()
		if t.Failed() {
			t.Logf("log:\n%s", log.String())
		}
	})
	for _, tc := range []struct {
		name, token string
		mount       int
		want        string
	}{
		{"a bad token", "nope", 0, "not init's"},
		{"no token", "", 0, "not init's"},
		{"a mount that is no socket", "t0ken", 1, "no socket"},
		{"a mount past the last", "t0ken", 9, "no socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := guestPair(t, f)
			hello(t, c, tc.token, tc.mount)
			if got, err := io.ReadAll(c); len(got) != 0 || err != nil {
				t.Errorf("read %q, %v: want the connection closed unanswered", got, err)
			}
			if !strings.Contains(log.String(), tc.want) {
				t.Errorf("log %q has no %q", log.String(), tc.want)
			}
		})
	}
	t.Run("a pipe nothing serves", func(t *testing.T) {
		for range 2 {
			c := guestPair(t, f)
			hello(t, c, "t0ken", 3)
			if got, err := io.ReadAll(c); len(got) != 1 || got[0] == guestspec.SocketOK || err != nil {
				t.Errorf("read %q, %v: want a refusal", got, err)
			}
		}
		if n := strings.Count(log.String(), "cannot reach"); n != 1 {
			t.Errorf("warned %d times, want once a minute:\n%s", n, log.String())
		}
		if !strings.Contains(log.String(), "docker-api start") {
			t.Errorf("no hint for the Engine API's pipe: %s", log.String())
		}
	})
}
