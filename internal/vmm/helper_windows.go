//go:build windows

package vmm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/virtio"
	"github.com/quasidock/runwhp/internal/vm"
)

// A layer server is a guest runwhp boots for itself, not for a container:
// its disks are image layers, read-only, which init mounts each on its
// own, and it serves their files over vsock, for save to rebuild the
// layers' tars from their metadata. It runs no process, and has no
// network, balloon or writable disk: the console's control port, the
// vsock device and the layers.

const (
	serverMemory  = 256 << 20
	serverCPUs    = 2
	serverTimeout = 30 * time.Second // to boot and report ready
)

// serverLayers is how many layers one server takes: every I/O APIC line
// but the console's and the vsock device's.
const serverLayers = vm.MaxDevices - 2

type layerServer struct {
	vsock  *virtio.Vsock
	files  []*disk.File
	cancel context.CancelFunc
	done   chan error
}

// bootLayerServer boots a guest serving the layers at paths, and returns
// once init is ready to serve them.
func bootLayerServer(ctx context.Context, root string, paths []string, debug bool) (*layerServer, error) {
	if len(paths) > serverLayers {
		return nil, fmt.Errorf("%d layers are more than one guest can serve", len(paths))
	}
	g, err := loadGuest(ctx, root, ExplicitKernel(""), nil, "", image.PullMissing)
	if err != nil {
		return nil, err
	}
	spec := guestspec.Spec{Layers: len(paths), Serve: guestspec.ServeLayers, Debug: debug}
	initrd, err := g.initramfs(&spec)
	if err != nil {
		return nil, err
	}
	ready := make(chan struct{})
	var once sync.Once
	status := &eventWriter{onReady: func() { once.Do(func() { close(ready) }) }}
	logf := func(string, ...any) {}
	if debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	}
	s := &layerServer{vsock: &virtio.Vsock{CID: guestspec.GuestCID, Logf: logf}, done: make(chan error, 1)}
	devices := []virtio.Device{
		&virtio.Console{Ports: []virtio.Port{{Name: guestspec.PortControl, Out: status}}, Logf: logf},
		s.vsock,
	}
	for i, p := range paths {
		f, err := disk.Open(p, disk.ReadOnly)
		if err != nil {
			s.closeFiles()
			return nil, err
		}
		s.files = append(s.files, f)
		size, err := f.Size()
		if err != nil {
			s.closeFiles()
			return nil, err
		}
		devices = append(devices, &virtio.Blk{File: f, Sectors: uint64(size) / 512, ReadOnly: true, Serial: guestspec.LayerSerial(i)})
	}
	tail := &ring{max: consoleTail}
	var tailMu sync.Mutex
	consoleLine := func(line string) {
		tailMu.Lock()
		tail.add(line)
		tailMu.Unlock()
	}
	if debug {
		consoleLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	}
	vmCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go func() {
		_, err := vm.Run(vmCtx, vm.Config{
			Kernel:      g.kernel,
			Initrd:      initrd,
			Cmdline:     guestCmdline(debug),
			Memory:      serverMemory,
			CPUs:        min(serverCPUs, runtime.NumCPU()),
			Devices:     devices,
			ConsoleLine: consoleLine,
			Logf:        logf,
		})
		s.done <- err
	}()
	fail := func(err error) (*layerServer, error) {
		tailMu.Lock()
		tail.print(os.Stderr)
		tailMu.Unlock()
		return nil, err
	}
	select {
	case <-ready:
		return s, nil
	case err := <-s.done:
		s.done <- err
		s.close()
		if st, ok := status.get(); ok && st.Error != "" {
			return fail(fmt.Errorf("the guest serving layers failed: %s", st.Error))
		}
		if err == nil {
			err = errors.New("it stopped")
		}
		return fail(fmt.Errorf("the guest serving layers failed: %w", err))
	case <-time.After(serverTimeout):
		s.close()
		return fail(fmt.Errorf("the guest serving layers was not ready after %v", serverTimeout))
	case <-ctx.Done():
		s.close()
		return nil, ctx.Err()
	}
}

// close stops the guest and lets go of its disks. The disks are
// read-only, so stopping it at once loses nothing.
func (s *layerServer) close() {
	s.cancel()
	err := <-s.done
	s.done <- err
	s.closeFiles()
}

