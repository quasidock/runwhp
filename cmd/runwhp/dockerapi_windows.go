//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/volume"
)

// runwhpVersion is runwhp's own version, which the Engine API reports as
// its engine's.
const runwhpVersion = "0.20.0"

// versionString is runwhpVersion with the commit it was built from, if
// the build recorded one.
func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return runwhpVersion + "+" + s.Value[:7]
			}
		}
	}
	return runwhpVersion
}

func gitCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return s.Value[:7]
			}
		}
	}
	return ""
}

const dockerAPIUsage = `Usage: runwhp docker-api start|stop|status

Serve Docker's Engine API on a named pipe of runwhp's own, so that the
docker CLI, and other clients of Docker's API, run containers through
runwhp. It runs in the background, as a container does, until stopped, and
containers outlive it.

  start [--pipe NAME] [--docker-engine] [--debug]
                                  start serving, unless it already is, and
                                  write a docker context for it, named
                                  runwhp; with --docker-engine, serve
                                  Docker's own pipe, docker_engine, too,
                                  which tools that take no context or
                                  DOCKER_HOST look for, unless Docker
                                  Desktop or another engine serves it
  stop                            stop serving; containers keep running
  status                          say whether it is serving, and where

The pipe is \\.\pipe\runwhp_engine for the default store, and has the
store's hash in its name for another RUNWHP_ROOT, as the context does. Use
it with

  docker --context runwhp run --rm alpine echo hello

or 'docker context use runwhp', or DOCKER_HOST=npipe:////./pipe/runwhp_engine.
Its log is in the store, in docker-api\server.log.
`

func cmdDockerAPI(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, dockerAPIUsage)
		return 2
	}
	switch args[0] {
	case "start":
		return dockerAPIStart(args[1:])
	case "stop":
		return dockerAPIStop(args[1:])
	case "status":
		return dockerAPIStatus(args[1:])
	case "serve":
		return dockerAPIServe(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Print(dockerAPIUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown docker-api command %q\n\n%s", args[0], dockerAPIUsage)
	return 2
}

// defaultStoreRoot is where the store is when RUNWHP_ROOT is not set.
func defaultStoreRoot() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cache, "runwhp")
}

// storeSuffix is "" for the default store, and "-" and a hash of its path
// for any other, as containers' pipes hash it.
func storeSuffix(root string) string {
	clean := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		return strings.ToLower(filepath.Clean(abs))
	}
	if def := defaultStoreRoot(); def != "" && clean(def) == clean(root) {
		return ""
	}
	sum := sha256.Sum256([]byte(clean(root)))
	return "-" + hex.EncodeToString(sum[:4])
}

// apiPipe is the pipe the API serves for the store at root, unless told.
func apiPipe(root string) string { return `\\.\pipe\runwhp_engine` + storeSuffix(root) }

// apiContextName is the docker context written for the store at root.
func apiContextName(root string) string { return "runwhp" + storeSuffix(root) }

