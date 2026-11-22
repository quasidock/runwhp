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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
)

// A container that outlives the command that started it is run by a
// runwhp of its own: runwhp __supervise, started detached from the
// caller's console and, where the caller's job allows, from its job, so
// closing the terminal does not end it. It reports on a pipe the caller
// passes it once the container is created or running, or why it is not.

// handleKind is what one of a process's standard handles is.
type handleKind int

const (
	handleNone    handleKind = iota // invalid, or the null device
	handleFile                      // a file on disk
	handlePipe                      // a pipe
	handleConsole                   // a console, or another character device
)

func classify(f *os.File) handleKind {
	if f == nil {
		return handleNone
	}
	t, err := windows.GetFileType(windows.Handle(f.Fd()))
	if err != nil && t == windows.FILE_TYPE_UNKNOWN {
		return handleNone
	}
	switch t {
	case windows.FILE_TYPE_DISK:
		return handleFile
	case windows.FILE_TYPE_PIPE:
		return handlePipe
	case windows.FILE_TYPE_CHAR:
		var mode uint32
		if windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil {
			return handleConsole
		}
	}
	return handleNone
}

const (
	detachedProcess  = 0x00000008 // DETACHED_PROCESS
	breakawayFromJob = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
)

var breakawayWarning sync.Once

// spawnSupervisor starts a runwhp to run the container in the background,
// and waits until it reports the container created (with waitStart) or
// running. Its output goes to this process's stdout and stderr if they are
// files, and otherwise to the container's log; its stdin, if it has one,
// is this process's, if that is a file or a pipe. If ctx ends first, the
// runwhp is killed.
func spawnSupervisor(ctx context.Context, root string, rec *container.Container, waitStart bool) (container.Reply, error) {
	exe, err := os.Executable()
	if err != nil {
		return container.Reply{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return container.Reply{}, err
	}
	readR, readW, err := os.Pipe()
	if err != nil {
		return container.Reply{}, err
	}
	defer readR.Close()
	args := []string{"__supervise", "--ready", strconv.FormatUint(uint64(readW.Fd()), 10)}
	if waitStart {
		args = append(args, "--wait-start")
	}
	cmd := exec.Command(exe, append(args, rec.ID)...)
	// Not the caller's working directory, which a process that stays
	// there would keep from being removed or renamed.
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+abs)
	if classify(os.Stdout) == handleFile {
		cmd.Stdout = os.Stdout
	}
	if classify(os.Stderr) == handleFile {
		cmd.Stderr = os.Stderr
	}
	if k := classify(os.Stdin); rec.Config.Stdin && (k == handleFile || k == handlePipe) {
		cmd.Stdin = os.Stdin
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              detachedProcess | windows.CREATE_NEW_PROCESS_GROUP | breakawayFromJob,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(readW.Fd())},
	}
	err = cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// This process is in a job that does not let its children leave
		// it, as some ssh servers and CI runners make. The container then
		// ends when the job does.
		breakawayWarning.Do(func() {
			errorf("warning: this session keeps its processes in a job they cannot leave, so the container will stop when the session ends")
		})
		cmd.SysProcAttr.CreationFlags &^= breakawayFromJob
		err = cmd.Start()
	}
	readW.Close()
	if err != nil {
		return container.Reply{}, fmt.Errorf("starting runwhp for container %s: %w", rec.Short(), err)
	}
	lines := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(readR).ReadBytes('\n')
		lines <- line
	}()
	select {
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return container.Reply{}, ctx.Err()
	case line := <-lines:
		var rep container.Reply
		if err := json.Unmarshal(line, &rep); err == nil {
			cmd.Process.Release()
			return rep, nil
		}
		// It ended without a word.
		werr := cmd.Wait()
		msg := fmt.Sprintf("runwhp's process for container %s ended at once (%v)", rec.Short(), werr)
		if log, err := os.ReadFile(filepath.Join(rec.Dir(), "runwhp.log")); err == nil && len(log) > 0 {
			msg += ": " + lastLine(string(log))
		}
		return container.Reply{}, errors.New(msg)
	}
}

func lastLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}

