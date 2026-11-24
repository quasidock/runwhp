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
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/vmm"
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
// running. With ownIO, its output goes to this process's stdout and stderr
// if they are files, and otherwise to the container's log, and its stdin,
// if it has one, is this process's, if that is a file or a pipe, and
// otherwise open to attached runwhps. Without ownIO, as for start -a,
// which attaches and carries the container's input and output itself, its
// output goes to the log and its input is open to attached runwhps. If
// ctx ends first, the runwhp is killed.
func spawnSupervisor(ctx context.Context, root string, rec *container.Container, waitStart, ownIO bool) (container.Reply, error) {
	var args []string
	if waitStart {
		args = append(args, "--wait-start")
	}
	return startSupervisor(ctx, root, rec, args, ownIO, nil)
}

// spawnHandOff starts a runwhp in the background to take container rec
// over from this one, with lock, and start it again after delay, the
// restarts'th time; it waits until that runwhp holds the container. Its
// output goes to the container's log, and its input, if it takes input, is
// open to attached runwhps.
func spawnHandOff(ctx context.Context, root string, rec *container.Container, lock *container.Lock, delay time.Duration, restarts int) (container.Reply, error) {
	h, err := lock.Inheritable()
	if err != nil {
		return container.Reply{}, err
	}
	args := []string{"--lock", strconv.FormatUint(uint64(h), 10),
		"--restart-after", delay.String(), "--restart-count", strconv.Itoa(restarts)}
	return startSupervisor(ctx, root, rec, args, false, []syscall.Handle{syscall.Handle(h)})
}

// startSupervisor starts a runwhp running cmdSupervise for rec, with args
// and the handles to inherit beside its ready pipe, and waits for its
// reply.
func startSupervisor(ctx context.Context, root string, rec *container.Container, extra []string, ownIO bool, handles []syscall.Handle) (container.Reply, error) {
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
	args := append([]string{"__supervise", "--ready", strconv.FormatUint(uint64(readW.Fd()), 10)}, extra...)
	cmd := exec.Command(exe, append(args, rec.ID)...)
	// Not the caller's working directory, which a process that stays
	// there would keep from being removed or renamed.
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+abs)
	if ownIO && classify(os.Stdout) == handleFile {
		cmd.Stdout = os.Stdout
	}
	if ownIO && classify(os.Stderr) == handleFile {
		cmd.Stderr = os.Stderr
	}
	if k := classify(os.Stdin); ownIO && rec.Config.Stdin && (k == handleFile || k == handlePipe) {
		cmd.Stdin = os.Stdin
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              detachedProcess | windows.CREATE_NEW_PROCESS_GROUP | breakawayFromJob,
		AdditionalInheritedHandles: append([]syscall.Handle{syscall.Handle(readW.Fd())}, handles...),
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
	var ready, handedLock uint64
	var waitStart bool
	var restartAfter time.Duration
	var restarts int
	fs.Uint64Var(&ready, "ready", 0, "")
	fs.BoolVar(&waitStart, "wait-start", false, "")
	// A container handed over by a foreground run, to start again.
	fs.Uint64Var(&handedLock, "lock", 0, "")
	fs.DurationVar(&restartAfter, "restart-after", 0, "")
	fs.IntVar(&restarts, "restart-count", 0, "")
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
	if f, err := openDiagLog(rec.DiagLogPath()); err == nil {
		files = append(files, f)
		vmm.Diag = f
		fmt.Fprintf(f, "%s runwhp %d starting container %s\n", time.Now().Format(time.RFC3339), os.Getpid(), rec.Short())
	} else {
		vmm.Diag = io.Discard
	}
	cfg := rec.Config
	logf := func(string, ...any) {}
	if cfg.Debug {
		logf = func(format string, a ...any) { fmt.Fprintf(vmm.Diag, "[vmm] "+format+"\n", a...) }
	}

	bio := vmm.BootIO{Diag: vmm.Diag}
	output := container.OutputFile
	var log *container.LogWriter
	openLog := func() (*container.LogWriter, error) {
		if log == nil {
			l, err := container.OpenLogWriter(rec.Dir(), cfg.LogOptions(), errorf)
			if err != nil {
				return nil, err
			}
			log = l
			files = append(files, l)
		}
		return log, nil
	}
	for _, s := range []struct {
		f      *os.File
		stream byte
		w      *io.Writer
	}{{os.Stdout, container.Stdout, &bio.Stdout}, {os.Stderr, container.Stderr, &bio.Stderr}} {
		if classify(s.f) == handleFile {
			*s.w = s.f
			continue
		}
		if !cfg.KeepsLog() {
			*s.w, output = io.Discard, container.OutputNone
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
		bio.Stdin = os.Stdin
	}

	var sup *vmm.Supervisor
	if handedLock != 0 {
		var lock *container.Lock
		if lock, err = store.AdoptLock(rec, windows.Handle(handedLock)); err == nil {
			sup, err = vmm.Adopt(root, store, rec, lock, restarts)
		}
	} else {
		sup, err = vmm.Supervise(root, store, rec, output)
	}
	if err != nil {
		report.fail(err, nil)
		return 1
	}
	// With -i and no file or pipe for input, the input is attached runwhps'.
	sup.RunBackground(vmm.Background{
		IO:           bio,
		OpenStdin:    cfg.Stdin && bio.Stdin == nil,
		StdinOnce:    cfg.StdinOnce,
		Files:        files,
		WaitStart:    waitStart,
		HandedOver:   handedLock != 0,
		RestartAfter: restartAfter,
		Backoff:      restartBackoff(errorf),
		Report:       readinessReport{report},
		Logf:         logf,
	})
	return 0
}

// restartBackoff is the delays between restarts: Docker's, unless
// RUNWHP_RESTART_BACKOFF says INITIAL[,MAX], as Go durations, for tests.
func restartBackoff(logf func(string, ...any)) container.Backoff {
	var b container.Backoff
	v, ok := os.LookupEnv("RUNWHP_RESTART_BACKOFF")
	if !ok {
		return b
	}
	first, last, _ := strings.Cut(v, ",")
	var err error
	if b.Initial, err = time.ParseDuration(first); err == nil && last != "" {
		b.Max, err = time.ParseDuration(last)
	}
	if err != nil || b.Initial <= 0 || b.Max < 0 {
		logf("RUNWHP_RESTART_BACKOFF=%q is not INITIAL[,MAX] durations; ignored", v)
		return container.Backoff{}
	}
	return b
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

// readinessReport reports a background run's first boot on its readiness
// pipe.
type readinessReport struct{ r *readiness }

func (r readinessReport) OK(status container.Status) { r.r.ok(status) }

func (r readinessReport) Fail(err error, code int, console []string) {
	r.r.failCode(err, code, console)
}

func (r readinessReport) Done() bool { return r.r.done() }