// pipePath is the full path of the pipe name names: a bare name, or a
// path, \\.\pipe\NAME or //./pipe/NAME, however many of its leading
// slashes a shell left.
func pipePath(name string) string {
	n := strings.TrimLeft(strings.ReplaceAll(name, "/", `\`), `\`)
	if len(n) >= len(`.\pipe\`) && strings.EqualFold(n[:len(`.\pipe\`)], `.\pipe\`) {
		n = n[len(`.\pipe\`):]
	}
	return `\\.\pipe\` + n
}

// dockerHost is the DOCKER_HOST value that names pipe.
func dockerHost(pipe string) string { return "npipe://" + strings.ReplaceAll(pipe, `\`, "/") }

func apiDir(root string) string { return filepath.Join(root, "docker-api") }

// dockerEnginePipe is Docker's own pipe, which docker-api start
// --docker-engine serves too.
const dockerEnginePipe = `\\.\pipe\docker_engine`

// apiStatus is what a serving runwhp says of itself.
type apiStatus struct {
	Pid  int    `json:"pid"`
	Pipe string `json:"pipe"`
	// Also are the pipes it serves besides Pipe: docker_engine, with
	// --docker-engine.
	Also    []string  `json:"also,omitempty"`
	Root    string    `json:"root"`
	Started time.Time `json:"started"`
	Version string    `json:"version"`
	Log     string    `json:"log"`
}

// apiClient is an HTTP client of the API at pipe.
func apiClient(pipe string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, pipe)
		},
	}}
}

// askStatus asks the API at pipe how it stands; it fails if nothing serves
// it.
func askStatus(pipe string) (*apiStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://runwhp/runwhp/status", nil)
	resp, err := apiClient(pipe).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answers %s: something other than runwhp's API serves it", pipe, resp.Status)
	}
	var st apiStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func pipeServed(pipe string) bool {
	timeout := 200 * time.Millisecond
	c, err := winio.DialPipe(pipe, &timeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func dockerAPIStart(args []string) int {
	fs := newFlags("docker-api start", dockerAPIUsage)
	var pipe string
	var debugLog, dockerEngine bool
	fs.StringVar(&pipe, "pipe", "", "")
	fs.BoolVar(&debugLog, "debug", false, "")
	fs.BoolVar(&dockerEngine, "docker-engine", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if pipe == "" {
		pipe = apiPipe(root)
	} else {
		pipe = pipePath(pipe)
	}
	if pipeServed(pipe) {
		st, err := askStatus(pipe)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		if dockerEngine && !slices.Contains(st.Also, dockerEnginePipe) {
			errorf("runwhp's Docker Engine API already serves %s (runwhp process %d), without --docker-engine; "+
				"stop it with 'runwhp docker-api stop' first", st.Pipe, st.Pid)
			return 1
		}
		fmt.Printf("runwhp's Docker Engine API already serves %s (runwhp process %d).\n", pipeList(st.Pipe, st.Also), st.Pid)
		return writeContextAndHint(root, pipe, st.Also)
	}
	if dockerEngine && pipeServed(dockerEnginePipe) {
		errorf("%s is already served, by Docker Desktop or another engine; start without --docker-engine, or stop it first", dockerEnginePipe)
		return 1
	}
	if err := os.MkdirAll(apiDir(root), 0o755); err != nil {
		errorf("%v", err)
		return 1
	}
	extra := []string{"--pipe", pipe}
	var also []string
	if dockerEngine {
		extra = append(extra, "--docker-engine")
		also = []string{dockerEnginePipe}
	}
	if debugLog {
		extra = append(extra, "--debug")
	}
	if err := spawnAPIServer(root, extra); err != nil {
		errorf("%v", err)
		return 1
	}
	fmt.Printf("runwhp's Docker Engine API serves %s, in the background, until 'runwhp docker-api stop'.\nIts log is %s.\n",
		pipeList(pipe, also), filepath.Join(apiDir(root), "server.log"))
	return writeContextAndHint(root, pipe, also)
}

// pipeList names a server's pipes, for messages.
func pipeList(pipe string, also []string) string {
	return strings.Join(append([]string{pipe}, also...), " and ")
}

func writeContextAndHint(root, pipe string, also []string) int {
	if slices.Contains(also, dockerEnginePipe) {
		fmt.Printf("\nThe docker CLI, Docker Compose, Testcontainers and Docker's SDKs find it on %s with no\ncontext or DOCKER_HOST, as they find Docker Desktop.\n", dockerEnginePipe)
	}
	name := apiContextName(root)
	if pipe != apiPipe(root) {
		fmt.Printf("\nUse it with DOCKER_HOST=%s.\n", dockerHost(pipe))
		return 0
	}
	if err := writeDockerContext(name, dockerHost(pipe)); err != nil {
		errorf("writing docker context %s: %v", name, err)
		fmt.Printf("\nUse it with DOCKER_HOST=%s.\n", dockerHost(pipe))
		return 0
	}
	fmt.Printf("\nUse it with 'docker --context %s', or 'docker context use %s', or DOCKER_HOST=%s.\n", name, name, dockerHost(pipe))
	return 0
}

// dockerConfigDir is where the docker CLI keeps its configuration.
func dockerConfigDir() (string, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker"), nil
}

// writeDockerContext writes a docker context named name whose endpoint is
// host, as 'docker context create' would, leaving the current context as
// it is.
func writeDockerContext(name, host string) error {
	dir, err := dockerConfigDir()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(name))
	metaDir := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return err
	}
	meta := map[string]any{
		"Name":     name,
		"Metadata": map[string]string{"Description": "runwhp's Docker Engine API"},
		"Endpoints": map[string]any{
			"docker": map[string]any{"Host": host, "SkipTLSVerify": false},
		},
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(metaDir, "meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(metaDir, "meta.json"))
}

// spawnAPIServer starts runwhp docker-api serve in the background, as a
// container's runwhp is started, and waits until it serves.
func spawnAPIServer(root string, extra []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	readR, readW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readR.Close()
	args := append([]string{"docker-api", "serve", "--ready", strconv.FormatUint(uint64(readW.Fd()), 10)}, extra...)
	cmd := exec.Command(exe, args...)
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "RUNWHP_ROOT="+abs)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              detachedProcess | windows.CREATE_NEW_PROCESS_GROUP | breakawayFromJob,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(readW.Fd())},
	}
	err = cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		errorf("warning: this session keeps its processes in a job they cannot leave, so the API will stop serving when the session ends")
		cmd.SysProcAttr.CreationFlags &^= breakawayFromJob
		err = cmd.Start()
	}
	readW.Close()
	if err != nil {
		return fmt.Errorf("starting runwhp to serve the API: %w", err)
	}
	line, _ := bufio.NewReader(readR).ReadBytes('\n')
	var rep struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &rep) == nil {
		if !rep.OK {
			cmd.Wait()
			return errors.New(rep.Error)
		}
		cmd.Process.Release()
		return nil
	}
	werr := cmd.Wait()
	return fmt.Errorf("runwhp's API server ended at once (%v); see %s", werr, filepath.Join(apiDir(root), "server.log"))
}

func dockerAPIStop(args []string) int {
	fs := newFlags("docker-api stop", dockerAPIUsage)
	var pipe string
	fs.StringVar(&pipe, "pipe", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if pipe = pipePath(pipe); pipe == `\\.\pipe\` {
		pipe = apiPipe(root)
	}
	st, err := askStatus(pipe)
	if err != nil {
		fmt.Printf("runwhp's Docker Engine API is not serving %s.\n", pipe)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://runwhp/runwhp/shutdown", nil)
	resp, err := apiClient(pipe).Do(req)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	resp.Body.Close()
	// It has stopped once its process has.
	if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(st.Pid)); err == nil {
		windows.WaitForSingleObject(h, 35_000)
		windows.CloseHandle(h)
	}
	fmt.Printf("runwhp's Docker Engine API no longer serves %s.\n", pipe)
	return 0
}

func dockerAPIStatus(args []string) int {
	fs := newFlags("docker-api status", dockerAPIUsage)
	var pipe string
	fs.StringVar(&pipe, "pipe", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if pipe = pipePath(pipe); pipe == `\\.\pipe\` {
		pipe = apiPipe(root)
	}
	st, err := askStatus(pipe)
	if err != nil {
		fmt.Printf("runwhp's Docker Engine API is not serving %s.\n", pipe)
		return 3
	}
	fmt.Printf("runwhp's Docker Engine API serves %s\n  process  %d\n  since    %s\n  store    %s\n  version  %s\n  log      %s\n",
		pipeList(st.Pipe, st.Also), st.Pid, st.Started.Local().Format(time.RFC3339), st.Root, st.Version, st.Log)
	return 0
}

// dockerAPIServe serves the API in the foreground: what docker-api start
// runs in the background.
func dockerAPIServe(args []string) int {
	fs := flag.NewFlagSet("docker-api serve", flag.ContinueOnError)
	var ready uint64
	var pipe string
	var debugLog, dockerEngine bool
	fs.Uint64Var(&ready, "ready", 0, "")
	fs.StringVar(&pipe, "pipe", "", "")
	fs.BoolVar(&debugLog, "debug", false, "")
	fs.BoolVar(&dockerEngine, "docker-engine", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	var readyFile *os.File
	if ready != 0 {
		readyFile = os.NewFile(uintptr(ready), "ready")
	}
	report := func(err error) {
		if readyFile == nil {
			if err != nil {
				errorf("%v", err)
			}
			return
		}
		rep := map[string]any{"ok": err == nil}
		if err != nil {
			rep["error"] = err.Error()
		}
		data, _ := json.Marshal(rep)
		readyFile.Write(append(data, '\n'))
		readyFile.Close()
		readyFile = nil
	}
	root, err := storeRoot()
	if err != nil {
		report(err)
		return 1
	}
	if pipe == "" {
		pipe = apiPipe(root)
	} else {
		pipe = pipePath(pipe)
	}
	if err := os.MkdirAll(apiDir(root), 0o755); err != nil {
		report(err)
		return 1
	}
	logPath := filepath.Join(apiDir(root), "server.log")
	logFile, err := openServerLog(logPath)
	if err != nil {
		report(err)
		return 1
	}
	defer logFile.Close()
	s, err := newAPIServer(root, pipe, logFile, debugLog)
	if err != nil {
		report(err)
		return 1
	}
	if dockerEngine {
		s.also = []string{dockerEnginePipe}
	}
	if err := s.listen(); err != nil {
		report(err)
		return 1
	}
	s.logf("runwhp %s serving the Engine API %s on %s, process %d, store %s", versionString(), dockerapi.APIVersion,
		pipeList(pipe, s.also), os.Getpid(), root)
	report(nil)
	if ready == 0 {
		fmt.Printf("runwhp's Docker Engine API serves %s until Ctrl+C. Its log is %s.\n", pipe, logPath)
	}
	return s.serve()
}

// openServerLog opens the server's log for appending, moving it aside
// first once it has grown past 10 MB.
func openServerLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > 10<<20 {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// apiServer serves Docker's Engine API for one store.
type apiServer struct {
	root    string
	pipe    string
	also    []string // further pipes: docker_engine
	started time.Time
	debug   bool

	logMu sync.Mutex
	log   io.Writer

	containers *container.Store
	mux        *http.ServeMux
	listeners  []net.Listener // pipe's, then also's
	http       *http.Server

	// ctx ends when the server shuts down, ending hijacked streams.
	ctx      context.Context
	cancel   context.CancelFunc
	shutdown sync.Once
	stopped  chan struct{}

	execs   *execTable
	events  *eventBroker
	jobs    *jobRunner
	builder builderState
}

func newAPIServer(root, pipe string, log io.Writer, debugLog bool) (*apiServer, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &apiServer{
		root: root, pipe: pipe, started: time.Now(), debug: debugLog, log: log,
		containers: container.Open(root),
		ctx:        ctx, cancel: cancel,
		stopped: make(chan struct{}),
	}
	jobs, err := newJobRunner(s)
	if err != nil {
		cancel()
		return nil, err
	}
	s.jobs = jobs
	s.execs = newExecTable()
	s.events = newEventBroker(s)
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

func (s *apiServer) logf(format string, a ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.log, "%s %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), fmt.Sprintf(format, a...))
}

func (s *apiServer) listen() error {
	sddl, err := container.PipeSecurity()
	if err != nil {
		return err
	}
	for _, p := range append([]string{s.pipe}, s.also...) {
		// Message mode, as Docker's daemon serves its pipe: a client that
		// has written all of a stream's input closes its writing end,
		// which only a message-mode pipe carries.
		l, err := winio.ListenPipe(p, &winio.PipeConfig{
			SecurityDescriptor: sddl,
			MessageMode:        true,
			InputBufferSize:    64 << 10,
			OutputBufferSize:   64 << 10,
		})
		if err != nil {
			for _, l := range s.listeners {
				l.Close()
			}
			s.listeners = nil
			// The first instance of a pipe is made with
			// FILE_FLAG_FIRST_PIPE_INSTANCE, which another server's pipe
			// of that name refuses.
			if p == dockerEnginePipe && (errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY)) {
				return fmt.Errorf("%s is already served, by Docker Desktop or another engine; start without --docker-engine, or stop it first", p)
			}
			return fmt.Errorf("serving %s: %w", p, err)
		}
		s.listeners = append(s.listeners, l)
	}
	return nil
}

func (s *apiServer) serve() int {
	go s.events.watch(s.ctx)
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	// Every pipe serves the same; Shutdown closes them all.
	for _, l := range s.listeners[1:] {
		go s.http.Serve(l)
	}
	err := s.http.Serve(s.listeners[0])
	if errors.Is(err, http.ErrServerClosed) {
		<-s.stopped
		s.logf("stopped serving")
		return 0
	}
	s.logf("serving ended: %v", err)
	return 1
}

// stop stops serving: requests under way have 30 s to finish, and
// hijacked streams and execs end at once. Containers keep running.
func (s *apiServer) stop() {
	s.shutdown.Do(func() {
		go func() {
			defer close(s.stopped)
			s.logf("stopping")
			s.cancel()
			s.execs.closeAll()
			s.jobs.close()
			// The builder is the server's, as Docker's BuildKit is its
			// daemon's; its cache stays on its volume.
			s.stopBuilder()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.http.Shutdown(ctx); err != nil {
				s.http.Close()
			}
		}()
	})
}

// ServeHTTP takes the API version off the path, checks it, and routes
// the rest.
func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h := w.Header()
	h.Set("Api-Version", dockerapi.APIVersion)
	h.Set("Docker-Experimental", "false")
	h.Set("Ostype", "linux")
	h.Set("Server", "runwhp/"+versionString()+" (windows)")
	version, rest := dockerapi.SplitVersion(r.URL.Path)
	rw := &statusWriter{ResponseWriter: w}
	defer func() {
		if v := recover(); v != nil {
			s.logf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, v, debug.Stack())
			if !rw.wrote {
				writeError(rw, fmt.Errorf("runwhp failed serving this request: %v", v))
			}
		}
		s.logf("%s %s -> %d (%v)", r.Method, r.URL.RequestURI(), rw.status, time.Since(start).Round(time.Millisecond))
	}()
	if err := dockerapi.CheckVersion(version); err != nil {
		writeError(rw, badRequest("%v", err))
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = rest, ""
	s.mux.ServeHTTP(rw, r2)
}

// statusWriter records the status a handler answered with, for the log.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("this connection cannot be taken over")
	}
	w.status, w.wrote = http.StatusSwitchingProtocols, true
	return hj.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// apiError is an error with the HTTP status the API answers it with.
type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) error {
	return &apiError{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

func conflict(format string, a ...any) error {
	return &apiError{http.StatusConflict, fmt.Sprintf(format, a...)}
}

func notImplemented(format string, a ...any) error {
	return &apiError{http.StatusNotImplemented, fmt.Sprintf(format, a...)}
}

// httpStatus is the status the API answers err with.
func httpStatus(err error) int {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae.code
	case errors.Is(err, container.ErrNotFound), errors.Is(err, image.ErrNoSuchImage), errors.Is(err, image.ErrNotPresent),
		errors.Is(err, network.ErrNotFound), errors.Is(err, volume.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, container.ErrNameInUse), errors.Is(err, container.ErrRunning), errors.Is(err, container.ErrBusy),
		errors.Is(err, container.ErrPaused), errors.Is(err, container.ErrNotRunning), errors.Is(err, container.ErrNotPaused),
		errors.Is(err, network.ErrExists), errors.Is(err, network.ErrActive), errors.Is(err, network.ErrInUse),
		errors.Is(err, network.ErrPredefined),
		errors.Is(err, volume.ErrExists), errors.Is(err, volume.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, container.ErrAmbiguous), errors.Is(err, image.ErrAmbiguous), errors.Is(err, image.ErrNoCommand):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// writeError answers err as Docker's daemon does: its status, and its
// message in JSON.
func writeError(w http.ResponseWriter, err error) {
	writeJSONStatus(w, httpStatus(err), map[string]string{"message": err.Error()})
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(map[string]string{"message": err.Error()})
	}
	w.Write(append(data, '\n'))
}

// handler adapts a handler that may fail to the mux.
type handler func(w http.ResponseWriter, r *http.Request) error

func (s *apiServer) handle(pattern string, h handler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && s.events != nil {
			// What a request changed shows in the events at once, even
			// for a container gone before the watcher would look.
			defer func() { go s.events.look() }()
		}
		if err := h(w, r); err != nil {
			if sw, ok := w.(*statusWriter); ok && sw.wrote {
				s.logf("%s %s failed after answering: %v", r.Method, r.URL.Path, err)
				return
			}
			writeError(w, err)
		}
	})
}

// readJSON reads a request's JSON body into v; an empty body leaves v as
// it is.
func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return badRequest("invalid JSON: %v", err)
	}
	return nil
}

// boolParam reads a query parameter as Docker's daemon reads booleans:
// absent, empty, 0, no, false and none are false.
func boolParam(r *http.Request, name string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	switch v {
	case "", "0", "no", "false", "none":
		return false
	}
	return true
}

func intParam(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badRequest("invalid %s %q", name, v)
	}
	return n, nil
}

// hijack takes the connection over for a raw stream, as attach and exec
// start do: with an Upgrade: tcp request, it answers 101; otherwise 200,
// as older clients expect.
func hijack(w http.ResponseWriter, r *http.Request, contentType string) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("this connection cannot be taken over for a stream")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	head := w.Header()
	var b strings.Builder
	if strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		b.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n")
	} else {
		b.WriteString("HTTP/1.1 200 OK\r\n")
	}
	fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
	for _, k := range []string{"Api-Version", "Docker-Experimental", "Ostype", "Server"} {
		if v := head.Get(k); v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := brw.WriteString(b.String()); err == nil {
		err = brw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw, nil
}

// closeWrite ends what this end writes on a hijacked connection, if it
// can, leaving the client's input to read.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// routes sets up what the API serves.
func (s *apiServer) routes() {
	s.handle("GET /runwhp/status", s.getStatus)
	s.handle("POST /runwhp/shutdown", s.postShutdown)

	s.handle("GET /_ping", s.ping)
	s.handle("HEAD /_ping", s.ping)
	s.handle("GET /version", s.getVersion)
	s.handle("GET /info", s.getInfo)
	s.handle("GET /events", s.getEvents)

	s.containerRoutes()
	s.execRoutes()
	s.imageRoutes()
	s.networkRoutes()
	s.volumeRoutes()
	s.handle("GET /system/df", s.getSystemDF)

	for _, p := range []string{"/swarm", "/swarm/", "/services", "/services/", "/nodes", "/nodes/", "/tasks", "/tasks/",
		"/secrets", "/secrets/", "/configs", "/configs/"} {
		s.handle(p, func(http.ResponseWriter, *http.Request) error {
			return &apiError{http.StatusServiceUnavailable, "This node is not a swarm manager. runwhp has no swarm mode."}
		})
	}
	s.handle("GET /plugins", func(w http.ResponseWriter, _ *http.Request) error {
		writeJSON(w, []any{})
		return nil
	})
	s.handle("/distribution/", s.getDistribution)
	s.handle("POST /grpc", s.postGRPC)
	s.handle("POST /session", s.postSession)
	for _, p := range []string{"/plugins/", "/images/search"} {
		s.handle(p, func(_ http.ResponseWriter, r *http.Request) error {
			return notImplemented("runwhp does not serve %s", r.URL.Path)
		})
	}
	s.handle("/", func(_ http.ResponseWriter, r *http.Request) error {
		return notFound("page not found")
	})
}

func (s *apiServer) getStatus(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, apiStatus{Pid: os.Getpid(), Pipe: s.pipe, Also: s.also, Root: s.root, Started: s.started, Version: versionString(),
		Log: filepath.Join(apiDir(s.root), "server.log")})
	return nil
}

func (s *apiServer) postShutdown(w http.ResponseWriter, _ *http.Request) error {
	w.WriteHeader(http.StatusNoContent)
	s.stop()
	return nil
}

func (s *apiServer) ping(w http.ResponseWriter, r *http.Request) error {
	h := w.Header()
	// BuildKit, as /grpc and /session serve it, for buildx's docker
	// driver. A docker CLI without buildx still uses /build.
	h.Set("Builder-Version", "2")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Swarm", "inactive")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method == http.MethodHead {
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "OK")
	return nil
}

// buildTime is when the commit runwhp was built from was made, if the
// build recorded it.
func buildTime() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.time" {
				return s.Value
			}
		}
	}
	return ""
}
