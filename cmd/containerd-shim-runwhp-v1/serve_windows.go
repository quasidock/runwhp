//go:build windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"

	"github.com/quasidock/runwhp/internal/vmm"
)

// storeRoot is runwhp's store the shim keeps its containers in:
// RUNWHP_ROOT, which runwhp containerd sets, else one of its own beside
// runwhp's, so that runwhp's prunes leave containerd's containers alone.
func storeRoot() (string, error) {
	if r := os.Getenv("RUNWHP_ROOT"); r != "" {
		return r, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "runwhp", "containerd", "store"), nil
}

// cmdServe is the shim proper: it serves the task API on o.socket until
// containerd shuts it down.
func cmdServe(o options) int {
	if o.socket == "" {
		fmt.Fprintln(os.Stderr, "containerd-shim-runwhp-v1: serving needs -socket")
		return 2
	}
	go serveLogPipe(logAddress(o.namespace, o.id))
	root, err := storeRoot()
	if err != nil {
		diagf("%v", err)
		return 1
	}
	events := newPublisher(os.Getenv("TTRPC_ADDRESS"), o.namespace)
	svc := newService(o.namespace, o.id, root, events)
	l, err := winio.ListenPipe(o.socket, nil)
	if err != nil {
		diagf("listening on %s: %v", o.socket, err)
		return 1
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		diagf("%v", err)
		return 1
	}
	taskv3.RegisterTTRPCTaskService(srv, svc)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background(), l) }()
	select {
	case <-svc.quit:
	case err := <-served:
		diagf("serving the task API: %v", err)
	}
	// Let Shutdown's answer, and the last events, go out.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	srv.Shutdown(ctx)
	cancel()
	events.close()
	return 0
}

// The shim's own messages go to containerd, which reads them from the log
// pipe while it is connected, and, once the task has a container, to that
// container's runwhp.log too.
var diag = &diagWriter{}

type diagWriter struct {
	mu   sync.Mutex
	pipe net.Conn
	file *os.File
}

func (d *diagWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pipe != nil {
		if _, err := d.pipe.Write(p); err != nil {
			d.pipe.Close()
			d.pipe = nil
		}
	}
	if d.file != nil {
		d.file.Write(p)
	}
	return len(p), nil
}

func init() { vmm.Diag = diag }

func diagf(format string, a ...any) {
	fmt.Fprintf(diag, "%s runwhp shim %d: %s\n", time.Now().Format(time.RFC3339Nano), os.Getpid(), fmt.Sprintf(format, a...))
}

// logDiagTo copies the shim's messages to the file at path too.
func logDiagTo(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		diagf("opening %s: %v", path, err)
		return
	}
	diag.mu.Lock()
	if diag.file != nil {
		diag.file.Close()
	}
	diag.file = f
	diag.mu.Unlock()
}

// resetDiag closes the file logDiagTo opened, so that its directory can
// be removed.
func resetDiag() {
	diag.mu.Lock()
	defer diag.mu.Unlock()
	if diag.file != nil {
		diag.file.Close()
		diag.file = nil
	}
}

// serveLogPipe serves containerd the shim's messages on the log pipe, to
// one reader at a time, again after containerd restarts.
func serveLogPipe(path string) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		diag.mu.Lock()
		if diag.pipe != nil {
			diag.pipe.Close()
		}
		diag.pipe = c
		diag.mu.Unlock()
	}
}
