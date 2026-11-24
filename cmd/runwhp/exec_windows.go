//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

const execUsage = `Usage: runwhp exec [flags] CONTAINER COMMAND [ARG...]

Run a command in a running container, with its environment, working
directory and user unless the flags say otherwise.

Flags:
  -i, --interactive       pass this program's standard input to the command
  -t, --tty               give the command a terminal; with -i, keys go to it raw
  -d, --detach            start the command and return, leaving it to run
  -e, --env KEY[=VALUE]   set an environment variable (repeatable); KEY alone copies it from here
  -w, --workdir DIR       working directory, which must exist
  -u, --user USER[:GROUP] user to run as: name or uid, optionally with a group

The exit code is the command's, or 125 if runwhp failed, 126 if the
command cannot be run and 127 if it does not exist. The command is killed
when the container's process exits.
`

func cmdExec(args []string) int {
	const fail = guestspec.ExitRuntime
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), execUsage) }
	var interactive, tty, detach bool
	var env listFlag
	var workdir, user string
	fs.BoolVar(&interactive, "i", false, "")
	fs.BoolVar(&interactive, "interactive", false, "")
	fs.BoolVar(&tty, "t", false, "")
	fs.BoolVar(&tty, "tty", false, "")
	both := func(string) error { interactive, tty = true, true; return nil }
	fs.BoolFunc("it", "", both)
	fs.BoolFunc("ti", "", both)
	fs.BoolVar(&detach, "d", false, "")
	fs.BoolVar(&detach, "detach", false, "")
	fs.Var(&env, "e", "")
	fs.Var(&env, "env", "")
	fs.StringVar(&workdir, "w", "", "")
	fs.StringVar(&workdir, "workdir", "", "")
	fs.StringVar(&user, "u", "", "")
	fs.StringVar(&user, "user", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return fail
	}
	if interactive && tty && !detach && !isTerminal(os.Stdin) {
		errorf("the input device is not a TTY; if you are using mintty, try prefixing the command with 'winpty'")
		return fail
	}
	ex := &guestspec.Exec{Args: fs.Args()[1:], Cwd: workdir, User: user, Stdin: interactive && !detach, TTY: tty, Detach: detach}
	for _, e := range env {
		if !strings.Contains(e, "=") {
			v, ok := os.LookupEnv(e)
			if !ok {
				continue
			}
			e += "=" + v
		}
		ex.Env = append(ex.Env, e)
	}
	if tty {
		if rows, cols, ok := terminalSize(os.Stdout); ok && rows > 0 {
			ex.Rows, ex.Cols = rows, cols
		} else {
			ex.Rows, ex.Cols = detachedRows, detachedCols
		}
	}

	store, ok := openContainers()
	if !ok {
		return fail
	}
	c, err := store.Resolve(fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return fail
	}
	if c.State.Status == container.Restarting {
		errorf("container %s is restarting: wait until it is running", c.Short())
		return fail
	}
	if c.State.Status != container.Running {
		errorf("container %s is not running", c.Short())
		return fail
	}
	root, _ := storeRoot()
	code, err := runExec(root, c, ex)
	if err != nil {
		errorf("%v", err)
	}
	return code
}

// execConn is one connection to init's exec server, through the runwhp
// running the container.
type execConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *execConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func dialExec(root, id string, hello guestspec.ExecHello) (*execConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := container.Dial(ctx, root, id)
	if errors.Is(err, container.ErrNoRunwhp) {
		return nil, errors.New("the container is not running")
	}
	if err != nil {
		return nil, err
	}
	rep, r, err := container.Exchange(conn, container.Request{Op: container.OpConnect, Port: guestspec.ExecPort})
	if err == nil && !rep.OK {
		err = errors.New(rep.Error)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &execConn{conn, r}
	line, _ := json.Marshal(hello)
	if _, err := c.Write(append(line, '\n')); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// runExec runs ex in the container and returns its exit code.
func runExec(root string, c *container.Container, ex *guestspec.Exec) (int, error) {
	const fail = guestspec.ExitRuntime
	ctl, err := dialExec(root, c.ID, guestspec.ExecHello{Exec: ex})
	if err != nil {
		return fail, err
	}
	defer ctl.Close()
	events := json.NewDecoder(ctl.r)
	var ev guestspec.ExecEvent
	if err := events.Decode(&ev); err != nil {
		return fail, fmt.Errorf("init did not answer runwhp exec: %w", err)
	}
	if ev.Exited {
		return ev.Exit, errors.New(ev.Error)
	}

	// The streams, one connection each.
	var outputs sync.WaitGroup
	for _, name := range ev.Streams {
		sc, err := dialExec(root, c.ID, guestspec.ExecHello{Attach: ev.ID, Stream: name})
		if err != nil {
			return fail, fmt.Errorf("attaching %s: %w", name, err)
		}
		defer sc.Close()
		switch name {
		case guestspec.PortStdout, guestspec.PortStderr:
			out := io.Writer(os.Stdout)
			if name == guestspec.PortStderr {
				out = os.Stderr
			}
			outputs.Add(1)
			go func() {
				defer outputs.Done()
				io.Copy(out, sc)
			}()
		case guestspec.PortStdin:
			var in io.Reader = os.Stdin
			if isTerminal(os.Stdin) {
				in = &consoleReader{h: windows.Handle(os.Stdin.Fd()), lines: !ex.TTY}
			}
			go func() {
				io.Copy(sc, in)
				sc.Close()
			}()
		}
	}

	// Control messages: signals and, with a terminal, its size.
	var ctlMu sync.Mutex
	send := func(m guestspec.Control) {
		line, _ := json.Marshal(m)
		ctlMu.Lock()
		ctl.Write(append(line, '\n'))
		ctlMu.Unlock()
	}
	if ex.TTY && !ex.Detach {
		term, err := setupConsole(ex.Stdin, isTerminal(os.Stdout))
		if err != nil {
			return fail, fmt.Errorf("setting up the console: %w", err)
		}
		defer term.restore()
		done := make(chan struct{})
		defer close(done)
		go followSize(done, ex.Rows, ex.Cols, send)
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	interrupted := make(chan struct{})
	go func() {
		n := 0
		for sig := range sigs {
			n++
			switch {
			case sig != os.Interrupt:
				send(guestspec.Control{Signal: guestspec.SIGTERM})
			case n == 1:
				send(guestspec.Control{Signal: guestspec.SIGINT, Group: true})
			default:
				send(guestspec.Control{Signal: guestspec.SIGKILL, Group: true})
				close(interrupted)
				return
			}
		}
	}()

	result := make(chan guestspec.ExecEvent, 1)
	go func() {
		for {
			var ev guestspec.ExecEvent
			if err := events.Decode(&ev); err != nil {
				result <- guestspec.ExecEvent{Exited: true, Exit: guestspec.ExitSignalBase + guestspec.SIGKILL,
					Error: "the container stopped while the command ran"}
				return
			}
			if ev.Started && ex.Detach {
				result <- guestspec.ExecEvent{Exited: true}
				return
			}
			if ev.Exited {
				result <- ev
				return
			}
		}
	}()
	select {
	case ev = <-result:
	case <-interrupted:
		return 130, nil
	}
	// Let the last of the output through.
	done := make(chan struct{})
	go func() { outputs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if ev.Error != "" {
		return ev.Exit, errors.New(ev.Error)
	}
	return ev.Exit, nil
}
