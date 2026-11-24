//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/passwd"
)

// rootServer serves the container's files on guestspec.RootPort, for
// runwhp cp, export, diff and commit. It runs in init, chrooted in the
// container's root once enterRoot has run, so paths resolve as the
// container's do, its mounts included. Nothing in the container can reach
// it: the kernel has no vsock loopback.
type rootServer struct {
	spec *guestspec.Spec
	user passwd.User
	// anon are the anonymous volumes' paths below the root, whose
	// contents are not the container's changes, as Docker's anonymous
	// volumes are not.
	anon []string
}

// startRootServer listens on RootPort, or logs why it cannot.
func startRootServer(spec *guestspec.Spec, user passwd.User) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guestspec.RootPort})
		if err == nil {
			err = unix.Listen(fd, 16)
		}
		if err != nil {
			unix.Close(fd)
		}
	}
	if err != nil {
		logf("runwhp cp, export, diff and commit are not available: vsock: %v", err)
		return
	}
	s := &rootServer{spec: spec, user: user}
	for _, m := range spec.Mounts {
		if m.Kind != guestspec.MountAnon || m.NoCopy {
			continue
		}
		// Only these keep their files in the writable layer.
		if p, err := resolveIn(m.Target); err == nil {
			s.anon = append(s.anon, strings.TrimPrefix(path.Clean(p), "/"))
		}
	}
	go func() {
		for {
			nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			if err != nil {
				logf("root server: accept: %v", err)
				return
			}
			go func() {
				conn := os.NewFile(uintptr(nfd), "vsock")
				defer conn.Close()
				if err := s.serve(conn); err != nil {
					debugf("root server: %v", err)
				}
			}()
		}
	}()
}

// serve answers one request read from conn.
func (s *rootServer) serve(conn io.ReadWriter) error {
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	var req guestspec.RootRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return s.reply(conn, guestspec.RootReply{Error: "bad request: " + err.Error()})
	}
	debugf("root server: %s %q", req.Op, req.Path)
	switch req.Op {
	case guestspec.RootStat:
		st, rep := s.stat(req.Path)
		rep.Stat = st
		return s.reply(conn, rep)
	case guestspec.RootGet:
		return s.get(conn, req.Path)
	case guestspec.RootPut:
		return s.put(conn, r, req)
	case guestspec.RootExport:
		return s.export(conn)
	case guestspec.RootChanges:
		if layerRoots.upper == nil {
			return s.reply(conn, guestspec.RootReply{Error: "init could not open the container's layers"})
		}
		cs, err := changes(layerRoots.upper, layerRoots.lowers, s.anon)
		if err != nil {
			return s.reply(conn, guestspec.RootReply{Error: err.Error()})
		}
		return s.reply(conn, guestspec.RootReply{Changes: cs})
	case guestspec.RootDiff:
		if layerRoots.upper == nil {
			return s.reply(conn, guestspec.RootReply{Error: "init could not open the container's layers"})
		}
		if err := s.reply(conn, guestspec.RootReply{}); err != nil {
			return err
		}
		return writeTar(conn, layerRoots.upper, tarOptions{whiteouts: true, excludeBelow: s.anon})
	}
	return s.reply(conn, guestspec.RootReply{Error: fmt.Sprintf("init does not know the request %q", req.Op)})
}

func (s *rootServer) reply(w io.Writer, rep guestspec.RootReply) error {
	line, _ := json.Marshal(rep)
	_, err := w.Write(append(line, '\n'))
	return err
}

// resolvePath is Docker's container ResolvePath: p, absolute, its
// trailing "/" or "/." kept, with the symlinks of its directory part
// followed in the container, and its last element left as it is.
func resolvePath(p string) (resolved, abs string, err error) {
	abs = preserveTrailingDotOrSeparator(path.Clean("/"+p), p)
	dir, base := path.Split(abs)
	rdir, err := resolveIn(dir)
	if err != nil {
		return "", "", err
	}
	if rdir == "/" {
		return "/" + base, abs, nil
	}
	return rdir + "/" + base, abs, nil
}

