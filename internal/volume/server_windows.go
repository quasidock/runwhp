//go:build windows

package volume

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// A shared volume's server.
//
// The server is a runwhp of its own, `runwhp __volume NAME`, whose guest
// has the volume's disk. It serves a named pipe, PipeName, on which each
// connection opens with a ServerRequest, a JSON line, answered by a
// ServerReply, another. A connect request's connection then carries FUSE
// frames, as package fuse's Forwarder and ServeStream speak them, between a
// container's runwhp and the server's guest, for as long as the container's
// guest runs; the server stops a while after its last such connection
// ends.

// Server requests' ops.
const (
	OpConnect = "connect"
	OpStatus  = "status"
	OpStop    = "stop"
)

// ServerRequest opens a connection to a volume's server.
type ServerRequest struct {
	Op string `json:"op"`
	// Container is the container connecting, for status.
	Container string `json:"container,omitempty"`
}

// ServerReply answers a ServerRequest, and is the line a server starting
// writes on its ready handle.
type ServerReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Busy says another server serves the volume, or is starting; Held,
	// that a container's runwhp has its disk.
	Busy bool `json:"busy,omitempty"`
	Held bool `json:"held,omitempty"`
	// For status: the server's process, and the containers it serves.
	PID        int      `json:"pid,omitempty"`
	Clients    int      `json:"clients"`
	Containers []string `json:"containers,omitempty"`
}

// ErrNoServer means no server serves the volume.
var ErrNoServer = errors.New("no server serves the volume")

// ErrServerBusy means another server holds the volume's server lock.
var ErrServerBusy = errors.New("another server holds the volume")

// PipeName is the pipe the server of the volume called name, in the store
// at root, serves: unique to the store and the volume, whose names are
// case-insensitive.
func PipeName(root, name string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return `\\.\pipe\runwhp-` + hex.EncodeToString(sum[:8]) + "-vol-" + strings.ToLower(name)
}

// storeRoot is the store the volume is in.
func (v *Volume) storeRoot() string { return filepath.Dir(filepath.Dir(v.dir)) }

// PipeName is the pipe the volume's server serves.
func (v *Volume) PipeName() string { return PipeName(v.storeRoot(), v.Name) }

// Dir is the volume's directory, where its server keeps its lock and log.
func (v *Volume) Dir() string { return v.dir }

// ask sends req to the volume's server and reads its reply, returning the
// connection, positioned after the reply.
func (v *Volume) ask(ctx context.Context, req ServerRequest) (net.Conn, ServerReply, error) {
	c, err := winio.DialPipeContext(ctx, v.PipeName())
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, ServerReply{}, ErrNoServer
	}
	if err != nil {
		return nil, ServerReply{}, err
	}
	if d, ok := ctx.Deadline(); ok {
		c.SetDeadline(d)
	}
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		c.Close()
		return nil, ServerReply{}, err
	}
	var rep ServerReply
	if err := readLine(c, &rep); err != nil {
		c.Close()
		return nil, ServerReply{}, fmt.Errorf("volume %s's server: %w", v.Name, err)
	}
	c.SetDeadline(time.Time{})
	return c, rep, nil
}

// readLine reads one JSON line from r into v, a byte at a time, so that
// nothing after it is read.
func readLine(r io.Reader, v any) error {
	var line []byte
	var b [1]byte
	for {
		if _, err := r.Read(b[:]); err != nil {
			return err
		}
		if b[0] == '\n' {
			break
		}
		if len(line) > 64<<10 {
			return errors.New("reply too long")
		}
		line = append(line, b[0])
	}
	return json.Unmarshal(line, v)
}

// ReadReply reads a ServerReply from r, as WriteReply writes it.
func ReadReply(r io.Reader) (ServerReply, error) {
	var rep ServerReply
	err := readLine(r, &rep)
	return rep, err
}

// ReadRequest reads a ServerRequest from r.
func ReadRequest(r io.Reader) (ServerRequest, error) {
	var req ServerRequest
	err := readLine(r, &req)
	return req, err
}

// WriteReply writes rep as a JSON line.
func WriteReply(w io.Writer, rep ServerReply) error {
	line, _ := json.Marshal(rep)
	_, err := w.Write(append(line, '\n'))
	return err
}

// Connect opens a connection to the volume's server for container id,
// which then carries FUSE frames. It returns ErrNoServer if none serves the
// volume.
func (v *Volume) Connect(ctx context.Context, id string) (net.Conn, error) {
	c, rep, err := v.ask(ctx, ServerRequest{Op: OpConnect, Container: id})
	if err != nil {
		return nil, err
	}
	if !rep.OK {
		c.Close()
		if rep.Busy {
			// The server is stopping.
			return nil, ErrNoServer
		}
		return nil, fmt.Errorf("volume %s's server: %s", v.Name, rep.Error)
	}
	return c, nil
}

// ServerStatus asks the volume's server how it is.
func (v *Volume) ServerStatus() (ServerReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, rep, err := v.ask(ctx, ServerRequest{Op: OpStatus})
	if err != nil {
		return ServerReply{}, err
	}
	c.Close()
	return rep, nil
}

// StopIdleServer stops the volume's server if it serves no container, and
// waits until it has let go of the disk. It reports whether a server
// stopped.
func (v *Volume) StopIdleServer() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, rep, err := v.ask(ctx, ServerRequest{Op: OpStop})
	if err != nil {
		return false
	}
	c.Close()
	return rep.OK
}

// LockServer takes the volume's server lock, which the server holds for
// as long as it runs, so that two never serve one volume. It returns
// ErrServerBusy if another holds it.
func (v *Volume) LockServer() (io.Closer, error) {
	path, err := windows.UTF16PtrFromString(filepath.Join(v.dir, "server.lock"))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, ErrServerBusy
	}
	if err != nil {
		return nil, fmt.Errorf("volume %s's server lock: %w", v.Name, err)
	}
	return handleCloser(h), nil
}

type handleCloser windows.Handle

func (h handleCloser) Close() error { return windows.CloseHandle(windows.Handle(h)) }
