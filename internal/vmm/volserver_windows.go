//go:build windows

package vmm

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/volume"
)

// A shared volume's server: a runwhp of its own, `runwhp __volume NAME`,
// whose guest has the volume's disk and serves its file system to the
// containers using it, each connection to its pipe one container's guest.
// The first container to need it starts it; it stops volumeGrace after the
// last container's guest stops.

const (
	// volumeGrace is how long a server serving no container waits for
	// another before it stops: RUNWHP_VOLUME_GRACE, in seconds, overrides
	// it.
	volumeGrace = 10 * time.Second
	// volumeMemory is a server's guest's memory, which it takes from
	// Windows only as it uses it, chiefly for the volume's page cache:
	// RUNWHP_VOLUME_MEMORY, in MiB, overrides it.
	volumeMemory = 1 << 30
	volumeCPUs   = 2
	// volumeStart bounds how long a container waits for a server to start
	// serving the volume, its kernel fetched first if need be.
	volumeStart = 60 * time.Second
	// frameSplice carries a READ's or WRITE's frame in one piece.
	frameSplice = 576 << 10
)

// Process creation flags a runwhp in the background is started with.
const (
	detachedProcess  = 0x00000008 // DETACHED_PROCESS
	breakawayFromJob = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
)

func envSeconds(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// connectVolume connects container cfg's guest to the server of the shared
// volume v, starting the server if none runs.
func connectVolume(ctx context.Context, root string, v *volume.Volume, cfg *container.Config, logf func(string, ...any)) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, volumeStart)
	defer cancel()
	for {
		c, err := v.Connect(ctx, cfg.ID)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, volume.ErrNoServer) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("volume %s's server did not start serving in %v", v.Name, volumeStart)
		}
		rep, err := spawnVolumeServer(ctx, root, v.Name, cfg)
		if err != nil {
			return nil, err
		}
		switch {
		case rep.Held:
			return nil, &volume.InUseError{Name: v.Name, Hint: "a container that has its disk to itself is running"}
		case rep.Busy:
			// Another server is starting, or stopping: try again.
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		case !rep.OK:
			return nil, fmt.Errorf("volume %s's server: %s", v.Name, rep.Error)
		}
	}
}

// spawnVolumeServer starts `runwhp __volume` for the volume called name,
// with the kernel, modules and init cfg boots, and returns what it says as
// it starts serving, or why it cannot.
func spawnVolumeServer(ctx context.Context, root, name string, cfg *container.Config) (volume.ServerReply, error) {
	exe, err := os.Executable()
	if err != nil {
		return volume.ServerReply{}, err
	}
	// runwhp-compose.exe and the containerd shim run containers too; the
	// server is runwhp.exe, which starts faster.
	exe = filepath.Join(filepath.Dir(exe), "runwhp.exe")
	abs, err := filepath.Abs(root)
	if err != nil {
		return volume.ServerReply{}, err
	}
	readR, readW, err := os.Pipe()
	if err != nil {
		return volume.ServerReply{}, err
	}
	defer readR.Close()
	args := []string{"__volume", "--ready", strconv.FormatUint(uint64(readW.Fd()), 10)}
	if cfg.Kernel != "" {
		args = append(args, "--kernel", cfg.Kernel)
	}
	for _, m := range cfg.Modules {
		args = append(args, "--module", m)
	}
	if cfg.Init != "" {
		args = append(args, "--init", cfg.Init)
	}
	if cfg.Debug {
		args = append(args, "--debug")
	}
	cmd := exec.Command(exe, append(args, name)...)
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+abs)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              detachedProcess | windows.CREATE_NEW_PROCESS_GROUP | breakawayFromJob,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(readW.Fd())},
	}
	err = cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// This process is in a job its children cannot leave: the
		// server ends with it, as the container does.
		cmd.SysProcAttr.CreationFlags &^= breakawayFromJob
		err = cmd.Start()
	}
	readW.Close()
	if err != nil {
		return volume.ServerReply{}, fmt.Errorf("starting volume %s's server: %w", name, err)
	}
	type result struct {
		rep volume.ServerReply
		err error
	}
	got := make(chan result, 1)
	go func() {
		rep, err := volume.ReadReply(bufio.NewReader(readR))
		got <- result{rep, err}
	}()
	select {
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return volume.ServerReply{}, fmt.Errorf("volume %s's server did not start serving: %w", name, ctx.Err())
	case r := <-got:
		if r.err != nil {
			werr := cmd.Wait()
			return volume.ServerReply{}, fmt.Errorf("volume %s's server ended at once (%v); see its server.log", name, werr)
		}
		cmd.Process.Release()
		return r.rep, nil
	}
}