// cmdSupervise runs a container in the background, for spawnSupervisor.
func cmdSupervise(args []string) int {
	fs := flag.NewFlagSet("__supervise", flag.ContinueOnError)
	var ready uint64
	var waitStart bool
	fs.Uint64Var(&ready, "ready", 0, "")
	fs.BoolVar(&waitStart, "wait-start", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 1 || ready == 0 {
		return 2
	}
	report := &readiness{f: os.NewFile(uintptr(ready), "ready")}
	defer report.close()
	root, err := storeRoot()
	if err != nil {
		report.fail(err, nil)
		return 1
	}
	store := container.Open(root)
	rec, err := store.Get(fs.Arg(0))
	if err != nil {
		report.fail(err, nil)
		return 1
	}
	// runwhp's own messages go to the container's runwhp.log, not to the
	// files its output may go to.
	var files []io.Closer
	if f, err := os.OpenFile(filepath.Join(rec.Dir(), "runwhp.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		files = append(files, f)
		diagOut = f
		fmt.Fprintf(f, "%s runwhp %d starting container %s\n", time.Now().Format(time.RFC3339), os.Getpid(), rec.Short())
	} else {
		diagOut = io.Discard
	}
	cfg := rec.Config
	logf := func(string, ...any) {}
	if cfg.Debug {
		logf = func(format string, a ...any) { fmt.Fprintf(diagOut, "[vmm] "+format+"\n", a...) }
	}

	bio := bootIO{diag: diagOut}
	output := container.OutputFile
	var log *container.Log
	openLog := func() (*container.Log, error) {
		if log == nil {
			f, err := os.OpenFile(rec.LogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				return nil, err
			}
			log = container.NewLog(f)
			files = append(files, f)
		}
		return log, nil
	}
	for _, s := range []struct {
		f      *os.File
		stream byte
		w      *io.Writer
	}{{os.Stdout, container.Stdout, &bio.stdout}, {os.Stderr, container.Stderr, &bio.stderr}} {
		if classify(s.f) == handleFile {
			*s.w = s.f
			continue
		}
		l, err := openLog()
		if err != nil {
			report.fail(fmt.Errorf("opening the container's log: %w", err), nil)
			return 1
		}
		*s.w, output = l.Stream(s.stream), container.OutputLog
	}
	if k := classify(os.Stdin); cfg.Stdin && (k == handleFile || k == handlePipe) {
		bio.stdin = os.Stdin
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy := &signalProxy{stop: cancel, queue: newControlQueue(), logf: logf}
	sup, err := supervise(root, store, rec, output, proxy)
	if err != nil {
		report.fail(err, nil)
		return 1
	}
	sup.files = files
	hooks := sup.hooks(waitStart)
	if waitStart {
		hooks.ready = func() {
			sup.isReady()
			report.ok(container.Created)
		}
	} else {
		hooks.started = func(pid int) {
			sup.started(pid)
			report.ok(container.Running)
		}
	}
	code, err := bootContainer(ctx, root, rec, image.PullNever, proxy, bio, hooks, logf, time.Now())
	if err != nil {
		errorf("%v", err)
	}
	if !report.done() {
		e := err
		if e == nil {
			e = fmt.Errorf("the container exited with %d before it was ready", code)
		}
		sup.mu.Lock()
		console := sup.console
		sup.mu.Unlock()
		report.failCode(e, code, console)
	}
	sup.finish(code, err)
	return 0
}

// readiness is the pipe on which a background runwhp tells the one that
// started it how the start went: one Reply, once.
type readiness struct {
	mu   sync.Mutex
	f    *os.File
	sent bool
}

func (r *readiness) send(rep container.Reply) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent {
		return
	}
	r.sent = true
	data, _ := json.Marshal(rep)
	r.f.Write(append(data, '\n'))
	r.f.Close()
}

func (r *readiness) ok(status container.Status) {
	r.send(container.Reply{OK: true, Status: status})
}

func (r *readiness) fail(err error, console []string) {
	r.failCode(err, 125, console)
}

func (r *readiness) failCode(err error, code int, console []string) {
	r.send(container.Reply{Error: err.Error(), ExitCode: code, Console: console})
}

func (r *readiness) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

func (r *readiness) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sent {
		r.f.Close()
		r.sent = true
	}
}
