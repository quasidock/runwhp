//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/rootfs"
	"github.com/quasidock/runwhp/internal/vmm"
)

// rootClient reaches a container's files for cp, export, diff and commit:
// through the runwhp running it, if it has a VM, or else through a guest
// booted with its storage, while this process holds its lock.
type rootClient struct {
	*rootfs.Client
	c   *container.Container
	ref string // as the user named it, for messages
	// release lets go of what openRoot took: the guest and the lock.
	release func()
	// booted says the client booted a guest for a stopped container.
	booted bool
}

// newRootClient words errors about c's files as Docker's daemon does.
func newRootClient(c *container.Container, ref string, dial rootfs.Dialer, release func(), booted bool) *rootClient {
	return &rootClient{
		Client: &rootfs.Client{
			Dial: dial,
			Name: "container " + c.Short(),
			NotFound: func(p string) error {
				return notFound("Could not find the file %s in container %s", p, ref)
			},
		},
		c: c, ref: ref, release: release, booted: booted,
	}
}

// openRoot reaches the files of the container named ref.
func openRoot(ctx context.Context, store *container.Store, root, ref string) (*rootClient, error) {
	c, err := store.Resolve(ref)
	if err != nil {
		return nil, err
	}
	for {
		switch c.State.Status {
		case container.Created, container.Running:
			return pipeRoot(root, c, ref), nil
		case container.Restarting:
			return nil, fmt.Errorf("container %s is restarting: wait until it is running", c.Short())
		case container.Creating:
		default:
			lock, err := store.Lock(c)
			if err == nil {
				// Nothing can start it now. Its state, as it is now.
				if cur, err := store.Get(c.ID); err == nil {
					c = cur
				}
				g, err := vmm.BootRootServer(ctx, root, c)
				if err != nil {
					lock.Close()
					return nil, err
				}
				return newRootClient(c, ref, func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
					conn, err := g.Dial(ctx)
					if err != nil {
						return nil, nil, fmt.Errorf("connecting to the guest serving container %s's files: %w", c.Short(), err)
					}
					return conn, bufio.NewReader(conn), nil
				}, func() {
					g.Close()
					lock.Close()
				}, true), nil
			}
			if !errors.Is(err, container.ErrRunning) {
				return nil, err
			}
			// Starting, or in another cp's hands: wait for either.
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if c, err = store.Resolve(c.ID); err != nil {
			return nil, err
		}
	}
}

// pipeRoot reaches the files of c, which has a VM, through its runwhp.
func pipeRoot(root string, c *container.Container, ref string) *rootClient {
	return newRootClient(c, ref, func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
		conn, err := container.Dial(ctx, root, c.ID)
		if errors.Is(err, container.ErrNoRunwhp) {
			return nil, nil, fmt.Errorf("container %s is not running", c.Short())
		}
		if err != nil {
			return nil, nil, err
		}
		rep, r, err := container.Exchange(conn, container.Request{Op: container.OpConnect, Port: guestspec.RootPort})
		if err == nil && !rep.OK {
			err = fmt.Errorf("container %s: %s", c.Short(), rep.Error)
		}
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		return conn, r, nil
	}, func() {}, false)
}

func (rc *rootClient) close() { rc.release() }

// busyOrStarted tells, once a start found c's lock held, whether another
// runwhp started it, which then records it as being created at once, or a
// cp, export, diff or commit holds it, which leaves it stopped.
func busyOrStarted(store *container.Store, c *container.Container) error {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		cur, err := store.Get(c.ID)
		if err != nil || cur.State.Active() {
			return nil
		}
	}
	return container.Refuse(container.ErrBusy, "container %s is busy: a cp, export, diff or commit is using it", c.Short())
}