// VolumeServer serves a shared volume, as `runwhp __volume` does.
type VolumeServer struct {
	Root, Name string
	// Kernel, Modules and Init are what the guest boots, as a container's
	// config has them.
	Kernel  string
	Modules []string
	Init    string
	Debug   bool
	// Log takes the server's diagnostics and its guest's console.
	Log io.Writer

	mu       sync.Mutex
	clients  map[net.Conn]string // connection -> container
	stopping bool
	idle     chan struct{} // signalled when the last client goes
}

func (s *VolumeServer) logf(format string, a ...any) {
	fmt.Fprintf(s.Log, time.Now().Format("2006-01-02T15:04:05.000Z07:00")+" "+format+"\n", a...)
}

// Run serves the volume until it has served no container for its grace,
// or is asked to stop. It writes a volume.ServerReply line on ready once
// it serves, or cannot, and returns the process's exit code.
func (s *VolumeServer) Run(ready io.Writer) int {
	if s.Log == nil {
		s.Log = io.Discard
	}
	answered := false
	answer := func(rep volume.ServerReply) {
		if !answered && ready != nil {
			volume.WriteReply(ready, rep)
			answered = true
		}
	}
	fail := func(err error) int {
		s.logf("%v", err)
		answer(volume.ServerReply{Error: err.Error()})
		return 1
	}
	v, err := volume.Open(s.Root).Get(s.Name)
	if err != nil {
		return fail(err)
	}
	lock, err := s.lock(v)
	if errors.Is(err, volume.ErrServerBusy) {
		answer(volume.ServerReply{Busy: true})
		return 0
	}
	if err != nil {
		return fail(err)
	}
	defer lock.Close()

	memory := uint64(volumeMemory)
	if mib := envSeconds("RUNWHP_VOLUME_MEMORY", 0); mib > 0 {
		memory = uint64(mib/time.Second) << 20
	}
	cfg := &container.Config{
		ID:      "volume-" + v.Name,
		Volumes: []container.Volume{{Name: v.Name, Exclusive: true}},
		Memory:  memory,
		CPUs:    min(int(cmp.Or(envSeconds("RUNWHP_VOLUME_CPUS", 0)/time.Second, volumeCPUs)), runtime.NumCPU()),
		Kernel:  s.Kernel, Modules: s.Modules, Init: s.Init, Debug: s.Debug,
	}
	opts := &guestOpts{serve: guestspec.ServeVolume, noNIC: true}
	ctx, cancel := context.WithTimeout(context.Background(), volumeStart)
	g, err := bootGuest(ctx, s.Root, cfg, ReadyLayer(nil, false), false, opts, BootIO{Diag: s.Log}, "the guest serving volume "+v.Name)
	cancel()
	if errors.Is(err, volume.ErrInUse) {
		s.logf("%v", err)
		answer(volume.ServerReply{Held: true, Error: err.Error()})
		return 0
	}
	if err != nil {
		return fail(err)
	}
	go keepClock(g.queue, g.exited, clockPeriod(s.logf), s.logf)
	sddl, err := container.PipeSecurity()
	if err != nil {
		g.Close()
		return fail(err)
	}
	l, err := winio.ListenPipe(v.PipeName(), &winio.PipeConfig{
		SecurityDescriptor: sddl, InputBufferSize: 256 << 10, OutputBufferSize: 256 << 10,
	})
	if err != nil {
		g.Close()
		s.logf("serving %s: %v", v.PipeName(), err)
		answer(volume.ServerReply{Busy: true})
		return 0
	}
	s.clients = map[net.Conn]string{}
	s.idle = make(chan struct{}, 1)
	stops := make(chan net.Conn)
	go s.accept(l, g, stops)
	s.logf("serving volume %s, pid %d", v.Name, os.Getpid())
	answer(volume.ServerReply{OK: true, PID: os.Getpid()})

	grace := envSeconds("RUNWHP_VOLUME_GRACE", volumeGrace)
	timer := time.NewTimer(grace)
	var stopConn net.Conn
	code := 0
loop:
	for {
		select {
		case <-timer.C:
			if s.beginStop() {
				s.logf("no container has used the volume for %v; stopping", grace)
				break loop
			}
		case <-s.idle:
			timer.Reset(grace)
		case c := <-stops:
			if s.beginStop() {
				s.logf("asked to stop")
				stopConn = c
				break loop
			}
			volume.WriteReply(c, volume.ServerReply{Clients: s.count()})
			c.Close()
		case <-g.Exited():
			s.logf("the guest serving the volume stopped unasked; its containers get I/O errors until they start again")
			s.beginStop()
			code = 1
			break loop
		}
	}
	timer.Stop()
	l.Close()
	s.closeClients()
	g.Close()
	lock.Close()
	if stopConn != nil {
		volume.WriteReply(stopConn, volume.ServerReply{OK: true})
		stopConn.Close()
	}
	s.logf("stopped")
	return code
}

