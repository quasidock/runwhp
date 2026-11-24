//go:build windows

// runwhp cp follows docker cp: the client side of github.com/docker/cli
// 29.7.2's cli/command/container/cp.go (Apache-2.0, Docker, Inc.), with
// runwhp's file server in the container where Docker's daemon would be.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/moby/go-archive"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/vmm"
)

const cpUsage = `Usage: runwhp cp [-a] [-L] [-q] CONTAINER:SRC_PATH DEST_PATH|-
       runwhp cp [-a] [-L] [-q] SRC_PATH|- CONTAINER:DEST_PATH

Copy files and directories between a container and Windows, as docker cp
does. '-' as the source reads a tar from stdin and extracts it into a
directory of the container; as the destination, it writes a tar of the
container's files to stdout. The container may be running, created,
paused or stopped: a stopped one's files are reached through a guest
booted for the purpose, and it cannot start until the copy is done. Its
volumes and bind mounts are copied into and out of too.

  -a, --archive       give what is copied in the owner of the container's
                      user, rather than the tar's
  -L, --follow-link   always follow a symlink in SRC_PATH
  -q, --quiet         say nothing when done; the default without a console
`

type cpOptions struct {
	followLink, copyUIDGID, quiet bool
	source, destination           string
}

func cmdCp(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("cp", cpUsage)
	var o cpOptions
	quietSet := false
	fs.BoolVar(&o.copyUIDGID, "a", false, "")
	fs.BoolVar(&o.copyUIDGID, "archive", false, "")
	fs.BoolVar(&o.followLink, "L", false, "")
	fs.BoolVar(&o.followLink, "follow-link", false, "")
	fs.Func("q", "", func(v string) error { quietSet, o.quiet = true, v != "false"; return nil })
	fs.Func("quiet", "", func(v string) error { quietSet, o.quiet = true, v != "false"; return nil })
	if err := fs.Parse(boolFlags(args, "q", "quiet")); err != nil || fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	o.source, o.destination = fs.Arg(0), fs.Arg(1)
	switch {
	case o.source == "":
		errorf("source can not be empty")
		return 1
	case o.destination == "":
		errorf("destination can not be empty")
		return 1
	}
	if !quietSet {
		// As docker cp: nothing to say unless a console reads it.
		f, ok := stdout.(*os.File)
		o.quiet = !ok || !vmm.IsTerminal(f)
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	root, _ := storeRoot()
	ctx, stop := interruptible()
	defer stop()
	if err := runCopy(ctx, store, root, o, stdin, stdout, stderr); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}

// boolFlags lets the bool-like flags names, given as Func flags so that a
// value is optional, stand alone, as -q does, by giving them "true".
func boolFlags(args []string, names ...string) []string {
	var out []string
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		name := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && !strings.Contains(name, "=") {
			for _, n := range names {
				if name == n {
					a += "=true"
				}
			}
		}
		out = append(out, a)
	}
	return out
}

func runCopy(ctx context.Context, store *container.Store, root string, o cpOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	srcContainer, srcPath := splitCpArg(o.source)
	dstContainer, dstPath := splitCpArg(o.destination)
	switch {
	case srcContainer != "" && dstContainer != "":
		return errors.New("copying between containers is not supported")
	case srcContainer == "" && dstContainer == "":
		return errors.New("must specify at least one container source")
	}
	ref := srcContainer + dstContainer
	rc, err := openRoot(ctx, store, root, ref)
	if err != nil {
		return err
	}
	defer rc.close()
	if srcContainer != "" {
		return copyFromContainer(ctx, rc, o, srcPath, dstPath, stdout, stderr)
	}
	return copyToContainer(ctx, rc, o, srcPath, dstPath, stdin, stderr)
}

func resolveLocalPath(localPath string) (string, error) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return "", err
	}
	return archive.PreserveTrailingDotOrSeparator(abs, localPath), nil
}

// countingReader counts what passes, for the summary.
type countingReader struct {
	io.ReadCloser
	n *int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	atomic.AddInt64(r.n, int64(n))
	return n, err
}

func copyFromContainer(ctx context.Context, rc *rootClient, o cpOptions, srcPath, dstPath string, stdout, stderr io.Writer) (err error) {
	if dstPath != "-" {
		if dstPath, err = resolveLocalPath(dstPath); err != nil {
			return err
		}
	}
	if err := validateOutputPath(dstPath); err != nil {
		return err
	}
	// With -L, a symlink at the source is followed, and what it leads to
	// is copied under the symlink's name.
	var rebaseName string
	if o.followLink {
		if st, err := rc.stat(ctx, srcPath); err == nil && st.Mode&os.ModeSymlink != 0 {
			linkTarget := joinLink(srcPath, st.LinkTarget)
			linkTarget, rebaseName = archive.GetRebaseName(srcPath, linkTarget)
			srcPath = linkTarget
		}
	}
	content, st, err := rc.get(ctx, srcPath)
	if err != nil {
		return err
	}
	defer content.Close()
	if dstPath == "-" {
		_, err = io.Copy(stdout, content)
		return err
	}
	srcInfo := archive.CopyInfo{Path: srcPath, Exists: true, IsDir: st.Mode.IsDir(), RebaseName: rebaseName}
	var copied int64
	var counted io.ReadCloser = &countingReader{content, &copied}
	pre := counted
	if srcInfo.RebaseName != "" {
		_, srcBase := archive.SplitPathDirEntry(srcInfo.Path)
		pre = archive.RebaseArchiveEntries(counted, srcBase, srcInfo.RebaseName)
	}
	if err := archive.CopyTo(pre, srcInfo, dstPath); err != nil {
		return err
	}
	if !o.quiet {
		reported := copied
		if !st.Mode.IsDir() {
			reported = st.Size
		}
		fmt.Fprint(stderr, copySummary(reported, copied, dstPath))
	}
	return nil
}

