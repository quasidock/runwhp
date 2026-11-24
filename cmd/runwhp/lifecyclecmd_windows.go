//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/vmm"
)

// interruptible returns a context that ends on Ctrl+C, and its stop.
func interruptible() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(sigs); cancel() }
}

const createUsage = `Usage: runwhp create [run flags] IMAGE [COMMAND [ARG...]]

Create a container and boot it, but do not start its process until
runwhp start. Prints its ID. Takes the flags runwhp run takes, but -d.

The container runs in a runwhp process of its own, which outlives this
one and the terminal. Its output goes to the files this command's stdout
and stderr are redirected to, if they are files, and otherwise to its
log, which runwhp logs shows. With -i, its input is this command's, if
that is a file or a pipe; otherwise it comes from runwhp start -ai or
runwhp attach, and ends when the first of them ends it.
`

func cmdCreate(args []string) int {
	var o runOptions
	fs := runFlags(&o)
	fs.Usage = func() { fmt.Fprint(fs.Output(), createUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return guestspec.ExitRuntime
	}
	if o.detach {
		errorf("create: -d is for run; a created container always runs in the background")
		return guestspec.ExitRuntime
	}
	entrypointSet := false
	fs.Visit(func(f *flag.Flag) { entrypointSet = entrypointSet || f.Name == "entrypoint" })
	ctx, stop := interruptible()
	defer stop()
	rec, code, err := createDetached(ctx, &o, entrypointSet, fs.Args(), true)
	if err != nil {
		errorf("%v", err)
		return code
	}
	fmt.Println(rec.ID)
	return 0
}

// createDetached makes a container and has a background runwhp boot it,
// up to the point of starting its process. With -i and no file or pipe for
// input, the container's input is open to attached runwhps: for create,
// until the first of them ends its own, as docker create -i does.
func createDetached(ctx context.Context, o *runOptions, entrypointSet bool, args []string, create bool) (*container.Container, int, error) {
	const fail = guestspec.ExitRuntime
	root, err := storeRoot()
	if err != nil {
		return nil, fail, err
	}
	logf := func(string, ...any) {}
	if o.debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[vmm] "+format+"\n", a...) }
	}
	policy, err := image.ParsePolicy(o.pull)
	if err != nil {
		return nil, fail, err
	}
	sweepStore(root, logf)
	// A prune waits until the container is recorded, and keeps its layers.
	release, err := gc.Hold(root)
	if err != nil {
		return nil, fail, err
	}
	cfg, err := resolveConfig(ctx, root, o, entrypointSet, args, policy, logf)
	if err != nil {
		release()
		return nil, fail, err
	}
	cfg.AutoRemove = o.rm
	cfg.StdinOnce = create && cfg.Stdin
	store := container.Open(root)
	rec, err := store.Create(cfg, o.name)
	release()
	if err != nil {
		return nil, fail, err
	}
	rep, err := spawnSupervisor(ctx, root, rec, true, true)
	if err != nil {
		if ctx.Err() != nil {
			store.Remove(rec)
			return nil, 130, errors.New("interrupted")
		}
		return nil, fail, err
	}
	if !rep.OK {
		printConsole(rep.Console)
		return nil, replyCode(rep), errors.New(rep.Error)
	}
	return rec, 0, nil
}

// runDetached is run -d: create the container, print its ID, start it.
func runDetached(o *runOptions, entrypointSet bool, args []string) int {
	ctx, stop := interruptible()
	defer stop()
	rec, code, err := createDetached(ctx, o, entrypointSet, args, false)
	if err != nil {
		errorf("%v", err)
		return code
	}
	// The ID goes first, before anything the container writes to a file
	// they share.
	fmt.Println(rec.ID)
	root, _ := storeRoot()
	rep, err := container.Ask(ctx, root, rec.ID, container.Request{Op: container.OpStart})
	if err != nil {
		errorf("starting container %s: %v", rec.Short(), err)
		return guestspec.ExitRuntime
	}
	if !rep.OK {
		printConsole(rep.Console)
		errorf("%s", rep.Error)
		return replyCode(rep)
	}
	return 0
}