// lock takes the volume's server lock. If another server holds it, it
// waits for that one to serve, or to stop, for a while.
func (s *VolumeServer) lock(v *volume.Volume) (io.Closer, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		l, err := v.LockServer()
		if !errors.Is(err, volume.ErrServerBusy) {
			return l, err
		}
		if _, err := v.ServerStatus(); err == nil || time.Now().After(deadline) {
			return nil, volume.ErrServerBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// beginStop marks the server stopping, unless it serves a container.
func (s *VolumeServer) beginStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) > 0 {
		return false
	}
	s.stopping = true
	return true
}

func (s *VolumeServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *VolumeServer) closeClients() {
	s.mu.Lock()
	s.stopping = true
	for c := range s.clients {
		c.Close()
	}
	s.mu.Unlock()
}

// accept takes connections to the pipe until it is closed.
func (s *VolumeServer) accept(l net.Listener, g *Guest, stops chan<- net.Conn) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.handle(c, g, stops)
	}
}

func (s *VolumeServer) handle(c net.Conn, g *Guest, stops chan<- net.Conn) {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	req, err := volume.ReadRequest(c)
	c.SetDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	switch req.Op {
	case volume.OpStatus:
		s.mu.Lock()
		rep := volume.ServerReply{OK: true, PID: os.Getpid(), Clients: len(s.clients)}
		for _, id := range s.clients {
			rep.Containers = append(rep.Containers, id)
		}
		s.mu.Unlock()
		volume.WriteReply(c, rep)
		c.Close()
	case volume.OpStop:
		stops <- c
	case volume.OpConnect:
		s.connect(c, req.Container, g)
	default:
		volume.WriteReply(c, volume.ServerReply{Error: "unknown op " + strconv.Quote(req.Op)})
		c.Close()
	}
}

// connect carries a container's FUSE frames to the guest, and the replies
// back, until either side ends.
func (s *VolumeServer) connect(c net.Conn, id string, g *Guest) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		volume.WriteReply(c, volume.ServerReply{Busy: true})
		c.Close()
		return
	}
	s.clients[c] = id
	s.mu.Unlock()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.clients, c)
		last := len(s.clients) == 0
		s.mu.Unlock()
		if last {
			select {
			case s.idle <- struct{}{}:
			default:
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	gc, err := g.DialPort(ctx, guestspec.VolumePort)
	cancel()
	if err != nil {
		s.logf("connecting container %s to the guest: %v", id, err)
		volume.WriteReply(c, volume.ServerReply{Error: err.Error()})
		return
	}
	defer gc.Close()
	if err := volume.WriteReply(c, volume.ServerReply{OK: true}); err != nil {
		return
	}
	if s.Debug {
		s.logf("container %s connected", id)
	}
	done := make(chan struct{}, 2)
	go func() {
		io.CopyBuffer(gc, c, make([]byte, frameSplice))
		done <- struct{}{}
	}()
	go func() {
		io.CopyBuffer(c, gc, make([]byte, frameSplice))
		done <- struct{}{}
	}()
	<-done
	if s.Debug {
		s.logf("container %s disconnected", id)
	}
}

// volumeAsyncWait is how long a vCPU waits for a shared volume's reply
// before it goes back to its guest: RUNWHP_VOLUME_WAIT_US, in
// microseconds, overrides it.
func volumeAsyncWait() time.Duration {
	if v, ok := os.LookupEnv("RUNWHP_VOLUME_WAIT_US"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Microsecond
		}
	}
	return time.Millisecond
}
