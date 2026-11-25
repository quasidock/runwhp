//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
)

const builderUsage = `Usage:
  runwhp builder prune [-a] [-f]   remove build cache

builder prune removes the build cache entries whose layers no image or
container uses, and those layers; with -a, every entry, and the volumes
RUN --mount=type=cache mounts.
`

// cmdBuilder runs runwhp builder.
func cmdBuilder(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, builderUsage)
		return 2
	}
	switch args[0] {
	case "prune":
		return cmdBuilderPrune(args[1:], stdin, stdout)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, builderUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown builder command %q\n\n%s", args[0], builderUsage)
	return 2
}

func cmdBuilderPrune(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := newFlags("builder prune", builderUsage)
	var all, force bool
	fs.BoolVar(&all, "a", false, "")
	fs.BoolVar(&all, "all", false, "")
	fs.BoolVar(&force, "f", false, "")
	fs.BoolVar(&force, "force", false, "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	warning := "This will remove all dangling build cache."
	if all {
		warning = "This will remove all build cache."
	}
	if !force && !confirm(stdin, warning) {
		return 0
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	release, err := exclusive(root)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer release()
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	r := u.Apply(u.BuildCache(all))
	printSection(stdout, "Deleted build cache objects:", shortKeys(r.Cache))
	fmt.Fprintf(stdout, "Total reclaimed space: %s\n", humanSize(r.Freed))
	if printReport(r) > 0 {
		return 1
	}
	return 0
}

// shortKeys shortens build cache keys as docker builder prune shows its
// objects' IDs.
func shortKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k[:min(len(k), 25)]
	}
	return out
}
