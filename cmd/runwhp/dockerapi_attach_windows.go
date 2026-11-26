//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/guestspec"
)

func allocatedSize(path string) (int64, error) { return disk.Allocated(path) }

// postContainerAttach attaches a client to a container's streams, as
// Docker's attach does: it takes the connection over, and carries the
// container's output to it, framed unless the container has a terminal,
// and its input, if asked for, to the container, until the container
// stops, the client goes, or it types the detach keys.
func (s *apiServer) postContainerAttach(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	stream, logs := boolParam(r, "stream"), boolParam(r, "logs")
	wantIn, wantOut, wantErr := boolParam(r, "stdin"), boolParam(r, "stdout"), boolParam(r, "stderr")
	if !stream && !logs {
		return badRequest("Bad parameters: you must choose at least one stream")
	}
	tty := c.Config.TTY
	var keys []byte
	if tty {
		spec := r.URL.Query().Get("detachKeys")
		if spec == "" {
			spec = defaultDetachKeys
		}
		if keys, err = parseDetachKeys(spec); err != nil {
			return badRequest("%v", err)
		}
	}

	// Attach to the container first, so that a refusal is an HTTP error.
	var sup net.Conn
	var sr *bufio.Reader
	attachedAt := time.Now()
	if stream {
		switch c.State.Status {
		case container.Stopped:
			if !logs {
				return conflict("You cannot attach to a stopped container, start it first")
			}
		case container.Restarting:
			return conflict("You cannot attach to a restarting container, wait until it is running")
		default:
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			conn, err := container.Dial(ctx, s.root, c.ID)
			cancel()
			if errors.Is(err, container.ErrNoRunwhp) {
				if !logs {
					return conflict("You cannot attach to a stopped container, start it first")
				}
				break
			}
			if err != nil {
				return err
			}
			attachedAt = time.Now()
			rep, br, err := container.Exchange(conn, container.Request{Op: container.OpAttach, Stdin: wantIn && c.Config.Stdin})
			if err == nil {
				err = container.ReplyError(rep)
			}
			if err != nil {
				conn.Close()
				if errors.Is(err, container.ErrNotRunning) {
					return conflict("You cannot attach to a stopped container, start it first")
				}
				return err
			}
			sup, sr = conn, br
			wantIn = wantIn && rep.Stdin
		}
	}
	if sup != nil {
		defer sup.Close()
	}

	ctype := dockerapiMultiplexed
	if tty {
		ctype = dockerapiRaw
	}
	conn, brw, err := hijack(w, r, ctype)
	if err != nil {
		return nil
	}
	defer conn.Close()
	s.emitContainer(c, "attach", nil)
	out := &lockedWriter{w: conn}
	var ow, ew io.Writer = io.Discard, io.Discard
	if wantOut {
		ow = streamWriter(out, container.Stdout, tty)
	}
	if wantErr {
		ew = streamWriter(out, container.Stderr, tty)
	}

	if logs {
		// What the log holds from before the attach; what came after it
		// comes through the attach.
		f := container.LogFilter{Tail: -1}
		if sup != nil {
			f.Until = attachedAt
		}
		if p, err := container.NewLogPrinter(c.Dir(), ow, ew, f); err == nil {
			p.Copy()
			p.Close()
		}
	}
	if sup == nil {
		closeWrite(conn)
		return nil
	}

	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }
	go func() {
		defer finish()
		for {
			kind, p, err := container.ReadFrame(sr)
			if err != nil {
				return
			}
			switch kind {
			case container.AttachStdout:
				if _, err := ow.Write(p); err != nil {
					return
				}
			case container.AttachStderr:
				if _, err := ew.Write(p); err != nil {
					return
				}
			case container.AttachExit:
				return
			}
		}
	}()
	go func() {
		// The client's input: what hijack's reader buffered first, then
		// the connection, until the client closes its end.
		var in io.Reader = brw.Reader
		if keys != nil && wantIn {
			in = &detachFilter{r: in, keys: keys}
		}
		buf := make([]byte, 32<<10)
		var mu sync.Mutex
		send := func(kind byte, p []byte) error {
			mu.Lock()
			defer mu.Unlock()
			return container.WriteFrame(sup, kind, p)
		}
		for {
			n, err := in.Read(buf)
			if n > 0 && wantIn {
				if send(container.AttachStdin, buf[:n]) != nil {
					finish()
					return
				}
			}
			if errors.Is(err, errDetached) {
				finish()
				return
			}
			if err != nil {
				if wantIn {
					// The client's input ended: so does the container's,
					// if it takes input once.
					send(container.AttachStdin, nil)
				}
				// A client closes its end once it has no more input, or
				// at once if it gives none, and goes on reading the
				// output: one that has gone shows when output to it
				// fails.
				return
			}
		}
	}()
	select {
	case <-done:
	case <-s.ctx.Done():
	}
	return nil
}

// sendControl hands ctl to the container through its runwhp.
func (s *apiServer) sendControl(ctx context.Context, c *container.Container, ctl guestspec.Control) error {
	return ask(ctx, s.containers, c, container.Request{Op: container.OpControl, Control: &ctl}, 5*time.Second)
}

var _ = json.Marshal
