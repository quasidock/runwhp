//go:build windows

package network

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"

	"github.com/Microsoft/go-winio"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/netswitch"
)

// pipeBuffer is the size of a link's pipe buffers, which lets a burst of
// frames go without the reader waking for each.
const pipeBuffer = 1 << 20

// Listen takes links to a container's endpoints, at a pipe of this boot's
// own, whose name it returns for the endpoints: a stale endpoint can never
// reach a later boot. Only this user can connect.
func Listen(root, cid string) (net.Listener, string, error) {
	var b [8]byte
	rand.Read(b[:])
	name := container.PipeName(root, cid) + "-net-" + hex.EncodeToString(b[:])
	sddl, err := container.PipeSecurity()
	if err != nil {
		return nil, "", err
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: sddl, InputBufferSize: pipeBuffer, OutputBufferSize: pipeBuffer})
	return ln, name, err
}

// Dialer connects links to peers on network id, at the pipe their
// endpoint names when the link dials.
func (s *Store) Dialer(id string) func(ctx context.Context, p netswitch.Peer) (io.ReadWriteCloser, error) {
	return func(ctx context.Context, p netswitch.Peer) (io.ReadWriteCloser, error) {
		e, ok := s.EndpointOf(id, p.Container)
		if !ok || e.IP != p.IP {
			return nil, errGone
		}
		return winio.DialPipeContext(ctx, e.Pipe)
	}
}

var errGone = goneError{}

type goneError struct{}

func (goneError) Error() string { return "the container is no longer on the network" }