func replyCode(rep container.Reply) int {
	if rep.ExitCode != 0 {
		return rep.ExitCode
	}
	return guestspec.ExitRuntime
}

func printConsole(lines []string) {
	if len(lines) == 0 {
		return
	}
	vmm.PrintConsole(os.Stderr, lines)
}

const startUsage = `Usage: runwhp start [-a [-i]] CONTAINER...

Start containers: a created one's process, or a stopped one again, in a
new VM on the writable layer it left. A container that is running is left
as it is. Its output goes as for runwhp create.

  -a, --attach            attach to the container's output, from its first
                          byte, as runwhp attach does, and exit with its exit
                          code; one container only
  -i, --interactive       with -a, attach its input as well
      --detach-keys KEYS  with -a, the keys that detach (default ctrl-p,ctrl-q)
`

func cmdStart(args []string, stdout io.Writer) int {
	fs := newFlags("start", startUsage)
	var attach, interactive bool
	var keys string
	fs.BoolVar(&attach, "a", false, "")
	fs.BoolVar(&attach, "attach", false, "")
	fs.BoolVar(&interactive, "i", false, "")
	fs.BoolVar(&interactive, "interactive", false, "")
	fs.StringVar(&keys, "detach-keys", defaultDetachKeys, "")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if attach && fs.NArg() > 1 {
		errorf("start -a attaches to one container at a time")
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	ctx, stop := interruptible()
	if attach {
		o := attachOptions{noStdin: !interactive, sigProxy: true}
		var err error
		if o.detachKeys, err = parseDetachKeys(keys); err != nil {
			stop()
			errorf("%v", err)
			return 2
		}
		code, err := startAttached(ctx, stop, store, fs.Arg(0), o)
		if err != nil {
			errorf("%v", err)
		}
		return code
	}
	defer stop()
	code := 0
	for _, ref := range fs.Args() {
		if err := startContainer(ctx, store, ref); err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

func startContainer(ctx context.Context, store *container.Store, ref string) error {
	c, err := store.Resolve(ref)
	if err != nil {
		return err
	}
	root, _ := storeRoot()
	var rep container.Reply
	switch c.State.Status {
	case container.Running, container.Restarting:
		return nil // a restarting one starts again by itself
	case container.Creating:
		return fmt.Errorf("container %s is still being created", c.Short())
	case container.Created:
		if rep, err = container.Ask(ctx, root, c.ID, container.Request{Op: container.OpStart}); err != nil {
			return fmt.Errorf("starting container %s: %w", c.Short(), err)
		}
	case container.Stopped:
		if c.Config.AutoRemove {
			return fmt.Errorf("container %s was run with --rm, and cannot be started again", c.Short())
		}
		// Fetch runwhp's kernel here, should it be missing, where its
		// progress shows; the background runwhp never uses the network.
		if _, err := vmm.ResolveKernel(ctx, root, c.Config.Kernel, image.PullMissing); err != nil {
			return err
		}
		if rep, err = spawnSupervisor(ctx, root, c, false, true); err != nil {
			return err
		}
		if errors.Is(errFromReply(rep), container.ErrRunning) {
			// Another runwhp started it first, or a cp, export, diff or
			// commit holds it.
			return busyOrStarted(store, c)
		}
	}
	if !rep.OK {
		printConsole(rep.Console)
		return fmt.Errorf("container %s: %s", c.Short(), rep.Error)
	}
	return nil
}

// startAttached is start -a: it boots a stopped container to wait for its
// start, attaches to it, and only then starts its process, so that none of
// its output is missed. A created container is attached to and started; a
// running one, attached to. done gives up ctx's Ctrl+C handling before the
// attach takes Ctrl+C over.
func startAttached(ctx context.Context, done func(), store *container.Store, ref string, o attachOptions) (int, error) {
	const fail = 1
	defer done()
	c, err := store.Resolve(ref)
	if err != nil {
		return fail, err
	}
	root, _ := storeRoot()
	start := func() error {
		rep, err := container.Ask(context.Background(), root, c.ID, container.Request{Op: container.OpStart})
		if err != nil {
			return fmt.Errorf("starting container %s: %w", c.Short(), err)
		}
		if !rep.OK && !strings.HasPrefix(rep.Error, "the container exited with") {
			// The attach goes on, to report how the container ended.
			printConsole(rep.Console)
			errorf("container %s: %s", c.Short(), rep.Error)
		}
		return nil
	}
	switch c.State.Status {
	case container.Running:
		start = nil
	case container.Restarting:
		return fail, fmt.Errorf("container %s is restarting: attach once it is running", c.Short())
	case container.Creating:
		return fail, fmt.Errorf("container %s is still being created", c.Short())
	case container.Stopped:
		if c.Config.AutoRemove {
			return fail, fmt.Errorf("container %s was run with --rm, and cannot be started again", c.Short())
		}
		if _, err := vmm.ResolveKernel(ctx, root, c.Config.Kernel, image.PullMissing); err != nil {
			return fail, err
		}
		// Its input comes through the attach, not from this process.
		rep, err := spawnSupervisor(ctx, root, c, true, false)
		if err != nil {
			return fail, err
		}
		switch {
		case errors.Is(errFromReply(rep), container.ErrRunning):
			start = nil // another runwhp started it first
		case !rep.OK:
			printConsole(rep.Console)
			return replyCode(rep), fmt.Errorf("container %s: %s", c.Short(), rep.Error)
		}
	}
	done()
	return attachContainer(store, c, o, start)
}

const waitUsage = `Usage: runwhp wait CONTAINER...

Wait for containers to stop, and print each one's exit code, in order. A
container that has stopped already answers at once.
`

func cmdWait(args []string, stdout io.Writer) int {
	fs := newFlags("wait", waitUsage)
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	ctx, stop := interruptible()
	defer stop()
	code := 0
	for _, ref := range fs.Args() {
		exit, err := waitContainer(ctx, store, ref)
		if ctx.Err() != nil {
			return 130
		}
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, exit)
	}
	return code
}

// waitContainer waits for a container to stop, and returns its exit code.
func waitContainer(ctx context.Context, store *container.Store, ref string) (int, error) {
	c, err := store.Resolve(ref)
	if err != nil {
		return 0, err
	}
	if !c.State.Active() {
		return c.State.ExitCode, nil
	}
	root, _ := storeRoot()
	rep, err := container.Ask(ctx, root, c.ID, container.Request{Op: container.OpWait})
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err == nil && (rep.Status == container.Stopped || rep.Status == container.Restarting) {
		// It answers before it removes a container run with --rm.
		return rep.ExitCode, nil
	}
	// Its runwhp went away without answering, or had gone already: what it
	// recorded says, or what the store records for a runwhp that died.
	if err := waitUnlocked(c, 10*time.Second); err != nil {
		return 0, err
	}
	if c, err = store.Resolve(c.ID); err != nil {
		return 0, err
	}
	if c.State.Active() {
		return 0, fmt.Errorf("container %s: its runwhp did not say how it ended", c.Short())
	}
	return c.State.ExitCode, nil
}

func errFromReply(rep container.Reply) error {
	if strings.Contains(rep.Error, container.ErrRunning.Error()) {
		return container.ErrRunning
	}
	return nil
}

const killUsage = `Usage: runwhp kill [-a] CONTAINER [SIGNAL]

Send a signal to a container's process: SIGTERM unless SIGNAL names
another, by name (KILL, SIGKILL) or number. A created container ends
without starting.

  -a, --all   send it to every process in the container
`

func cmdKill(args []string) int {
	fs := newFlags("kill", killUsage)
	var all bool
	fs.BoolVar(&all, "a", false, "")
	fs.BoolVar(&all, "all", false, "")
	if fs.Parse(args) != nil || fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	sig := guestspec.SIGTERM
	if fs.NArg() == 2 {
		var err error
		if sig, err = guestspec.ParseSignal(fs.Arg(1)); err != nil {
			errorf("%v", err)
			return 2
		}
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
	if err := ask(c, container.Request{Op: container.OpKill, Signal: sig, All: all}, 10*time.Second); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}

// ask sends a request to the runwhp running c.
func ask(c *container.Container, req container.Request, timeout time.Duration) error {
	if !c.State.Active() {
		return fmt.Errorf("container %s is not running", c.Short())
	}
	root, _ := storeRoot()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rep, err := container.Ask(ctx, root, c.ID, req)
	if errors.Is(err, container.ErrNoRunwhp) {
		return fmt.Errorf("container %s is not running", c.Short())
	}
	if err != nil {
		return fmt.Errorf("container %s: %w", c.Short(), err)
	}
	if !rep.OK {
		return fmt.Errorf("container %s: %s", c.Short(), rep.Error)
	}
	return nil
}

const stopUsage = `Usage: runwhp stop [-t SECONDS] CONTAINER...

Stop containers: send each its stop signal, SIGTERM unless the image says
otherwise, then SIGKILL if it has not stopped after the timeout.

  -t, --time SECONDS   how long to wait before SIGKILL (default 10)
`

func stopFlags(fs *flag.FlagSet) *int {
	t := new(int)
	fs.IntVar(t, "t", 10, "")
	fs.IntVar(t, "time", 10, "")
	return t
}

func cmdStop(args []string, stdout io.Writer) int {
	fs := newFlags("stop", stopUsage)
	timeout := stopFlags(fs)
	if fs.Parse(args) != nil || fs.NArg() == 0 || *timeout < 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	code := 0
	for _, ref := range fs.Args() {
		if err := stopContainer(store, ref, time.Duration(*timeout)*time.Second); err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

// stopContainer stops a container, and waits for its runwhp to let it go.
func stopContainer(store *container.Store, ref string, timeout time.Duration) error {
	c, err := store.Resolve(ref)
	if err != nil {
		return err
	}
	if !c.State.Active() {
		return nil
	}
	err = ask(c, container.Request{Op: container.OpStop, Timeout: timeout.Seconds()}, timeout+vmm.KillGrace+30*time.Second)
	if err != nil && !strings.Contains(err.Error(), "not running") {
		return err
	}
	return waitUnlocked(c, 10*time.Second)
}

// waitUnlocked waits for the runwhp running c to let it go.
func waitUnlocked(c *container.Container, d time.Duration) error {
	for deadline := time.Now().Add(d); c.Locked(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s is still held by runwhp's process %d", c.Short(), c.State.Pid)
		}
	}
	return nil
}

const restartUsage = `Usage: runwhp restart [-t SECONDS] CONTAINER...

Stop containers, as runwhp stop does, and start them again.

  -t, --time SECONDS   how long to wait before SIGKILL (default 10)
`

func cmdRestart(args []string, stdout io.Writer) int {
	fs := newFlags("restart", restartUsage)
	timeout := stopFlags(fs)
	if fs.Parse(args) != nil || fs.NArg() == 0 || *timeout < 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	ctx, stop := interruptible()
	defer stop()
	code := 0
	for _, ref := range fs.Args() {
		c, err := store.Resolve(ref)
		if err == nil && c.Config.AutoRemove {
			err = fmt.Errorf("container %s was run with --rm, and would be gone once stopped", c.Short())
		}
		if err == nil {
			err = stopContainer(store, ref, time.Duration(*timeout)*time.Second)
		}
		if err == nil {
			err = startContainer(ctx, store, ref)
		}
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

const logsUsage = `Usage: runwhp logs [-f] [-t] [-n N] [--since TIME] [--until TIME] CONTAINER

Print a container's output, stdout to stdout and stderr to stderr, as its
log holds it. Every container keeps a log, but one run with --rm, with
--log-driver none, or in the background with files for its output; the
log is rotated as --log-opt says, by default at 20 MB, keeping 5 files.

  -f, --follow        keep printing what the container writes until it stops
  -t, --timestamps    print the time each line was written before it
  -n, --tail N        print only the last N lines (default all)
      --since TIME    print only lines written at or after TIME
      --until TIME    print only lines written at or before TIME

TIME is a date and time such as 2026-11-24T12:00:00Z (local time without
a zone), Unix seconds, or a duration before now, such as 10m or 1h30m.
`

func cmdLogs(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("logs", logsUsage)
	var follow bool
	var f container.LogFilter
	var tail, since, until string
	fs.BoolVar(&follow, "f", false, "")
	fs.BoolVar(&follow, "follow", false, "")
	fs.BoolVar(&f.Timestamps, "t", false, "")
	fs.BoolVar(&f.Timestamps, "timestamps", false, "")
	fs.StringVar(&tail, "n", "all", "")
	fs.StringVar(&tail, "tail", "all", "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	var err error
	now := time.Now()
	if f.Tail, err = parseTail(tail); err != nil {
		errorf("%v", err)
		return 2
	}
	if f.Since, err = parseLogTime(since, now); err == nil {
		f.Until, err = parseLogTime(until, now)
	}
	if err != nil {
		errorf("%v", err)
		return 1
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
	return printLogs(c, f, follow, stdout, stderr)
}

// printLogs prints a container's log through f, and with follow, what it
// writes after, until it stops.
func printLogs(c *container.Container, f container.LogFilter, follow bool, stdout, stderr io.Writer) int {
	files, err := container.LogFiles(c.Dir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errorf("%v", err)
		return 1
	}
	if len(files) == 0 {
		switch {
		case !c.Config.KeepsLog():
			errorf("container %s keeps no log: it was made with --log-driver none", c.Short())
		case c.State.Output == container.OutputFile:
			errorf("container %s's output goes to the files its runwhp was given, not to a log", c.Short())
		case c.State.Output == container.OutputTerminal && c.Config.AutoRemove:
			errorf("container %s's output goes to the terminal of the runwhp running it, not to a log", c.Short())
		}
		if !follow || !c.State.Active() {
			return 0
		}
	}
	p, err := container.NewLogPrinter(c.Dir(), stdout, stderr, f)
	if err != nil {
		errorf("container %s: %v", c.Short(), err)
		return 1
	}
	defer p.Close()
	for {
		if _, err := p.Copy(); err != nil {
			errorf("container %s: %v", c.Short(), err)
			return 1
		}
		if !follow || p.Done || (!f.Until.IsZero() && time.Now().After(f.Until)) {
			return 0
		}
		if !c.Locked() {
			// Stopped: whatever was written before is there now.
			if _, err := p.Copy(); err != nil {
				errorf("container %s: %v", c.Short(), err)
				return 1
			}
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killAndWait kills a container and waits for its runwhp to let it go,
// ending that runwhp itself if it does not.
func killAndWait(store *container.Store, c *container.Container) error {
	err := ask(c, container.Request{Op: container.OpKill, Signal: guestspec.SIGKILL}, 10*time.Second)
	if err != nil && !strings.Contains(err.Error(), "not running") {
		return err
	}
	if waitUnlocked(c, vmm.KillGrace+5*time.Second) == nil {
		return nil
	}
	if c.State.Pid > 0 {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(c.State.Pid)); err == nil {
			windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
	}
	return waitUnlocked(c, 5*time.Second)
}
