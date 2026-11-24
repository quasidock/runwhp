//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/vmm"
)

const attachUsage = `Usage: runwhp attach [flags] CONTAINER

Attach this console to a running container: show its output as it comes,
and pass it what is typed, or this process's input, if the container was
run in the background with -i. Exit with the container's exit code once it
stops, or with 0 on detaching, which leaves it running.

With a terminal (-t), keys go to the container as they are pressed, and
the detach keys, Ctrl+P Ctrl+Q unless --detach-keys says otherwise,
detach. Without one, Ctrl+C sends the container SIGINT.

A container created with runwhp create -i takes input once: when the
first attached input ends, so does the container's. One run with
run -d -i keeps its input open for whoever attaches next.

      --no-stdin           do not pass input to the container
      --sig-proxy=false    detach on Ctrl+C instead of sending SIGINT
      --detach-keys KEYS   the keys that detach: comma-separated, each
                           ctrl-X or a single character (default ctrl-p,ctrl-q)
`

type attachOptions struct {
	noStdin    bool
	sigProxy   bool
	detachKeys []byte
}

func attachFlags(fs *flag.FlagSet, o *attachOptions, keys *string) {
	fs.BoolVar(&o.noStdin, "no-stdin", false, "")
	fs.BoolVar(&o.sigProxy, "sig-proxy", true, "")
	fs.StringVar(keys, "detach-keys", defaultDetachKeys, "")
}

const defaultDetachKeys = "ctrl-p,ctrl-q"

func cmdAttach(args []string) int {
	fs := newFlags("attach", attachUsage)
	var o attachOptions
	var keys string
	attachFlags(fs, &o, &keys)
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	var err error
	if o.detachKeys, err = parseDetachKeys(keys); err != nil {
		errorf("%v", err)
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	c, err := store.Resolve(fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if c.State.Paused {
		errorf("cannot attach to a paused container, unpause it first")
		return 1
	}
	code, err := attachContainer(store, c, o, nil)
	if err != nil {
		errorf("%v", err)
	}
	return code
}

// attachContainer attaches this process's console to container c, and
// returns the container's exit code once it stops, or 0 on detaching.
// started, if set, is called once the attach is in place: start -a starts
// the container then, so that none of its output is missed.
func attachContainer(store *container.Store, c *container.Container, o attachOptions, started func() error) (int, error) {
	const fail = 1
	root, _ := storeRoot()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := container.Dial(ctx, root, c.ID)
	cancel()
	if errors.Is(err, container.ErrNoRunwhp) {
		return fail, fmt.Errorf("container %s is not running", c.Short())
	}
	if err != nil {
		return fail, err
	}
	defer conn.Close()
	rep, r, err := container.Exchange(conn, container.Request{Op: container.OpAttach, Stdin: c.Config.Stdin && !o.noStdin})
	if err != nil {
		return fail, err
	}
	if !rep.OK {
		if strings.Contains(rep.Error, "does not know the request") {
			return fail, fmt.Errorf("container %s was started by an older runwhp, which cannot be attached to; restart it to attach", c.Short())
		}
		return fail, fmt.Errorf("container %s: %s", c.Short(), rep.Error)
	}

	var wmu sync.Mutex
	send := func(kind byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return container.WriteFrame(conn, kind, p)
	}
	sendControl := func(m guestspec.Control) {
		data, _ := json.Marshal(m)
		send(container.AttachControl, data)
	}

	input := rep.Stdin && !o.noStdin
	console := vmm.IsTerminal(os.Stdin)
	tty := c.Config.TTY
	done := make(chan struct{})
	defer close(done)
	if tty {
		term, err := vmm.SetupConsole(input && console, vmm.IsTerminal(os.Stdout))
		if err != nil {
			return fail, fmt.Errorf("setting up the console: %w", err)
		}
		defer term.Restore()
		if rows, cols, ok := vmm.TerminalSize(os.Stdout); ok && rows > 0 {
			sendControl(guestspec.Control{Rows: rows, Cols: cols})
			go vmm.FollowSize(done, rows, cols, sendControl)
		}
	}

	// Ctrl+C: SIGINT for the container, as at its own terminal, unless it
	// has a terminal, where Ctrl+C is a key, or --sig-proxy=false; then it
	// detaches. The console closing detaches too.
	detached := make(chan struct{})
	var detachOnce sync.Once
	detach := func() { detachOnce.Do(func() { close(detached) }) }
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				if sig == os.Interrupt && o.sigProxy && !tty {
					sendControl(guestspec.Control{Signal: guestspec.SIGINT, Group: true})
					continue
				}
				detach()
			}
		}
	}()

	if input {
		var in io.Reader = os.Stdin
		if console {
			in = &detachFilter{r: vmm.NewConsoleReader(windows.Handle(os.Stdin.Fd()), !tty), keys: o.detachKeys}
		}
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := in.Read(buf)
				if n > 0 && send(container.AttachStdin, buf[:n]) != nil {
					return
				}
				if errors.Is(err, errDetached) {
					detach()
					return
				}
				if err != nil {
					send(container.AttachStdin, nil) // the end of the input
					return
				}
			}
		}()
	}

	if started != nil {
		if err := started(); err != nil {
			return fail, err
		}
	}

	ended := make(chan *container.Reply, 1)
	go func() {
		for {
			kind, p, err := container.ReadFrame(r)
			if err != nil {
				ended <- nil
				return
			}
			switch kind {
			case container.AttachStdout:
				os.Stdout.Write(p)
			case container.AttachStderr:
				os.Stderr.Write(p)
			case container.AttachExit:
				var rep container.Reply
				json.Unmarshal(p, &rep)
				ended <- &rep
				return
			}
		}
	}()
	select {
	case <-detached:
		return 0, nil
	case rep := <-ended:
		if rep != nil {
			if rep.Error != "" {
				return rep.ExitCode, errors.New(rep.Error)
			}
			return rep.ExitCode, nil
		}
	}
	// The connection ended without saying how the container did: its
	// runwhp went away, or cut this attach off.
	if err := waitUnlocked(c, 2*time.Second); err != nil {
		return fail, errors.New("the attach ended while the container runs on")
	}
	if c, err = store.Resolve(c.ID); err != nil {
		return fail, err
	}
	return c.State.ExitCode, nil
}

