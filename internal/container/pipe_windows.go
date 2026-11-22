//go:build windows

package container

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// PipeName is the named pipe the runwhp running a container serves, unique
// to the store and the container.
func PipeName(root, id string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return `\\.\pipe\runwhp-` + hex.EncodeToString(sum[:8]) + "-" + id
}

// Listen serves a container's pipe. Only this user, and SYSTEM, can
// connect, and only from this machine. A second runwhp cannot serve the
// same pipe.
func Listen(root, id string) (net.Listener, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + u.User.Sid.String() + ")"
	return winio.ListenPipe(PipeName(root, id), &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

// ErrNoRunwhp means no runwhp serves the container's pipe: the container
// has no VM.
var ErrNoRunwhp = errors.New("no runwhp is running the container")

// Dial connects to the pipe of the runwhp running the container.
func Dial(ctx context.Context, root, id string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := winio.DialPipeContext(ctx, PipeName(root, id))
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, ErrNoRunwhp
	}
	return c, err
}

// Ask sends one request to the runwhp running the container and returns
// its reply. Requests that wait for the container, such as a stop, wait as
// long as ctx allows.
func Ask(ctx context.Context, root, id string, req Request) (Reply, error) {
	c, err := Dial(ctx, root, id)
	if err != nil {
		return Reply{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	rep, _, err := Exchange(c, req)
	return rep, err
}

// Exchange sends req on c and reads the reply. The reader it returns holds
// whatever followed the reply, for connect requests.
func Exchange(c net.Conn, req Request) (Reply, *bufio.Reader, error) {
	req.V = ProtocolVersion
	line, err := json.Marshal(req)
	if err != nil {
		return Reply{}, nil, err
	}
	if _, err := c.Write(append(line, '\n')); err != nil {
		return Reply{}, nil, err
	}
	r := bufio.NewReader(c)
	data, err := r.ReadBytes('\n')
	if err != nil {
		return Reply{}, nil, fmt.Errorf("the container's runwhp did not answer: %w", err)
	}
	var rep Reply
	if err := json.Unmarshal(data, &rep); err != nil {
		return Reply{}, nil, fmt.Errorf("bad reply from the container's runwhp: %w", err)
	}
	return rep, r, nil
}
