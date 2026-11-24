//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins/snapshots/lcow"
	"google.golang.org/grpc"
)

// runwhp's snapshotter is containerd's LCOW snapshotter, whose layers are
// ext4 images, layer.vhd, which containerd's windows-lcow differ makes,
// with two changes:
//
//   - Views have no mounts. nerdctl mounts a view of every image it runs
//     on the host, to read its users and copy its volumes' files, and
//     Windows cannot mount an LCOW layer: an empty list of mounts is one
//     it can. Nothing else on the host reads them.
//   - Its root holds an empty scratch.vhdx, which the snapshotter copies
//     into each container's snapshot as sandbox.vhdx, instead of making
//     one with runhcs, which needs an administrator. runwhp's writable
//     layer is rw.ext4 beside it, made on the container's first boot.
//
// containerd reaches it as a proxy plugin, over a named pipe.

// cmdSnapshotter serves the snapshotter until interrupted.
func cmdSnapshotter(args []string) int {
	fs := flag.NewFlagSet("containerd-shim-runwhp-v1 snapshotter", flag.ContinueOnError)
	root := fs.String("root", "", "directory to keep snapshots in, on NTFS")
	address := fs.String("address", "", "named pipe to serve containerd on")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *address == "" {
		fmt.Fprintln(os.Stderr, "usage: containerd-shim-runwhp-v1 snapshotter --root DIR --address PIPE")
		return 2
	}
	pipe := *address
	if !strings.HasPrefix(pipe, `\\`) {
		pipe = `\\.\pipe\` + pipe
	}
	if err := serveSnapshotter(*root, pipe); err != nil {
		fmt.Fprintf(os.Stderr, "runwhp snapshotter: %v\n", err)
		return 1
	}
	return 0
}

func serveSnapshotter(root, pipe string) error {
	sn, err := newSnapshotter(root)
	if err != nil {
		return err
	}
	defer sn.Close()
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(srv, snapshotservice.FromSnapshotter(sn))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		srv.GracefulStop()
	}()
	fmt.Fprintf(os.Stderr, "runwhp snapshotter serving %s on %s\n", root, pipe)
	return srv.Serve(l)
}

// newSnapshotter opens runwhp's snapshotter at root.
func newSnapshotter(root string) (snapshots.Snapshotter, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	scratch := filepath.Join(root, "scratch.vhdx")
	if _, err := os.Stat(scratch); os.IsNotExist(err) {
		if err := os.WriteFile(scratch, nil, 0o644); err != nil {
			return nil, err
		}
	}
	sn, err := lcow.NewSnapshotter(root)
	if err != nil {
		return nil, err
	}
	return viewless{sn}, nil
}

// viewless is a snapshotter whose views have no mounts.
type viewless struct{ snapshots.Snapshotter }

func (v viewless) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	_, err := v.Snapshotter.View(ctx, key, parent, opts...)
	return nil, err
}

func (v viewless) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	info, err := v.Snapshotter.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if info.Kind == snapshots.KindView {
		return nil, nil
	}
	return v.Snapshotter.Mounts(ctx, key)
}
