//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/vmm"
)

// The Engine API's BuildKit: buildx's docker driver, which Docker Compose
// and Dev Containers build through, talks to Docker's embedded BuildKit
// over /grpc and /session. runwhp's is BuildKit's own daemon,
// runwhp-buildkitd, in a privileged container of the server's: the
// builder, started at the first build, kept running until the server
// stops, its cache on a volume of its own.
const (
	builderName   = "runwhp_buildkit"
	builderVolume = "runwhp_buildkit"
	// builderImage gives runwhp-buildkitd what BuildKit's OCI worker runs
	// steps with: runc, git, the qemu emulators. RUNWHP_BUILDKIT_IMAGE
	// names another, as the end-to-end tests name a mirror's copy.
	builderImage = "docker.io/moby/buildkit:v0.35.0@sha256:a521fc1fd903a46c611c733dd692548512232fba2e0386d6055846a903d05a71"
	// builderVersionLabel says what made the builder: one made by another
	// runwhp-buildkitd, or from another image, is made again.
	builderVersionLabel = "io.runwhp.builder.version"
	builderDaemonPath   = "/usr/local/bin/runwhp-buildkitd"
	// builderReady bounds how long the builder's daemon may take to start.
	builderReady = 60 * time.Second
)

type builderState struct {
	version     sync.Once
	versionHash string
	versionErr  error
	flight      singleflight.Group
}

// builderDaemon is runwhp-buildkitd, beside runwhp.exe.
func builderDaemon() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(exe), "runwhp-buildkitd")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("BuildKit's daemon for runwhp, %s, is missing: %w", p, err)
	}
	return p, nil
}

func builderImageRef() string {
	if ref := os.Getenv("RUNWHP_BUILDKIT_IMAGE"); ref != "" {
		return ref
	}
	return builderImage
}

