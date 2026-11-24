//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/ocispec"
	"github.com/quasidock/runwhp/internal/vm"
	"github.com/quasidock/runwhp/internal/vmm"
)

// defaultStorageSize is how large a container's writable layer may grow,
// as runwhp run's default --storage-opt size.
const defaultStorageSize = 64 << 30

// service is the task API for the one task a shim runs, and the execs in
// it.
type service struct {
	ns, id string
	root   string // runwhp's store the shim keeps its container in
	events *publisher
	quit   chan struct{}
	quitMu sync.Once

	mu    sync.Mutex
	task  *task
	execs map[string]*execProcess
}

// task is the task's container, and its process.
type task struct {
	bundle string
	rec    *container.Container
	store  *container.Store
	sup    *vmm.Supervisor
	io     *processIO
	paths  [3]string // stdin, stdout, stderr as containerd gave them
	tty    bool

	exited   chan struct{} // closed once the container has stopped for good
	exit     uint32
	exitedAt time.Time
	deleted  bool
}

// execProcess is a process exec started in the container.
type execProcess struct {
	id      string
	ex      *guestspec.Exec
	paths   [3]string
	io      *processIO
	sess    *vmm.ExecSession
	pid     uint32
	started bool

	exited   chan struct{}
	exit     uint32
	exitedAt time.Time
}

func newService(ns, id, root string, events *publisher) *service {
	return &service{ns: ns, id: id, root: root, events: events, quit: make(chan struct{}), execs: map[string]*execProcess{}}
}

func errNotFound(format string, a ...any) error {
	return status.Errorf(codes.NotFound, format, a...)
}

func errPrecondition(format string, a ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, a...)
}

// get returns the task, or NotFound before Create.
func (s *service) get() (*task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == nil || s.task.deleted {
		return nil, errNotFound("task %s not found", s.id)
	}
	return s.task, nil
}

// exec returns the exec id, or NotFound.
func (s *service) exec(id string) (*execProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.execs[id]
	if !ok {
		return nil, errNotFound("exec %s not found in task %s", id, s.id)
	}
	return e, nil
}

// createReport hands Create how the container's first boot went.
type createReport struct {
	once sync.Once
	done chan error
}

func (r *createReport) OK(container.Status) { r.send(nil) }

func (r *createReport) Fail(err error, code int, console []string) {
	if len(console) > 0 {
		for _, l := range console {
			diagf("console: %s", l)
		}
	}
	r.send(err)
}

func (r *createReport) Done() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *createReport) send(err error) {
	r.once.Do(func() { r.done <- err; close(r.done) })
}