// errDetached is what detachFilter returns once the detach keys are typed.
var errDetached = errors.New("detached")

// detachFilter passes input on until it reads the detach keys, which it
// holds back while they might be the start of them, as Docker does.
type detachFilter struct {
	r    io.Reader
	keys []byte

	held     int // how many of keys the input ends with so far
	pending  []byte
	detached bool
	buf      [4096]byte
}

func (f *detachFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.detached {
			return 0, errDetached
		}
		n, err := f.r.Read(f.buf[:])
		f.filter(f.buf[:n])
		if err != nil && !f.detached && f.held > 0 {
			// The input ended partway through the keys: they were input.
			f.pending = append(f.pending, f.keys[:f.held]...)
			f.held = 0
		}
		if err != nil && len(f.pending) == 0 {
			if f.detached {
				return 0, errDetached
			}
			return 0, err
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *detachFilter) filter(in []byte) {
	for _, b := range in {
		if f.detached {
			return // what follows the keys is dropped
		}
		if len(f.keys) > 0 && b == f.keys[f.held] {
			f.held++
			if f.held == len(f.keys) {
				f.detached = true
			}
			continue
		}
		if f.held > 0 {
			f.pending = append(f.pending, f.keys[:f.held]...)
			f.held = 0
			if len(f.keys) > 0 && b == f.keys[0] {
				f.held = 1
				if f.held == len(f.keys) {
					f.detached = true
				}
				continue
			}
		}
		f.pending = append(f.pending, b)
	}
}

// parseDetachKeys parses --detach-keys as Docker does: comma-separated
// keys, each ctrl-X for a control key or a single character.
func parseDetachKeys(s string) ([]byte, error) {
	var keys []byte
	for _, k := range strings.Split(s, ",") {
		switch {
		case len(k) == 1:
			keys = append(keys, k[0])
		case len(k) == 6 && strings.EqualFold(k[:5], "ctrl-"):
			c := k[5]
			switch {
			case c >= 'a' && c <= 'z':
				keys = append(keys, c-'a'+1)
			case c >= 'A' && c <= 'Z':
				keys = append(keys, c-'A'+1)
			case c == '@' || c == '[' || c == '\\' || c == ']' || c == '^' || c == '_':
				keys = append(keys, c&0x1f)
			default:
				return nil, fmt.Errorf("--detach-keys: %q is not a control key", k)
			}
		default:
			return nil, fmt.Errorf("--detach-keys: %q is not ctrl-X or a single character", k)
		}
	}
	return keys, nil
}