// preserveTrailingDotOrSeparator is go-archive's.
func preserveTrailingDotOrSeparator(cleaned, original string) string {
	if path.Base(cleaned) != "." && path.Base(original) == "." {
		if !strings.HasSuffix(cleaned, "/") {
			cleaned += "/"
		}
		cleaned += "."
	}
	if !strings.HasSuffix(cleaned, "/") && strings.HasSuffix(original, "/") {
		cleaned += "/"
	}
	return cleaned
}

// stat is Docker's StatPath.
func (s *rootServer) stat(p string) (*guestspec.PathStat, guestspec.RootReply) {
	resolved, abs, err := resolvePath(p)
	if err != nil {
		return nil, guestspec.RootReply{Error: err.Error()}
	}
	fi, err := os.Lstat(resolved)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, guestspec.RootReply{Error: err.Error(), NotFound: true}
	}
	if err != nil {
		return nil, guestspec.RootReply{Error: err.Error()}
	}
	st := &guestspec.PathStat{Name: path.Base(abs), Size: fi.Size(), Mode: fi.Mode(), Mtime: fi.ModTime()}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := resolveIn(abs)
		if err != nil {
			return nil, guestspec.RootReply{Error: err.Error()}
		}
		st.LinkTarget = path.Join("/", target)
	}
	return st, guestspec.RootReply{}
}

// get sends a tar of p, as Docker's ContainerArchivePath does.
func (s *rootServer) get(conn io.Writer, p string) error {
	st, rep := s.stat(p)
	if st == nil {
		return s.reply(conn, rep)
	}
	resolved, abs, _ := resolvePath(p)
	dir, base := splitPathDirEntry(resolved)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	}
	defer root.Close()
	if err := s.reply(conn, guestspec.RootReply{Stat: st}); err != nil {
		return err
	}
	rebase := path.Base(abs)
	return writeTar(conn, root, tarOptions{include: []string{base}, includeSourceDir: true, rebase: map[string]string{base: rebase}})
}

// put extracts the tar that follows into the directory p, as Docker's
// ContainerExtractToDir does.
func (s *rootServer) put(conn io.Writer, r io.Reader, req guestspec.RootRequest) error {
	resolved, _, err := resolvePath(req.Path)
	if err != nil {
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	}
	fi, err := os.Lstat(resolved)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s.reply(conn, guestspec.RootReply{Error: err.Error(), NotFound: true})
	case err != nil:
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	case !fi.IsDir():
		return s.reply(conn, guestspec.RootReply{Error: "extraction point is not a directory"})
	}
	var sfs unix.Statfs_t
	if unix.Statfs(resolved, &sfs) == nil && sfs.Flags&unix.ST_RDONLY != 0 {
		return s.reply(conn, guestspec.RootReply{Error: "mounted volume is marked read-only"})
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	}
	defer root.Close()
	if err := s.reply(conn, guestspec.RootReply{}); err != nil {
		return err
	}
	opts := untarOptions{noOverwriteDirNonDir: req.NoOverwriteDirNonDir}
	if req.CopyUIDGID && s.spec.User != "" {
		// docker cp -a: the container's user owns what is copied in.
		opts.chown = &[2]int{int(s.user.UID), int(s.user.GID)}
	}
	// The tar's end marks the end of the request: the host cannot end its
	// side of the connection alone through its pipe.
	if err := untar(r, root, opts); err != nil {
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	}
	return s.reply(conn, guestspec.RootReply{})
}

// export sends a tar of the container's root file system without what is
// mounted in it, as docker export does: a copy of the root's mount alone,
// with nothing mounted below it, shows the files the mounts cover.
func (s *rootServer) export(conn io.Writer) error {
	fd, err := unix.OpenTree(unix.AT_FDCWD, "/", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return s.reply(conn, guestspec.RootReply{Error: fmt.Sprintf("cloning the root's mount: %v", err)})
	}
	tree := os.NewFile(uintptr(fd), "root")
	defer tree.Close()
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return s.reply(conn, guestspec.RootReply{Error: err.Error()})
	}
	defer root.Close()
	if err := s.reply(conn, guestspec.RootReply{}); err != nil {
		return err
	}
	return writeTar(conn, root, tarOptions{})
}