// Create boots the container from the bundle's spec and the snapshot's
// layers, ready to start its process.
func (s *service) Create(ctx context.Context, r *taskv3.CreateTaskRequest) (_ *taskv3.CreateTaskResponse, retErr error) {
	s.mu.Lock()
	if s.task != nil {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "task %s already exists", r.ID)
	}
	s.mu.Unlock()

	// containerd removes the bundle when the task goes, unmounting its
	// rootfs directory first, which Windows refuses unelevated; a missing
	// directory it leaves alone. runwhp's root is the snapshot's layers,
	// never mounted there.
	if err := os.Remove(filepath.Join(r.Bundle, "rootfs")); err != nil && !os.IsNotExist(err) {
		diagf("removing the bundle's rootfs directory: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("reading the bundle's config.json: %w", err)
	}
	var rootfs []ocispec.Mount
	for _, m := range r.Rootfs {
		rootfs = append(rootfs, ocispec.Mount{Type: m.Type, Source: m.Source, Options: m.Options})
	}
	cfg, err := ocispec.Config(ocispec.Task{Namespace: s.ns, ID: r.ID, Spec: &spec, Rootfs: rootfs,
		Stdin: r.Stdin != "", Terminal: r.Terminal})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.complete(ctx, cfg, r.ID); err != nil {
		return nil, err
	}

	store := container.Open(s.root)
	rec, err := store.CreateWith(cfg, "", container.State{Pid: os.Getpid(), Output: container.OutputNone})
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			store.Remove(rec)
		}
	}()
	if data, err := json.Marshal(bundleRecord{Root: s.root, Container: rec.ID}); err == nil {
		os.WriteFile(filepath.Join(r.Bundle, bundleRecordName), data, 0o644)
	}
	logDiagTo(rec.DiagLogPath())
	diagf("task %s/%s is container %s", s.ns, r.ID, rec.ID)

	pio, err := openIO(s.ns, r.ID, r.Stdin, r.Stdout, r.Stderr, cfg.TTY)
	if err != nil {
		return nil, err
	}
	sup, err := vmm.Supervise(s.root, store, rec, container.OutputNone)
	if err != nil {
		pio.close()
		return nil, err
	}
	t := &task{bundle: r.Bundle, rec: rec, store: store, sup: sup, io: pio, tty: cfg.TTY,
		paths: [3]string{r.Stdin, r.Stdout, r.Stderr}, exited: make(chan struct{})}
	bio := vmm.BootIO{Stdout: pio.stdout, Stderr: pio.stderr, Diag: vmm.Diag}
	if pio.stdin != nil {
		bio.Stdin = pio.stdin
	}
	logf := func(string, ...any) {}
	if cfg.Debug {
		logf = func(format string, a ...any) { diagf("[vmm] "+format, a...) }
	}
	report := &createReport{done: make(chan error, 1)}
	go func() {
		sup.RunBackground(vmm.Background{IO: bio, WaitStart: true, Report: report, Logf: logf})
		s.finished(t)
	}()
	select {
	case err = <-report.done:
	case <-ctx.Done():
		err = ctx.Err()
		sup.Kill(guestspec.SIGKILL, false)
	}
	if err != nil {
		<-t.exited
		return nil, err
	}
	s.mu.Lock()
	s.task = t
	s.mu.Unlock()
	s.events.publish("/tasks/create", &eventsapi.TaskCreate{ContainerID: r.ID, Bundle: r.Bundle,
		Rootfs: r.Rootfs, IO: &eventsapi.TaskIO{Stdin: r.Stdin, Stdout: r.Stdout, Stderr: r.Stderr, Terminal: r.Terminal},
		Pid: uint32(os.Getpid())})
	return &taskv3.CreateTaskResponse{Pid: uint32(os.Getpid())}, nil
}

// complete fills in what the spec leaves to the runtime: runwhp's
// defaults for memory, vCPUs and the writable layer's size, the kernel,
// and the hostname, and checks the devices fit.
func (s *service) complete(ctx context.Context, cfg *container.Config, id string) error {
	cfg.Version, cfg.ID, cfg.Created = container.ConfigVersion, container.NewID(), time.Now().UTC()
	if cfg.Memory == 0 {
		cfg.Memory = vmm.DefaultMemory()
	} else {
		cfg.Memory = (cfg.Memory + (2<<20 - 1)) &^ (2<<20 - 1)
		if maxMem := vm.MaxMemory(); cfg.Memory < 128<<20 || cfg.Memory > maxMem {
			return status.Errorf(codes.InvalidArgument, "memory must be between 128m and %dm, this machine's memory", maxMem>>20)
		}
	}
	most := min(runtime.NumCPU(), vm.MaxCPUs)
	switch {
	case cfg.CPUs == 0:
		cfg.CPUs = min(most, vmm.DefaultCPUs)
	case cfg.CPUs > most:
		return status.Errorf(codes.InvalidArgument, "--cpus must be between 1 and %d", most)
	}
	cfg.StorageSize = defaultStorageSize
	if cfg.Hostname == "" {
		cfg.Hostname = id
		if len(id) > 12 {
			cfg.Hostname = id[:12]
		}
	}
	// The kernel: RUNWHP_KERNEL's, which runwhp containerd sets to its
	// own, else runwhp's, fetched into the shim's store if need be.
	if cfg.Kernel = vmm.ExplicitKernel(""); cfg.Kernel == "" {
		if _, err := vmm.ResolveKernel(ctx, s.root, "", image.PullMissing); err != nil {
			return err
		}
	}
	return vmm.CheckDevices(cfg, 0)
}

