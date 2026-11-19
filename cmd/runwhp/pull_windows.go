//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/quasidock/runwhp/internal/image"
)

func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), "Usage: runwhp pull IMAGE") }
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
	store := image.Open(root)
	store.Progress = os.Stderr
	img, err := store.Pull(ctx, fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return 1
	}
	fmt.Println(img.Digest)
	return 0
}
