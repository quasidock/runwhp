package vmm

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/virtio"
)

// pipeDialTimeout bounds how long a connection from the guest waits for
// its pipe, a busy one's server included. A pipe with no server fails at
// once. It is Docker's client's own, as a busy machine's server makes 100
// connections at once wait past 2 s for their instances.
var pipeDialTimeout = 10 * time.Second

// pipeWarnEvery spaces the log's lines about a pipe that cannot be
// reached, which a client trying again and again would otherwise fill.
const pipeWarnEvery = time.Minute

// spliceBuffer is how much a forwarded connection reads at a time, each
// way: a stream's output, an attach's, is read off the pipe in large
// pieces, as fast as its server writes them.
const spliceBuffer = 256 << 10

// socketToken makes a boot's Spec.SocketToken.
func socketToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// guestConn is the guest's side of a forwarded connection.
type guestConn interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// pipeForwarder carries the connections init makes for MountSocket mounts
// to the mounts' named pipes on the host.
type pipeForwarder struct {
	token  string
	mounts []guestspec.Mount
	dial   func(ctx context.Context, pipe string) (net.Conn, error)
	logf   func(string, ...any)

	mu     sync.Mutex
	warned map[string]time.Time
}

// hasSockets reports whether any of mounts is a socket.
func hasSockets(mounts []guestspec.Mount) bool {
	for _, m := range mounts {
		if m.Kind == guestspec.MountSocket {
			return true
		}
	}
	return false
}

func newPipeForwarder(token string, mounts []guestspec.Mount, logf func(string, ...any)) *pipeForwarder {
	return &pipeForwarder{token: token, mounts: mounts, logf: logf, warned: map[string]time.Time{},
		dial: func(ctx context.Context, pipe string) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) }}
}

// listen takes the guest's connections to PipePort on vsock until the
// device is drained.
func (f *pipeForwarder) listen(v *virtio.Vsock) error {
	l, err := v.Listen(guestspec.PipePort)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return nil
}

// serve reads a connection's hello, connects to its mount's pipe, answers,
// and carries bytes both ways.
func (f *pipeForwarder) serve(c guestConn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadSlice('\n')
	if err != nil {
		f.logf("pipes: a connection from the guest sent no hello: %v", err)
		return
	}
	var hello guestspec.SocketHello
	if err := json.Unmarshal(line, &hello); err != nil || hello.Token != f.token || f.token == "" {
		f.logf("pipes: refused a connection from the guest that is not init's")
		return
	}
	if hello.Mount < 0 || hello.Mount >= len(f.mounts) || f.mounts[hello.Mount].Kind != guestspec.MountSocket {
		f.logf("pipes: refused a connection for mount %d, which is no socket", hello.Mount)
		return
	}
	m := f.mounts[hello.Mount]
	ctx, cancel := context.WithTimeout(context.Background(), pipeDialTimeout)
	p, err := f.dial(ctx, m.Pipe)
	cancel()
	if err != nil {
		f.warn(m, err)
		c.Write([]byte{guestspec.SocketOK + 1})
		return
	}
	defer p.Close()
	if _, err := c.Write([]byte{guestspec.SocketOK}); err != nil {
		return
	}
	splicePipe(c, r, p)
}

// warn logs that m's pipe could not be reached, at most once a minute for
// each pipe.
func (f *pipeForwarder) warn(m guestspec.Mount, err error) {
	f.mu.Lock()
	last, seen := f.warned[m.Pipe]
	now := time.Now()
	if seen && now.Sub(last) < pipeWarnEvery {
		f.mu.Unlock()
		return
	}
	f.warned[m.Pipe] = now
	f.mu.Unlock()
	f.logf("pipes: %s in the container cannot reach %s: %v%s", m.Target, m.Pipe, err, pipeHint(m.Pipe))
}

// pipeHint says how to serve a pipe runwhp's own server serves.
func pipeHint(pipe string) string {
	if isEnginePipe(pipe) {
		return "; is runwhp's Engine API running? Start it with runwhp docker-api start"
	}
	return ""
}

// isEnginePipe reports whether pipe is one of runwhp's Engine API's.
func isEnginePipe(pipe string) bool {
	const prefix = `\\.\pipe\runwhp_engine`
	return len(pipe) >= len(prefix) && pipe[:len(prefix)] == prefix
}

// splicePipe carries bytes between the guest's connection, whose first
// bytes past the hello wait in r, and the pipe p, until both directions
// have ended. Each side's half-close reaches the other where it can: a
// pipe in message mode carries one, as a zero-length message; one in byte
// mode does not, and only its end, or the guest's, ends the connection.
func splicePipe(c guestConn, r io.Reader, p net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			p.Close()
			c.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.CopyBuffer(p, r, make([]byte, spliceBuffer))
		if pc, ok := p.(interface{ CloseWrite() error }); ok && err == nil {
			err = pc.CloseWrite()
		}
		if err != nil {
			closeBoth()
		}
	}()
	go func() {
		defer wg.Done()
		_, err := io.CopyBuffer(c, p, make([]byte, spliceBuffer))
		if err == nil {
			err = c.CloseWrite()
		}
		if err != nil {
			closeBoth()
		}
	}()
	wg.Wait()
}