// finished records that the container has stopped for good, once its
// output has all been passed on, and tells containerd.
func (s *service) finished(t *task) {
	rep := t.sup.Wait()
	t.io.close()
	s.mu.Lock()
	t.exit, t.exitedAt = uint32(rep.ExitCode), time.Now()
	s.mu.Unlock()
	close(t.exited)
	diagf("container %s exited with %d", t.rec.ID, rep.ExitCode)
	s.events.publish("/tasks/exit", &eventsapi.TaskExit{ContainerID: s.id, ID: s.id, Pid: uint32(os.Getpid()),
		ExitStatus: t.exit, ExitedAt: timestamppb.New(t.exitedAt)})
}

// Start starts the task's process, or an exec's.
func (s *service) Start(ctx context.Context, r *taskv3.StartRequest) (*taskv3.StartResponse, error) {
	if r.ExecID != "" {
		return s.startExec(ctx, r)
	}
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	rep := t.sup.Start()
	if !rep.OK {
		for _, l := range rep.Console {
			diagf("console: %s", l)
		}
		return nil, errPrecondition("%s", rep.Error)
	}
	s.events.publish("/tasks/start", &eventsapi.TaskStart{ContainerID: r.ID, Pid: uint32(os.Getpid())})
	return &taskv3.StartResponse{Pid: uint32(os.Getpid())}, nil
}

func (s *service) startExec(ctx context.Context, r *taskv3.StartRequest) (*taskv3.StartResponse, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	e, err := s.exec(r.ExecID)
	if err != nil {
		return nil, err
	}
	if e.started {
		return nil, errPrecondition("exec %s has already started", r.ExecID)
	}
	pio, err := openIO(s.ns, r.ID+"-"+r.ExecID, e.paths[0], e.paths[1], e.paths[2], e.ex.TTY)
	if err != nil {
		return nil, err
	}
	streams := vmm.ExecStreams{Stdout: pio.stdout, Stderr: pio.stderr}
	if pio.stdin != nil {
		streams.Stdin = pio.stdin
		e.ex.Stdin = true
	}
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return t.sup.DialGuest(ctx, guestspec.ExecPort)
	}
	sess, err := vmm.StartExec(ctx, dial, e.ex, streams)
	if err != nil {
		pio.close()
		return nil, errPrecondition("%v", err)
	}
	pid, err := sess.Started()
	if err != nil {
		sess.Close()
		pio.close()
		return nil, errPrecondition("%v", err)
	}
	s.mu.Lock()
	e.io, e.sess, e.pid, e.started = pio, sess, uint32(pid), true
	s.mu.Unlock()
	s.events.publish("/tasks/exec-started", &eventsapi.TaskExecStarted{ContainerID: r.ID, ExecID: r.ExecID, Pid: e.pid})
	go func() {
		code, err := sess.Wait()
		if err != nil {
			diagf("exec %s: %v", r.ExecID, err)
		}
		sess.Close()
		pio.close()
		s.mu.Lock()
		e.exit, e.exitedAt = uint32(code), time.Now()
		s.mu.Unlock()
		close(e.exited)
		s.events.publish("/tasks/exit", &eventsapi.TaskExit{ContainerID: r.ID, ID: r.ExecID, Pid: e.pid,
			ExitStatus: e.exit, ExitedAt: timestamppb.New(e.exitedAt)})
	}()
	return &taskv3.StartResponse{Pid: e.pid}, nil
}

// Exec records an exec, which Start starts.
func (s *service) Exec(ctx context.Context, r *taskv3.ExecProcessRequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	select {
	case <-t.exited:
		return nil, errPrecondition("container %s is not running", r.ID)
	default:
	}
	var p specs.Process
	if r.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "the exec has no process spec")
	}
	if err := json.Unmarshal(r.Spec.Value, &p); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "the exec's process spec: %v", err)
	}
	ex, err := ocispec.Exec(&p, r.Terminal)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.execs[r.ExecID]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "exec %s already exists", r.ExecID)
	}
	s.execs[r.ExecID] = &execProcess{id: r.ExecID, ex: ex, paths: [3]string{r.Stdin, r.Stdout, r.Stderr}, exited: make(chan struct{})}
	s.events.publish("/tasks/exec-added", &eventsapi.TaskExecAdded{ContainerID: r.ID, ExecID: r.ExecID})
	return &emptypb.Empty{}, nil
}

