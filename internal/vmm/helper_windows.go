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

// A root server is a guest runwhp boots with a stopped container's
// storage, for cp, export, diff and commit: its layers, its writable
// layer, its volumes and its shares, which init mounts as it would for the
// container, serving its files on guestspec.RootPort. It runs no process,
// and has no network or balloon. The caller holds the container's lock
// throughout, so the container cannot start meanwhile.
type RootGuest struct {
	vsock   *virtio.Vsock
	queue   *controlQueue
	storage *containerStorage
	rw      *disk.File // nil: the container has no writable layer yet
	cancel  context.CancelFunc
	done    chan error
	logf    func(string, ...any)
}

// rootShutdownWait bounds how long a root server takes to freeze its file
// systems and stop once asked.
const rootShutdownWait = 10 * time.Second

// BootRootServer boots a root server for the stopped container c, and
// returns once init is ready to serve.
func BootRootServer(ctx context.Context, root string, c *container.Container) (_ *RootGuest, err error) {
	cfg := c.Config
	logf := func(string, ...any) {}
	if cfg.Debug {
		logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	}
	g := &RootGuest{vsock: &virtio.Vsock{CID: guestspec.GuestCID, Logf: logf}, queue: newControlQueue(), done: make(chan error, 1), logf: logf}
	defer func() {
		if err != nil {
			g.closeFiles()
		}
	}()
	if g.storage, err = openStorage(root, cfg, logf); err != nil {
		return nil, err
	}
	// The writable layer, if the container has one: a container whose
	// writable layer is in memory, or that never booted, has none, and
	// init gives it an empty one.
	if f, err := disk.Open(c.RW(), disk.Exclusive); err == nil {
		size, err := f.Size()
		if err != nil {
			f.Close()
			return nil, err
		}
		g.rw = f
		g.storage.setScratch(f, size, true)
	} else if errors.Is(err, disk.ErrInUse) {
		return nil, fmt.Errorf("container %s's writable layer is in use", c.Short())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	guestFiles, err := loadGuest(ctx, root, cfg.Kernel, cfg.Modules, cfg.Init, image.PullMissing)
	if err != nil {
		return nil, err
	}
	spec := guestspec.Spec{
		User: cfg.User, Hostname: cfg.Hostname, Hosts: cfg.Hosts, Network: GuestNetwork(cfg.Network),
		Layers: len(g.storage.layers), Scratch: g.rw != nil, KeepLayer: g.rw != nil, Volumes: len(g.storage.volumes),
		Mounts: cfg.Mounts, Debug: cfg.Debug, Serve: guestspec.ServeRoot,
	}
	initrd, err := guestFiles.initramfs(&spec)
	if err != nil {
		return nil, err
	}
	ready := make(chan struct{})
	var once sync.Once
	status := &eventWriter{onReady: func() { once.Do(func() { close(ready) }) }}
	devices := append([]virtio.Device{
		&virtio.Console{Ports: []virtio.Port{{Name: guestspec.PortControl, Out: status, In: g.queue}}, Logf: logf},
		g.vsock,
	}, g.storage.devices()...)
	// Fewer than the container boots with: no network or balloon.
	if len(devices) > vm.MaxDevices {
		return nil, fmt.Errorf("container %s has more devices than a guest can take", c.Short())
	}
	tail := &ring{max: consoleTail}
	var tailMu sync.Mutex
	consoleLine := func(line string) {
		tailMu.Lock()
		tail.add(line)
		tailMu.Unlock()
	}
	if cfg.Debug {
		consoleLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	}
	vmCtx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	go func() {
		_, err := vm.Run(vmCtx, vm.Config{
			Kernel: guestFiles.kernel, Initrd: initrd, Cmdline: guestCmdline(cfg.Debug),
			Memory: serverMemory, CPUs: min(serverCPUs, runtime.NumCPU()),
			Devices: devices, ConsoleLine: consoleLine, Logf: logf,
		})
		g.done <- err
	}()
	fail := func(err error) (*RootGuest, error) {
		tailMu.Lock()
		tail.print(os.Stderr)
		tailMu.Unlock()
		return nil, err
	}
	select {
	case <-ready:
		return g, nil
	case err := <-g.done:
		g.done <- err
		g.stop()
		if st, ok := status.get(); ok && st.Error != "" {
			return fail(fmt.Errorf("the guest serving container %s's files failed: %s", c.Short(), st.Error))
		}
		if err == nil {
			err = errors.New("it stopped")
		}
		return fail(fmt.Errorf("the guest serving container %s's files failed: %w", c.Short(), err))
	case <-time.After(serverTimeout):
		g.stop()
		return fail(fmt.Errorf("the guest serving container %s's files was not ready after %v", c.Short(), serverTimeout))
	case <-ctx.Done():
		g.stop()
		return nil, ctx.Err()
	}
}

// Dial connects to the guest's root server.
func (g *RootGuest) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	return g.vsock.Dial(ctx, guestspec.RootPort)
}

// Close asks init to freeze the writable layer and volumes and stop the
// machine, waits for it, and lets go of the disks.
func (g *RootGuest) Close() {
	g.queue.send(guestspec.Control{Shutdown: true})
	select {
	case err := <-g.done:
		g.done <- err
	case <-time.After(rootShutdownWait):
		g.logf("the guest serving files had not stopped %v after it was asked to; stopping it", rootShutdownWait)
	}
	g.stop()
}

// stop stops the machine at once, and lets go of the disks.
func (g *RootGuest) stop() {
	if g.cancel != nil {
		g.cancel()
		err := <-g.done
		g.done <- err
	}
	g.closeFiles()
}

func (g *RootGuest) closeFiles() {
	if g.storage != nil {
		g.storage.close()
	}
	if g.rw != nil {
		g.rw.Close()
		g.rw = nil
	}
}
