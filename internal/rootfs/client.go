// Package rootfs is the host's client of init's root file server, which
// serves a guest's root on guestspec.RootPort: cp, export, diff and commit
// reach a container's files through it, and build reaches its steps'.
//
// Each request is a connection of its own: the host sends a
// guestspec.RootRequest as a JSON line, init answers with a
// guestspec.RootReply line, and a tar follows, for the operations that
// send or take one, until one side closes the connection.
package rootfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// Dialer connects to a root file server. The reader reads what follows
// on the connection; it may hold bytes the dialer read past its own
// handshake.
type Dialer func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error)

// Client reaches one guest's root.
type Client struct {
	Dial Dialer
	// Name names the root in errors: "container 0123456789ab", say.
	Name string
	// NotFound, if set, words the error for a path that does not exist.
	NotFound func(path string) error
}

// PutOptions says how Put extracts a tar.
type PutOptions struct {
	// NoOverwriteDirNonDir refuses to replace a directory with a
	// non-directory, or the other way round, as docker cp does.
	NoOverwriteDirNonDir bool
	// CopyUIDGID gives what is extracted to the container's user.
	CopyUIDGID bool
	// MakeDir, Chown, ChownParents and Merge are build's, as
	// guestspec.RootRequest describes them.
	MakeDir      bool
	Chown        string
	ChownParents bool
	Merge        bool
}

// request sends req, and returns the connection and init's reply.
func (c *Client) request(ctx context.Context, req guestspec.RootRequest, rep *guestspec.RootReply) (io.ReadWriteCloser, *bufio.Reader, error) {
	conn, r, err := c.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := c.readReply(r, req.Path, rep); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, r, nil
}

// readReply reads one of init's replies into rep, if set, and returns its
// error.
func (c *Client) readReply(r *bufio.Reader, p string, rep *guestspec.RootReply) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%s's file server did not answer: %w", c.Name, err)
	}
	var got guestspec.RootReply
	if err := json.Unmarshal(line, &got); err != nil {
		return fmt.Errorf("bad answer from %s's file server: %w", c.Name, err)
	}
	if rep != nil {
		*rep = got
	}
	switch {
	case got.NotFound:
		if c.NotFound != nil {
			return c.NotFound(p)
		}
		return &NotFoundError{Path: p}
	case got.Error != "":
		return errors.New(got.Error)
	}
	return nil
}

// NotFoundError is a path the root lacks.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("%q: not found", e.Path) }

// Stat describes the path p.
func (c *Client) Stat(ctx context.Context, p string) (*guestspec.PathStat, error) {
	var rep guestspec.RootReply
	conn, _, err := c.request(ctx, guestspec.RootRequest{Op: guestspec.RootStat, Path: Slash(p)}, &rep)
	if err != nil {
		return nil, err
	}
	conn.Close()
	return rep.Stat, nil
}

// Glob lists the paths matching pattern, as filepath.Glob matches it in
// the root, sorted; none is no error.
func (c *Client) Glob(ctx context.Context, pattern string) ([]string, error) {
	var rep guestspec.RootReply
	conn, _, err := c.request(ctx, guestspec.RootRequest{Op: guestspec.RootGlob, Path: Slash(pattern)}, &rep)
	if err != nil {
		return nil, err
	}
	conn.Close()
	return rep.Matches, nil
}

// stream is what follows init's reply: a tar, until init closes the
// connection, checked for its end marker, as init ends a tar it cannot
// finish.
type stream struct {
	io.Reader
	conn io.Closer
}

func (s *stream) Close() error { return s.conn.Close() }

// Get returns a tar of the path p, and p's stat.
func (c *Client) Get(ctx context.Context, p string) (io.ReadCloser, *guestspec.PathStat, error) {
	var rep guestspec.RootReply
	conn, r, err := c.request(ctx, guestspec.RootRequest{Op: guestspec.RootGet, Path: Slash(p)}, &rep)
	if err != nil {
		return nil, nil, err
	}
	return &stream{CheckTar(r), conn}, rep.Stat, nil
}

// Put extracts the tar content into the directory dir.
func (c *Client) Put(ctx context.Context, dir string, content io.Reader, o PutOptions) error {
	dir = Slash(dir)
	conn, r, err := c.request(ctx, guestspec.RootRequest{
		Op: guestspec.RootPut, Path: dir,
		NoOverwriteDirNonDir: o.NoOverwriteDirNonDir, CopyUIDGID: o.CopyUIDGID,
		MakeDir: o.MakeDir, Chown: o.Chown, ChownParents: o.ChownParents, Merge: o.Merge,
	}, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Init reads the tar to its end, then answers, or answers sooner with
	// an error. Either way its answer settles it, and closing the
	// connection ends the copy.
	go io.Copy(conn, content)
	return c.readReply(r, dir, nil)
}

// Tar asks for a tar the file server makes: guestspec.RootExport or
// guestspec.RootDiff.
func (c *Client) Tar(ctx context.Context, op string) (io.ReadCloser, error) {
	conn, r, err := c.request(ctx, guestspec.RootRequest{Op: op}, nil)
	if err != nil {
		return nil, err
	}
	return &stream{CheckTar(r), conn}, nil
}

// Changes lists what the writable layer changed.
func (c *Client) Changes(ctx context.Context) ([]guestspec.Change, error) {
	var rep guestspec.RootReply
	conn, _, err := c.request(ctx, guestspec.RootRequest{Op: guestspec.RootChanges}, &rep)
	if err != nil {
		return nil, err
	}
	conn.Close()
	return rep.Changes, nil
}

// Slash writes a path in the guest as Linux does: Windows' path
// functions, which go-archive's copy rules go through, turn its slashes
// into backslashes.
func Slash(p string) string { return filepath.ToSlash(p) }

// CheckTar passes a tar on, and fails if it ends before its end marker:
// init ends a tar it cannot finish.
func CheckTar(r io.Reader) io.Reader { return &tarChecker{r: r} }

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
		return n, fmt.Errorf("the file server ended the tar early: %w", io.ErrUnexpectedEOF)
	}
	return n, err
}
