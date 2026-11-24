//go:build windows

package main

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/quasidock/runwhp/internal/guestspec"
)

const exportUsage = `Usage: runwhp export [-o FILE] CONTAINER

Write a container's root file system to a tar, to stdout unless -o, as
docker export does: its image's files with its changes, without what its
volumes and bind mounts hold. The container may be running, created,
paused or stopped.

  -o, --output FILE   write to FILE rather than stdout
`

func cmdExport(args []string, stdout io.Writer) int {
	fs := newFlags("export", exportUsage)
	var output string
	fs.StringVar(&output, "o", "", "")
	fs.StringVar(&output, "output", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if f, ok := stdout.(*os.File); ok && output == "" && isTerminal(f) {
		errorf("export: cowardly refusing to save to a terminal. Use the -o flag or redirect")
		return 1
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	root, _ := storeRoot()
	ctx, stop := interruptible()
	defer stop()
	rc, err := openRoot(ctx, store, root, fs.Arg(0))
	if err != nil {
		errorf("export: %v", err)
		return 1
	}
	defer rc.close()
	write := func(w io.Writer) error {
		content, err := rc.tarOf(ctx, guestspec.RootExport)
		if err != nil {
			return err
		}
		defer content.Close()
		bw := bufio.NewWriterSize(w, 1<<20)
		if err := copyTar(bw, content); err != nil {
			return err
		}
		return bw.Flush()
	}
	if output == "" {
		err = write(stdout)
	} else {
		err = writeFileAtomically(output, write)
	}
	if err != nil {
		errorf("export: %v", err)
		return 1
	}
	return 0
}

// copyTar copies the tar src to dst, as it comes, and fails if it ends
// before its end-of-archive marker: init ends a tar it cannot finish.
func copyTar(dst io.Writer, src io.Reader) error {
	// A tar cut between two entries reads to tar as one that ends there;
	// tarChecker catches it lacking its end marker.
	src = &tarChecker{r: src}
	tr := tar.NewReader(io.TeeReader(src, dst))
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the container's file server ended the tar early: %w", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("the container's file server ended the tar early: %w", err)
		}
	}
	// Whatever follows the end marker: padding to a record, if any.
	_, err := io.Copy(dst, src)
	return err
}

const diffUsage = `Usage: runwhp diff CONTAINER

List what a container changed in its image's files, as docker diff does:
A for a path added, C for one changed, D for one deleted, with every
directory above a change. Volumes and bind mounts are not the
container's changes. The container may be running, created, paused or
stopped.
`

func cmdDiff(args []string, stdout io.Writer) int {
	fs := newFlags("diff", diffUsage)
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	root, _ := storeRoot()
	ctx, stop := interruptible()
	defer stop()
	rc, err := openRoot(ctx, store, root, fs.Arg(0))
	if err != nil {
		errorf("diff: %v", err)
		return 1
	}
	defer rc.close()
	cs, err := rc.changes(ctx)
	if err != nil {
		errorf("diff: %v", err)
		return 1
	}
	w := bufio.NewWriter(stdout)
	for _, c := range cs {
		kind := "C"
		switch c.Kind {
		case guestspec.ChangeAdd:
			kind = "A"
		case guestspec.ChangeDelete:
			kind = "D"
		}
		fmt.Fprintf(w, "%s %s\n", kind, c.Path)
	}
	w.Flush()
	return 0
}
