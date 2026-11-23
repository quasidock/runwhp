//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
)

func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: runwhp pull IMAGE\n\nPulls the image, and runwhp's kernel if it is missing, into the local store.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// A prune waits until the image is recorded, and keeps its layers.
	release, err := gc.Hold(root)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer release()
	store := image.Open(root)
	store.Progress = os.Stderr
	img, err := store.Pull(ctx, fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return 1
	}
	// Also fetch the kernel if it is missing, so that a machine prepared
	// with pull can run the image offline.
	if os.Getenv("RUNWHP_KERNEL") == "" {
		kernels := kernel.Open(root)
		kernels.Progress = os.Stderr
		if _, err := kernels.Ensure(ctx, kernel.Default, true); err != nil {
			errorf("%v", err)
			return 1
		}
	}
	fmt.Println(img.Digest)
	return 0
}