// builderVersion is what the builder's version label says, for this
// server: its daemon's and its image's.
func (s *apiServer) builderVersion() (string, error) {
	b := &s.builder
	b.version.Do(func() {
		p, err := builderDaemon()
		if err != nil {
			b.versionErr = err
			return
		}
		f, err := os.Open(p)
		if err != nil {
			b.versionErr = err
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			b.versionErr = err
			return
		}
		io.WriteString(h, "\x00"+builderImageRef())
		b.versionHash = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return b.versionHash, b.versionErr
}

// isBuilder reports whether c is runwhp's builder, which the API lists
// with no other containers, as Docker's embedded BuildKit is no container.
func isBuilder(c *container.Container) bool {
	_, ok := c.Config.Labels[guestspec.BuilderLabel]
	return ok
}

// ensureBuilder returns once the builder runs and its daemon answers,
// starting it if it has to. The start goes on if ctx ends meanwhile, for
// the next build: pulling the builder's image the first time can outlast
// the client's patience.
func (s *apiServer) ensureBuilder(ctx context.Context) error {
	if s.builderUp(ctx) {
		return nil
	}
	ch := s.builder.flight.DoChan("builder", func() (any, error) {
		err := s.startBuilder(s.ctx)
		if err != nil {
			s.logf("the builder: %v", err)
		}
		return nil, err
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// builderUp reports whether the builder runs, as this server would make
// it, and its daemon answers.
func (s *apiServer) builderUp(ctx context.Context) bool {
	c, err := s.containers.Resolve(builderName)
	if err != nil || c.State.Status != container.Running || !s.builderCurrent(c) {
		return false
	}
	return s.pingBuilder(ctx) == nil
}

// builderCurrent reports whether c is the builder this server would make:
// by its runwhp-buildkitd, from its image, reaching it through Docker's
// socket.
func (s *apiServer) builderCurrent(c *container.Container) bool {
	want, err := s.builderVersion()
	if err != nil || c.Config.Labels[builderVersionLabel] != want {
		return false
	}
	for _, m := range c.Config.Mounts {
		if m.Kind == guestspec.MountSocket && m.Target == "/var/run/docker.sock" {
			return strings.EqualFold(m.Pipe, s.pipe)
		}
	}
	return false
}

func (s *apiServer) startBuilder(ctx context.Context) error {
	if _, err := s.builderVersion(); err != nil {
		return err
	}
	c, err := s.containers.Resolve(builderName)
	switch {
	case errors.Is(err, container.ErrNotFound):
		c = nil
	case err != nil:
		return err
	case !s.builderCurrent(c):
		// Made by another runwhp-buildkitd, or for another server: made
		// again, on the same volume, so the cache stays.
		s.logf("making the builder again: it was made by another runwhp or for another server")
		if err := deleteContainer(s.containers, c.ID, true); err != nil && !errors.Is(err, container.ErrNotFound) {
			return fmt.Errorf("removing the old builder: %w", err)
		}
		c = nil
	}
	if c == nil {
		if c, err = s.createBuilder(ctx); err != nil {
			return fmt.Errorf("making the builder: %w", err)
		}
	}
	if c.State.Status != container.Running {
		s.logf("starting the builder")
		if err := startContainerIO(ctx, s.containers, c.ID, false); err != nil {
			return fmt.Errorf("starting the builder: %w", err)
		}
	}
	deadline := time.Now().Add(builderReady)
	for {
		err := s.pingBuilder(ctx)
		if err == nil {
			s.logf("the builder is ready")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the builder did not become ready in %v: %w (see `runwhp logs %s`)", builderReady, err, builderName)
		}
		if c, rerr := s.containers.Resolve(builderName); rerr == nil && c.State.Status == container.Stopped {
			return fmt.Errorf("the builder stopped as it started, with %d (see `runwhp logs %s`)", c.State.ExitCode, builderName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *apiServer) createBuilder(ctx context.Context) (*container.Container, error) {
	version, _ := s.builderVersion()
	daemon, err := builderDaemon()
	if err != nil {
		return nil, err
	}
	var mounts []mountSpec
	for _, m := range []string{
		// overlayfs, which BuildKit's snapshots need, takes no overlay as
		// its upper: the cache goes on a disk of its own. It is sparse, so
		// its size says how much the cache may grow to, not what it takes.
		"type=volume,source=" + builderVolume + ",target=/var/lib/buildkit,volume-opt=size=256g",
		"type=bind,source=" + daemon + ",target=" + builderDaemonPath + ",readonly",
	} {
		ms, err := parseMount(m, "")
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, ms)
	}
	sock, err := parseVolume("/var/run/docker.sock:/var/run/docker.sock", "")
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, sock)
	gwLine, _, err := netstack.HostsLine("host.docker.internal:host-gateway")
	if err != nil {
		return nil, err
	}
	maxUsed, reserved := builderGCCaps(s.root)
	logs, err := parseLogOptions("", nil)
	if err != nil {
		return nil, err
	}
	spec := runSpec{
		memory:        vmm.DefaultMemory(),
		storageSize:   defaultDiskSize,
		logs:          logs,
		image:         builderImageRef(),
		entrypointSet: true,
		entrypoint:    []string{builderDaemonPath},
		command: []string{
			"--host-gateway", netstack.Gateway.String(),
			"--gc-max-used", fmt.Sprint(maxUsed), "--gc-reserved", fmt.Sprint(reserved),
		},
		privileged: true,
		cpus:       min(runtime.NumCPU(), 64),
		mounts:     mounts,
		apiPipe:    s.pipe,
		network:    networkRequest{hosts: []string{gwLine}, hostGateway: true},
		labels:     map[string]string{guestspec.BuilderLabel: "buildkit", builderVersionLabel: version},
		hostname:   "buildkitsandbox",
	}
	s.logf("making the builder from %s", spec.image)
	progress := &logLines{s: s, prefix: "builder: "}
	c, _, err := createContainer(ctx, s.root, &spec, createOptions{name: builderName, policy: image.PullMissing, progress: progress})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// builderGCCaps are the builder's cache's limits, from the room on the
// store's disk now, since the volume's own size, sparse, says nothing of
// it: at most half of what is free, up to 100 GB, keeping a quarter of
// that however full the disk.
func builderGCCaps(root string) (maxUsed, reserved int64) {
	maxUsed = 20 << 30
	var avail, total, free uint64
	if p, err := windows.UTF16PtrFromString(root); err == nil {
		if windows.GetDiskFreeSpaceEx(p, &avail, &total, &free) == nil {
			maxUsed = int64(avail / 2)
		}
	}
	maxUsed = min(max(maxUsed, 2<<30), 100e9)
	return maxUsed, maxUsed / 4
}

// logLines writes what it is given to the server's log, a line at a time.
type logLines struct {
	s      *apiServer
	prefix string
	buf    []byte
}

func (l *logLines) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexAny(string(l.buf), "\r\n")
		if i < 0 {
			return len(p), nil
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			l.s.logf("%s%s", l.prefix, line)
		}
		l.buf = l.buf[i+1:]
	}
}

// stopBuilder stops the builder, as the server stops.
func (s *apiServer) stopBuilder() {
	c, err := s.containers.Resolve(builderName)
	if err != nil || c.State.Status != container.Running || !s.builderCurrent(c) {
		return
	}
	s.logf("stopping the builder")
	if err := stopContainer(s.containers, c.ID, 10*time.Second); err != nil {
		s.logf("stopping the builder: %v", err)
	}
}

// builderConn is a connection to the builder's daemon, through the runwhp
// running it: what the pipe's reply left buffered is read first.
type builderConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *builderConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialBuilder connects to the builder's daemon, saying what for.
func (s *apiServer) dialBuilder(ctx context.Context, hello guestspec.BuildKitHello) (*builderConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := s.containers.Resolve(builderName)
	if err != nil {
		return nil, err
	}
	conn, err := container.Dial(ctx, s.root, c.ID)
	if err != nil {
		return nil, err
	}
	rep, r, err := container.Exchange(conn, container.Request{Op: container.OpConnect, Port: guestspec.BuildKitPort})
	if err == nil && !rep.OK {
		err = errors.New(rep.Error)
	}
	if err == nil {
		var line []byte
		if line, err = json.Marshal(hello); err == nil {
			_, err = conn.Write(append(line, '\n'))
		}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &builderConn{conn, r}, nil
}

func (s *apiServer) pingBuilder(ctx context.Context) error {
	c, err := s.dialBuilder(ctx, guestspec.BuildKitHello{Kind: guestspec.BuildKitPing})
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		return err
	}
	if line != "ok\n" {
		return fmt.Errorf("the builder answered %q", line)
	}
	return nil
}

// postGRPC serves /grpc, BuildKit's API, as Docker's daemon does for
// buildx's docker driver: the client's HTTP/2, carried to the builder.
func (s *apiServer) postGRPC(w http.ResponseWriter, r *http.Request) error {
	return s.carryToBuilder(w, r, guestspec.BuildKitHello{Kind: guestspec.BuildKitGRPC})
}

// postSession serves /session, a client's session, which BuildKit calls
// back into for the build's context, secrets, credentials and outputs.
func (s *apiServer) postSession(w http.ResponseWriter, r *http.Request) error {
	return s.carryToBuilder(w, r, guestspec.BuildKitHello{Kind: guestspec.BuildKitSession, Headers: sessionHeaders(r.Header)})
}

// sessionHeaders are the headers of a /session request that describe the
// session to BuildKit, as dockerd passes them on.
func sessionHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-docker-expose-session-") || strings.HasPrefix(lk, "x-buildkit-session-") {
			out[k] = v
		}
	}
	return out
}

func (s *apiServer) carryToBuilder(w http.ResponseWriter, r *http.Request, hello guestspec.BuildKitHello) error {
	proto := r.Header.Get("Upgrade")
	if !strings.EqualFold(proto, "h2c") {
		return badRequest("%s takes an upgrade to h2c, not %q", r.URL.Path, proto)
	}
	// A missing daemon file is the one failure worth an HTTP answer.
	if _, err := s.builderVersion(); err != nil {
		return fmt.Errorf("BuildKit is not available: %w", err)
	}
	// The upgrade is answered at once, the builder started behind it if
	// it has to be: buildx takes the docker driver only if a probe of
	// /grpc is answered quickly, and gives up on the first build's gRPC
	// handshake only after 20 s, retrying.
	conn, brw, err := upgrade(w, proto)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	err = s.ensureBuilder(ctx)
	var bc *builderConn
	if err == nil {
		bc, err = s.dialBuilder(ctx, hello)
	}
	cancel()
	if err != nil {
		s.logf("%s: BuildKit is not available: %v", r.URL.Path, err)
		conn.Close()
		return nil
	}
	spliceConns(s.ctx, conn, brw.Reader, bc)
	return nil
}

// upgrade takes the connection over, answering 101 for proto, as dockerd
// answers /grpc and /session.
func upgrade(w http.ResponseWriter, proto string) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("this connection cannot be taken over for a stream")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	conn.SetDeadline(time.Time{})
	head := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + proto + "\r\n\r\n"
	if _, err := brw.WriteString(head); err == nil {
		err = brw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw, nil
}

// spliceConns carries bytes both ways between a client's hijacked
// connection, whose first bytes may wait in buffered, and the builder,
// until either ends or ctx does; then it closes both.
func spliceConns(ctx context.Context, client net.Conn, buffered *bufio.Reader, builder io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(builder, io.MultiReader(io.LimitReader(buffered, int64(buffered.Buffered())), client))
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, builder)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	client.Close()
	builder.Close()
}