func (s *layerServer) closeFiles() {
	for _, f := range s.files {
		f.Close()
	}
	s.files = nil
}

// open asks the guest for the files names of its layer i, whose sizes the
// layer's metadata gives.
func (s *layerServer) open(ctx context.Context, i int, names []string, sizes []int64) (*serverFiles, error) {
	conn, err := s.vsock.Dial(ctx, guestspec.ArchivePort)
	if err != nil {
		return nil, fmt.Errorf("connecting to the guest serving layers: %w", err)
	}
	// init reads the whole request before it answers.
	if err := guestspec.WriteFilesRequest(conn, i, names); err != nil {
		conn.Close()
		return nil, err
	}
	return &serverFiles{conn: conn, r: bufio.NewReaderSize(conn, 256<<10), names: names, sizes: sizes}, nil
}

// serverFiles reads the files of one layer from its server, in order.
type serverFiles struct {
	conn  io.Closer
	r     *bufio.Reader
	names []string
	sizes []int64
	next  int
	cur   *io.LimitedReader
}

func (f *serverFiles) Get(name string) (io.ReadCloser, error) {
	// What the last file left unread, if anything, comes before this one.
	if f.cur != nil && f.cur.N > 0 {
		if _, err := io.Copy(io.Discard, f.cur); err != nil {
			return nil, err
		}
	}
	if f.next >= len(f.names) || f.names[f.next] != name {
		return nil, fmt.Errorf("%w: %q", image.ErrMismatch, name)
	}
	want := f.sizes[f.next]
	f.next++
	size, err := guestspec.ReadFileHeader(f.r)
	if err != nil {
		return nil, err
	}
	if size != want {
		return nil, fmt.Errorf("%q is %d bytes, not the %d its tar gave it", name, size, want)
	}
	f.cur = &io.LimitedReader{R: f.r, N: size}
	return io.NopCloser(f.cur), nil
}

func (f *serverFiles) Close() error { return f.conn.Close() }

// LayerServers serves the layers save rebuilds, in the order it rebuilds
// them, booting a server for each run of up to serverLayers of them as it
// comes to it.
type LayerServers struct {
	ctx    context.Context
	root   string
	layers *layer.Store
	order  []string // image.SaveLayers
	debug  bool

	cur      *layerServer
	curStart int
	curLen   int
}

func (ls *LayerServers) Open(diffID string, names []string, sizes []int64) (image.LayerFiles, error) {
	i := slices.Index(ls.order, diffID)
	if i < 0 {
		return nil, fmt.Errorf("layer %s was not planned", diffID)
	}
	if ls.cur == nil || i < ls.curStart || i >= ls.curStart+ls.curLen {
		ls.Close()
		batch := ls.order[i:min(i+serverLayers, len(ls.order))]
		var paths []string
		for _, d := range batch {
			p, err := ls.layers.Path(d)
			if err != nil {
				return nil, err
			}
			paths = append(paths, p)
		}
		s, err := bootLayerServer(ls.ctx, ls.root, paths, ls.debug)
		if err != nil {
			return nil, err
		}
		ls.cur, ls.curStart, ls.curLen = s, i, len(batch)
	}
	return ls.cur.open(ls.ctx, i-ls.curStart, names, sizes)
}

// Close stops the server running, if any.
func (ls *LayerServers) Close() {
	if ls.cur != nil {
		ls.cur.close()
		ls.cur = nil
	}
}

// A Guest is one runwhp boots from a container's config in the background,
// for something other than a container's run: to reach its files, for cp,
// export, diff and commit of a stopped container, or for a step of a
// build. Init mounts its root as it would for the container, and serves it
// on guestspec.RootPort.
//
// A served guest runs no process: it waits for the host to send Shutdown,
// and has no network device or balloon, and a root server's memory and
// processors. A process guest waits for Run to start its process, and
// stops when the process ends, as a container's does.
type Guest struct {
	vsock   *virtio.Vsock
	queue   *controlQueue
	layer   *PendingLayer
	process bool
	cancel  context.CancelFunc
	done    chan guestResult
	logf    func(string, ...any)
}

// guestResult is how a guest's boot ended.
type guestResult struct {
	code int
	err  error
}

// rootShutdownWait bounds how long a served guest takes to freeze its file
// systems and stop once asked.
const rootShutdownWait = 10 * time.Second