// State describes the task's process, or an exec's.
func (s *service) State(ctx context.Context, r *taskv3.StateRequest) (*taskv3.StateResponse, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := tasktypes.Status_CREATED
		switch {
		case isClosed(e.exited):
			st = tasktypes.Status_STOPPED
		case e.started:
			st = tasktypes.Status_RUNNING
		}
		return &taskv3.StateResponse{ID: r.ID, ExecID: r.ExecID, Bundle: t.bundle, Pid: e.pid, Status: st,
			Stdin: e.paths[0], Stdout: e.paths[1], Stderr: e.paths[2], Terminal: e.ex.TTY,
			ExitStatus: e.exit, ExitedAt: timestamppb.New(e.exitedAt)}, nil
	}
	resp := &taskv3.StateResponse{ID: r.ID, Bundle: t.bundle, Pid: uint32(os.Getpid()),
		Stdin: t.paths[0], Stdout: t.paths[1], Stderr: t.paths[2], Terminal: t.tty}
	st := t.sup.State()
	switch {
	case isClosed(t.exited):
		resp.Status = tasktypes.Status_STOPPED
		s.mu.Lock()
		resp.ExitStatus, resp.ExitedAt = t.exit, timestamppb.New(t.exitedAt)
		s.mu.Unlock()
	case st.Status == container.Running && st.Paused:
		resp.Status = tasktypes.Status_PAUSED
	case st.Status == container.Running:
		resp.Status = tasktypes.Status_RUNNING
	case st.Status == container.Stopped:
		resp.Status = tasktypes.Status_STOPPED
	default:
		resp.Status = tasktypes.Status_CREATED
	}
	return resp, nil
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// Kill signals the task's process, or with All every process in the
// container, or an exec's process.
func (s *service) Kill(ctx context.Context, r *taskv3.KillRequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if r.Signal == 0 || r.Signal > 64 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid signal %d", r.Signal)
	}
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		if !e.started || isClosed(e.exited) {
			return nil, errNotFound("exec %s is not running", r.ExecID)
		}
		e.sess.Send(guestspec.Control{Signal: int(r.Signal), Group: r.All})
		return &emptypb.Empty{}, nil
	}
	if isClosed(t.exited) {
		return nil, errNotFound("process already finished")
	}
	if err := t.sup.Kill(int(r.Signal), r.All); err != nil {
		if errors.Is(err, vmm.ErrNotRunning) {
			return nil, errNotFound("process already finished")
		}
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// Wait waits for the task's process, or an exec's, to exit.
func (s *service) Wait(ctx context.Context, r *taskv3.WaitRequest) (*taskv3.WaitResponse, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	exited, get := t.exited, func() (uint32, time.Time) { return t.exit, t.exitedAt }
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		exited, get = e.exited, func() (uint32, time.Time) { return e.exit, e.exitedAt }
	}
	select {
	case <-exited:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	code, at := get()
	s.mu.Unlock()
	return &taskv3.WaitResponse{ExitStatus: code, ExitedAt: timestamppb.New(at)}, nil
}

// Delete removes an exited exec, or the task once its container has
// stopped: its record goes, and its writable layer stays with the
// snapshot.
func (s *service) Delete(ctx context.Context, r *taskv3.DeleteRequest) (*taskv3.DeleteResponse, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		if e.started && !isClosed(e.exited) {
			return nil, errPrecondition("exec %s is still running", r.ExecID)
		}
		s.mu.Lock()
		delete(s.execs, r.ExecID)
		s.mu.Unlock()
		return &taskv3.DeleteResponse{Pid: e.pid, ExitStatus: e.exit, ExitedAt: timestamppb.New(e.exitedAt)}, nil
	}
	select {
	case <-t.exited:
	default:
		return nil, errPrecondition("task %s is still running; kill it first", r.ID)
	}
	resetDiag()
	if err := t.store.Remove(t.rec); err != nil {
		diagf("removing container %s: %v", t.rec.ID, err)
	}
	s.mu.Lock()
	t.deleted = true
	s.mu.Unlock()
	s.events.publish("/tasks/delete", &eventsapi.TaskDelete{ContainerID: r.ID, ID: r.ID, Pid: uint32(os.Getpid()),
		ExitStatus: t.exit, ExitedAt: timestamppb.New(t.exitedAt)})
	return &taskv3.DeleteResponse{Pid: uint32(os.Getpid()), ExitStatus: t.exit, ExitedAt: timestamppb.New(t.exitedAt)}, nil
}

