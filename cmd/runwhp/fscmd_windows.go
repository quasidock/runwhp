//go:build windows

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
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

const commitUsage = `Usage: runwhp commit [-a AUTHOR] [-c CHANGE]... [-m MESSAGE] [-p=false] CONTAINER [REPOSITORY[:TAG]]

Make an image of a container's changes, as docker commit does: its
image's layers, with what the container changed as one more layer, and
its configuration, the container's command, environment, working
directory and user included. Prints the image's ID. A container that
changed nothing adds no layer. The container may be running, created,
paused or stopped; a running one is paused while its changes are read.
Its volumes and bind mounts are not its changes.

  -a, --author AUTHOR    the image's author, as in "Jane <jane@example.com>"
  -c, --change CHANGE    apply a Dockerfile instruction to the image's
                         configuration: CMD, ENTRYPOINT, ENV, EXPOSE, LABEL,
                         ONBUILD, STOPSIGNAL, USER, VOLUME or WORKDIR
  -m, --message MESSAGE  the comment of the image's new history entry
  -p, --pause            pause a running container while committing (default true)
`

func cmdCommit(args []string, stdout io.Writer) int {
	fs := newFlags("commit", commitUsage)
	var author, message string
	var changes listFlag
	pause := true
	fs.StringVar(&author, "a", "", "")
	fs.StringVar(&author, "author", "", "")
	fs.Var(&changes, "c", "")
	fs.Var(&changes, "change", "")
	fs.StringVar(&message, "m", "", "")
	fs.StringVar(&message, "message", "", "")
	fs.BoolVar(&pause, "p", true, "")
	fs.BoolVar(&pause, "pause", true, "")
	if fs.Parse(args) != nil || fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	ref := fs.Arg(1)
	if strings.Contains(ref, "@") {
		errorf("commit: %s: a reference with a digest cannot be committed to", ref)
		return 1
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	// A prune waits until the image is recorded, and keeps the layers it
	// builds on.
	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	ctx, stop := interruptible()
	defer stop()
	rc, err := openRoot(ctx, store, root, fs.Arg(0))
	if err != nil {
		errorf("commit: %v", err)
		return 1
	}
	defer rc.close()
	c := rc.c
	is := image.Open(root)
	commit, err := commitOf(is, c, changes, author, message)
	if err != nil {
		errorf("commit: %v", err)
		return 1
	}

	// Paused while its changes are read, so that they are whole.
	if pause && !rc.booted && c.State.Status == container.Running && !c.State.Paused {
		if err := askPause(root, c, container.OpPause); err != nil {
			errorf("commit: %v", err)
			return 1
		}
		defer func() {
			if err := askPause(root, c, container.OpUnpause); err != nil {
				errorf("commit: %v", err)
			}
		}()
	}
	diff, err := rc.tarOf(ctx, guestspec.RootDiff)
	if err != nil {
		errorf("commit: %v", err)
		return 1
	}
	defer diff.Close()
	img, err := is.Commit(commit, diff, ref)
	if err != nil {
		errorf("commit: %v", err)
		return 1
	}
	fmt.Fprintln(stdout, img.ID())
	return 0
}

// commitOf describes the image commit makes of c: its image's
// configuration with the container's over it, as docker commit makes it.
func commitOf(is *image.Store, c *container.Container, changes []string, author, message string) (image.Commit, error) {
	cfg := c.Config
	commit := image.Commit{Container: c.ID, Changes: changes, Author: author, Message: message, Layers: cfg.Layers, Now: time.Now()}
	var raw []byte
	if cfg.ImageID != "" {
		raw, _ = is.Blob(cfg.ImageID)
	}
	if raw == nil && cfg.ImageName != "" {
		// Containers made before iteration 15 do not record their image's
		// ID: their image's record, if it is still that image.
		if img, err := is.Get(cfg.ImageName); err == nil && img.Digest == cfg.ImageDigest {
			raw = img.Config
		}
	}
	if raw != nil {
		parent, err := v1.ParseConfigFile(bytes.NewReader(raw))
		if err != nil {
			return commit, err
		}
		commit.Parent = parent
		commit.Config = parent.Config
	} else {
		errorf("commit: warning: the configuration of image %s is no longer in the store: the image takes the container's alone, with no history before it", imageOf(cfg))
	}
	if len(cfg.LayerFiles) > 0 {
		return commit, errors.New("a container made from --layer files cannot be committed")
	}

	k := &commit.Config
	k.Hostname, k.User, k.Env, k.WorkingDir, k.Image = cfg.Hostname, cfg.User, cfg.Env, cfg.Cwd, cfg.Image
	k.Tty, k.OpenStdin, k.StdinOnce = cfg.TTY, cfg.Stdin, cfg.StdinOnce
	cmd := cfg.Cmd
	if cfg.Entrypoint != nil || cfg.Cmd != nil {
		k.Entrypoint, k.Cmd = cfg.Entrypoint, cfg.Cmd
	} else {
		// Before iteration 15, a container kept its command whole.
		k.Entrypoint, k.Cmd, cmd = nil, cfg.Args, cfg.Args
	}
	for _, p := range cfg.Publish {
		if k.ExposedPorts == nil {
			k.ExposedPorts = map[string]struct{}{}
		}
		k.ExposedPorts[fmt.Sprintf("%d/tcp", p.Guest)] = struct{}{}
	}
	for _, m := range cfg.Mounts {
		if m.Kind == guestspec.MountAnon {
			if k.Volumes == nil {
				k.Volumes = map[string]struct{}{}
			}
			k.Volumes[m.Target] = struct{}{}
		}
	}
	commit.CreatedBy = strings.Join(cmd, " ")
	return commit, nil
}
