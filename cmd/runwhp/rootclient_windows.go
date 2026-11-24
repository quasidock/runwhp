//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/vmm"
)

// rootClient reaches a container's files for cp, export, diff and commit:
// through the runwhp running it, if it has a VM, or else through a guest
// booted with its storage, while this process holds its lock.
type rootClient struct {
	c    *container.Container
	ref  string // as the user named it, for messages
	dial func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error)
	// release lets go of what openRoot took: the guest and the lock.
	release func()
	// booted says the client booted a guest for a stopped container.
	booted bool
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
				return &rootClient{c: c, ref: ref, booted: true,
					dial: func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
						conn, err := g.Dial(ctx)
						if err != nil {
							return nil, nil, fmt.Errorf("connecting to the guest serving container %s's files: %w", c.Short(), err)
						}
						return conn, bufio.NewReader(conn), nil
					},
					release: func() {
						g.Close()
						lock.Close()
					},
				}, nil
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
	return &rootClient{c: c, ref: ref, release: func() {},
		dial: func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
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
		}}
}

func (rc *rootClient) close() { rc.release() }

// request sends req, and returns the connection and init's reply.
func (rc *rootClient) request(ctx context.Context, req guestspec.RootRequest) (io.ReadWriteCloser, *bufio.Reader, error) {
	conn, r, err := rc.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := rc.readReply(r, req.Path, nil); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, r, nil
}

// readReply reads one of init's replies into rep, if set, and returns its
// error, worded as Docker's daemon words it.
func (rc *rootClient) readReply(r *bufio.Reader, p string, rep *guestspec.RootReply) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("container %s's file server did not answer: %w", rc.c.Short(), err)
	}
	var got guestspec.RootReply
	if err := json.Unmarshal(line, &got); err != nil {
		return fmt.Errorf("bad answer from container %s's file server: %w", rc.c.Short(), err)
	}
	if rep != nil {
		*rep = got
	}
	switch {
	case got.NotFound:
		return fmt.Errorf("Could not find the file %s in container %s", p, rc.ref)
	case got.Error != "":
		return errors.New(got.Error)
	}
	return nil
}

// stat describes the container's path p.
func (rc *rootClient) stat(ctx context.Context, p string) (*guestspec.PathStat, error) {
	p = slashPath(p)
	conn, r, err := rc.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	line, _ := json.Marshal(guestspec.RootRequest{Op: guestspec.RootStat, Path: p})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	var rep guestspec.RootReply
	if err := rc.readReply(r, p, &rep); err != nil {
		return nil, err
	}
	return rep.Stat, nil
}

// stream is what follows init's reply: a tar, until init closes the
// connection, checked for its end marker, as init ends a tar it cannot
// finish.
type stream struct {
	io.Reader
	conn io.Closer
}

func (s *stream) Close() error { return s.conn.Close() }

// get returns a tar of the container's path p, and p's stat.
func (rc *rootClient) get(ctx context.Context, p string) (io.ReadCloser, *guestspec.PathStat, error) {
	p = slashPath(p)
	conn, r, err := rc.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	line, _ := json.Marshal(guestspec.RootRequest{Op: guestspec.RootGet, Path: p})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	var rep guestspec.RootReply
	if err := rc.readReply(r, p, &rep); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return &stream{&tarChecker{r: r}, conn}, rep.Stat, nil
}

// put extracts the tar content into the container's directory dir.
func (rc *rootClient) put(ctx context.Context, dir string, content io.Reader, noOverwriteDirNonDir, copyUIDGID bool) error {
	dir = slashPath(dir)
	conn, r, err := rc.request(ctx, guestspec.RootRequest{Op: guestspec.RootPut, Path: dir, NoOverwriteDirNonDir: noOverwriteDirNonDir, CopyUIDGID: copyUIDGID})
	if err != nil {
		return err
	}
	defer conn.Close()
	// Init reads the tar to its end, then answers, or answers sooner with
	// an error. Either way its answer settles it, and closing the
	// connection ends the copy.
	go io.Copy(conn, content)
	return rc.readReply(r, dir, nil)
}

// tarOf asks for a tar the container's file server makes: an export or
// the writable layer's diff.
func (rc *rootClient) tarOf(ctx context.Context, op string) (io.ReadCloser, error) {
	conn, r, err := rc.request(ctx, guestspec.RootRequest{Op: op})
	if err != nil {
		return nil, err
	}
	return &stream{&tarChecker{r: r}, conn}, nil
}

// changes lists what the container's writable layer changed.
func (rc *rootClient) changes(ctx context.Context) ([]guestspec.Change, error) {
	conn, r, err := rc.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	line, _ := json.Marshal(guestspec.RootRequest{Op: guestspec.RootChanges})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	var rep guestspec.RootReply
	if err := rc.readReply(r, "", &rep); err != nil {
		return nil, err
	}
	return rep.Changes, nil
}

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
	return fmt.Errorf("container %s is busy: a cp, export, diff or commit is using it", c.Short())
}

// slashPath writes a path in the container as Linux does: Windows'
// path functions, which go-archive's copy rules go through, turn its
// slashes into backslashes.
func slashPath(p string) string { return filepath.ToSlash(p) }

// tarChecker passes a tar on, and fails if it ends before its end marker:
// init ends a tar it cannot finish.
type tarChecker struct {
	r    io.Reader
	tail []byte // the last bytes read
}

func (t *tarChecker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.tail = append(t.tail, p[:n]...)
	if len(t.tail) > 1024 {
		t.tail = t.tail[len(t.tail)-1024:]
	}
	if errors.Is(err, io.EOF) && !bytes.Equal(t.tail, make([]byte, 1024)) {
		return n, fmt.Errorf("the container's file server ended the tar early: %w", io.ErrUnexpectedEOF)
	}
	return n, err
}