// Pids lists the task's processes: its own, as the shim's pid, and its
// execs', as their pids in the guest.
func (s *service) Pids(ctx context.Context, r *taskv3.PidsRequest) (*taskv3.PidsResponse, error) {
	if _, err := s.get(); err != nil {
		return nil, err
	}
	resp := &taskv3.PidsResponse{Processes: []*tasktypes.ProcessInfo{{Pid: uint32(os.Getpid())}}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.execs {
		if e.started && !isClosed(e.exited) {
			resp.Processes = append(resp.Processes, &tasktypes.ProcessInfo{Pid: e.pid})
		}
	}
	return resp, nil
}

// Pause freezes the container's processes.
func (s *service) Pause(ctx context.Context, r *taskv3.PauseRequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if rep := t.sup.Pause(); !rep.OK {
		return nil, errPrecondition("%s", rep.Error)
	}
	s.events.publish("/tasks/paused", &eventsapi.TaskPaused{ContainerID: r.ID})
	return &emptypb.Empty{}, nil
}

// Resume thaws them.
func (s *service) Resume(ctx context.Context, r *taskv3.ResumeRequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if rep := t.sup.Unpause(); !rep.OK {
		return nil, errPrecondition("%s", rep.Error)
	}
	s.events.publish("/tasks/resumed", &eventsapi.TaskResumed{ContainerID: r.ID})
	return &emptypb.Empty{}, nil
}

// ResizePty resizes the terminal of the task's process, or an exec's.
func (s *service) ResizePty(ctx context.Context, r *taskv3.ResizePtyRequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if r.Width == 0 || r.Height == 0 || r.Width > 65535 || r.Height > 65535 {
		return &emptypb.Empty{}, nil
	}
	m := guestspec.Control{Rows: uint16(r.Height), Cols: uint16(r.Width)}
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		if e.started && !isClosed(e.exited) {
			e.sess.Send(m)
		}
		return &emptypb.Empty{}, nil
	}
	t.sup.Control(m)
	return &emptypb.Empty{}, nil
}

// CloseIO ends the input of the task's process, or an exec's.
func (s *service) CloseIO(ctx context.Context, r *taskv3.CloseIORequest) (*emptypb.Empty, error) {
	t, err := s.get()
	if err != nil {
		return nil, err
	}
	if !r.Stdin {
		return &emptypb.Empty{}, nil
	}
	pio := t.io
	if r.ExecID != "" {
		e, err := s.exec(r.ExecID)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		pio = e.io
		s.mu.Unlock()
	}
	if pio != nil && pio.stdin != nil {
		pio.stdin.close()
	}
	return &emptypb.Empty{}, nil
}

// Connect says which process serves the task: the shim, which also runs
// its VM.
func (s *service) Connect(ctx context.Context, r *taskv3.ConnectRequest) (*taskv3.ConnectResponse, error) {
	resp := &taskv3.ConnectResponse{ShimPid: uint32(os.Getpid())}
	if _, err := s.get(); err == nil {
		resp.TaskPid = uint32(os.Getpid())
	}
	return resp, nil
}

// Shutdown ends the shim once its task is gone, or at once with Now.
func (s *service) Shutdown(ctx context.Context, r *taskv3.ShutdownRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	busy := s.task != nil && !s.task.deleted
	s.mu.Unlock()
	if busy && !r.Now {
		return &emptypb.Empty{}, nil
	}
	s.quitMu.Do(func() { close(s.quit) })
	return &emptypb.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, r *taskv3.StatsRequest) (*taskv3.StatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "runwhp's shim does not report stats yet; runwhp stats does, with RUNWHP_ROOT set to the shim's store")
}

func (s *service) Update(ctx context.Context, r *taskv3.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "a runwhp container's resources cannot be changed while it runs")
}

func (s *service) Checkpoint(ctx context.Context, r *taskv3.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "runwhp cannot checkpoint containers")
}