// BootRootServer boots a guest serving the files of the stopped container
// c: its layers, its writable layer, its volumes and its shares. The caller
// holds the container's lock throughout, so the container cannot start
// meanwhile.
func BootRootServer(ctx context.Context, root string, c *container.Container) (*Guest, error) {
	// The writable layer, if the container has one: a container whose
	// writable layer is in memory, or that never booted, has none, and
	// init gives it an empty one.
	var rw *disk.File
	if f, err := disk.Open(c.RW(), disk.Exclusive); err == nil {
		rw = f
	} else if errors.Is(err, disk.ErrInUse) {
		return nil, fmt.Errorf("container %s's writable layer is in use", c.Short())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return bootGuest(ctx, root, c.Config, ReadyLayer(rw, false), false, BootIO{}, "the guest serving container "+c.Short()+"'s files")
}

// bootGuest boots a guest of the root cfg describes, with layer as its
// writable layer, which the guest closes when it stops, and returns once
// init is ready: to serve, or, for a process guest, to start the process,
// whose streams bio holds. what names the guest in errors.
func bootGuest(ctx context.Context, root string, cfg *container.Config, layer *PendingLayer, process bool, bio BootIO, what string) (*Guest, error) {
	logf := func(string, ...any) {}
	if cfg.Debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	}
	// The guest outlives ctx, which bounds only its boot; Run, Close or
	// stop ends it.
	vmCtx, cancel := context.WithCancel(context.Background())
	proxy := NewSignalProxy(cancel, logf)
	g := &Guest{queue: proxy.queue, layer: layer, process: process, cancel: cancel, done: make(chan guestResult, 1), logf: logf}
	ready := make(chan struct{})
	var once sync.Once
	hooks := bootHooks{
		ready:    func() { once.Do(func() { close(ready) }) },
		attached: func(h *vmHandles) { g.vsock = h.vsock },
	}
	opts := guestOpts{serve: guestspec.ServeRoot, noNIC: true, noBalloon: true, memory: serverMemory, cpus: min(serverCPUs, runtime.NumCPU())}
	if process {
		opts, hooks.waitStart = guestOpts{}, true
	}
	if bio.Diag == nil {
		bio.Diag = os.Stderr
	}
	go func() {
		code, err := bootContainer(vmCtx, root, cfg, layer, opts, image.PullMissing, proxy, bio, hooks, logf, time.Now())
		g.done <- guestResult{code, err}
	}()
	select {
	case <-ready:
		return g, nil
	case res := <-g.done:
		g.done <- res
		g.stop()
		err := res.err
		if err == nil {
			err = errors.New("it stopped")
		}
		return nil, fmt.Errorf("%s failed: %w", what, err)
	case <-time.After(serverTimeout):
		g.stop()
		return nil, fmt.Errorf("%s was not ready after %v", what, serverTimeout)
	case <-ctx.Done():
		g.stop()
		return nil, ctx.Err()
	}
}

// Dial connects to the guest's root server.
func (g *Guest) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	return g.vsock.Dial(ctx, guestspec.RootPort)
}

// Run starts a process guest's process, and returns its exit code once it
// ends, and the guest with it. If ctx ends first, Run stops the guest, and
// returns 130.
func (g *Guest) Run(ctx context.Context) (int, error) {
	if !g.process {
		return guestspec.ExitRuntime, errors.New("the guest runs no process")
	}
	g.queue.send(guestspec.Control{Start: true})
	select {
	case res := <-g.done:
		g.done <- res
		return res.code, res.err
	case <-ctx.Done():
		g.stop()
		return 130, ctx.Err()
	}
}

// Close stops the guest and lets go of its disks: a served guest is asked
// to freeze its writable layer and volumes and stop, and waited for; a
// process guest whose process has not ended is stopped at once.
func (g *Guest) Close() {
	if !g.process {
		g.queue.send(guestspec.Control{Shutdown: true})
		select {
		case res := <-g.done:
			g.done <- res
		case <-time.After(rootShutdownWait):
			g.logf("the guest serving files had not stopped %v after it was asked to; stopping it", rootShutdownWait)
		}
	}
	g.stop()
}

// stop stops the machine at once, and lets go of the disks.
func (g *Guest) stop() {
	g.cancel()
	res := <-g.done
	g.done <- res
	g.layer.Close()
}