// copyToContainer stats the destination in the container to know what the
// archive sent should look like, as docker cp does.
func copyToContainer(ctx context.Context, rc *rootClient, o cpOptions, srcPath, dstPath string, stdin io.Reader, stderr io.Writer) error {
	if srcPath != "-" {
		p, err := resolveLocalPath(srcPath)
		if err != nil {
			return err
		}
		srcPath = p
	}
	dstInfo := archive.CopyInfo{Path: dstPath}
	if st, err := rc.stat(ctx, dstPath); err == nil {
		if st.Mode&os.ModeSymlink != 0 {
			linkTarget := joinLink(dstPath, st.LinkTarget)
			dstInfo.Path = linkTarget
			st, err = rc.stat(ctx, linkTarget)
		}
		if err == nil {
			if err := validateOutputPathFileMode(st.Mode); err != nil {
				return fmt.Errorf(`destination "%s:%s" must be a directory or a regular file: %w`, rc.ref, dstPath, err)
			}
			dstInfo.Exists, dstInfo.IsDir = true, st.Mode.IsDir()
		}
		// Other errors leave it to the extraction: the parent may exist.
	}

	var (
		content      io.Reader
		resolvedDst  string
		copied, size int64
		sizeErr      error
	)
	if srcPath == "-" {
		content = stdin
		resolvedDst = dstInfo.Path
		sizeErr = errors.New("content size not available for stdin")
		if !dstInfo.IsDir {
			return fmt.Errorf(`destination "%s:%s" must be a directory`, rc.ref, dstPath)
		}
	} else {
		srcInfo, err := archive.CopyInfoSourcePath(srcPath, o.followLink)
		if err != nil {
			return err
		}
		size, sizeErr = localContentSize(srcInfo.Path)
		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			return err
		}
		defer srcArchive.Close()
		dstDir, prepared, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, dstInfo)
		if err != nil {
			return err
		}
		defer prepared.Close()
		resolvedDst = dstDir
		content = &countingReader{prepared, &copied}
	}
	if err := rc.put(ctx, resolvedDst, content, true, o.copyUIDGID); err != nil {
		return err
	}
	if !o.quiet {
		reported := copied
		if sizeErr == nil {
			reported = size
		}
		fmt.Fprint(stderr, copySummary(reported, copied, rc.ref+":"+filepath.ToSlash(dstInfo.Path)))
	}
	return nil
}

// localContentSize is the size of the regular files at path.
func localContentSize(path string) (int64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return -1, err
	}
	if !fi.IsDir() {
		if fi.Mode().IsRegular() {
			return fi.Size(), nil
		}
		return 0, nil
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func copySummary(contentSize, transferredSize int64, dest string) string {
	if contentSize != transferredSize {
		return fmt.Sprintf("Successfully copied %s (transferred %s) to %s\n", decimalSize(uint64(contentSize)), decimalSize(uint64(transferredSize)), dest)
	}
	return fmt.Sprintf("Successfully copied %s to %s\n", decimalSize(uint64(contentSize)), dest)
}

func validateOutputPath(path string) error {
	dir := filepath.Dir(filepath.Clean(path))
	if dir != "" && dir != "." {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("invalid output path: directory %q does not exist", dir)
		}
	}
	if fi, err := os.Stat(path); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		if fi.Mode().IsDir() || fi.Mode().IsRegular() {
			return nil
		}
		if err := validateOutputPathFileMode(fi.Mode()); err != nil {
			return fmt.Errorf("invalid output path: %q must be a directory or a regular file: %w", path, err)
		}
	}
	return nil
}

func validateOutputPathFileMode(mode os.FileMode) error {
	switch {
	case mode&os.ModeDevice != 0:
		return errors.New("got a device")
	case mode&os.ModeIrregular != 0:
		return errors.New("got an irregular file")
	}
	return nil
}

// splitCpArg splits CONTAINER:PATH. A local path with a colon must be
// written as one, as ./a:b, and C:\x is a Windows path.
func splitCpArg(arg string) (ctr, path string) {
	if isAbs(arg) {
		return "", arg
	}
	ctr, path, ok := strings.Cut(arg, ":")
	if !ok || strings.HasPrefix(ctr, ".") {
		return "", arg
	}
	return ctr, path
}

func isAbs(path string) bool {
	return filepath.IsAbs(path) || strings.HasPrefix(path, string(os.PathSeparator))
}

// joinLink is where a symlink at the container's path p leads, given its
// target: a path in the container, which Windows' path functions would
// not take for absolute.
func joinLink(p, target string) string {
	if strings.HasPrefix(target, "/") {
		return target
	}
	parent, _ := archive.SplitPathDirEntry(p)
	return path.Join(slashPath(parent), target)
}
