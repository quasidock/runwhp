//go:build linux

// runwhp-init is PID 1 inside a runwhp guest. It mounts the image layers as
// an overlay, runs the container process in it with its standard streams
// relayed to the host through virtio console ports, passes on the host's
// signals and terminal sizes, reports how the process ended, and resets the
// machine so the VMM exits.
//
// The container shares init's PID namespace, so init keeps it from stopping
// the machine itself: see protectInit.
//
// The contract with the host is in internal/guestspec.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/quasidock/runwhp/internal/guestspec"
)

var (
	console *os.File // the kernel console, for init's own diagnostics
	debug   bool
)

// logf writes to the kernel console, which the host keeps and shows when
// something goes wrong (or always, with --debug).
func logf(format string, a ...any) {
	if console != nil {
		fmt.Fprintf(console, "runwhp-init: "+format+"\n", a...)
	}
}

func debugf(format string, a ...any) {
	if debug {
		logf(format, a...)
	}
}

// failure is an error with the exit code the host should report.
type failure struct {
	code int
	err  error
}

func (f *failure) Error() string { return f.err.Error() }

func fail(code int, format string, a ...any) *failure {
	return &failure{code, fmt.Errorf(format, a...)}
}

func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "runwhp-init must run as PID 1 inside a runwhp guest")
		os.Exit(1)
	}
	begin := time.Now()
	syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	syscall.Mount("proc", "/proc", "proc", 0, "")
	syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	console, _ = os.OpenFile("/dev/console", os.O_WRONLY, 0)
	protectInit() // before init starts anything

	ctl := newController(false)
	status, err := start(begin, ctl)
	if err != nil {
		var f *failure
		if e, ok := err.(*failure); ok {
			f = e
		} else {
			f = &failure{guestspec.ExitRuntime, err}
		}
		logf("%v", f.err)
		status = guestspec.Status{Exit: f.code, Error: f.err.Error()}
	}
	ctl.send(guestspec.Event{Exited: true, Exit: status.Exit, Error: status.Error})
	shutdown()
}

func start(begin time.Time, ctl *controller) (guestspec.Status, error) {
	var none guestspec.Status
	data, err := os.ReadFile("/" + guestspec.SpecPath)
	if err != nil {
		return none, err
	}
	var spec guestspec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return none, fmt.Errorf("reading %s: %w", guestspec.SpecPath, err)
	}
	debug = spec.Debug
	ctl.waitMode = spec.WaitStart
	debugf("started %d ms after the kernel handed over", time.Since(begin).Milliseconds())
	loadModules()

	// Control first: once it is open, the host can send signals.
	want := []portMode{{guestspec.PortControl, os.O_RDWR}, {guestspec.PortStdout, os.O_WRONLY}, {guestspec.PortStderr, os.O_WRONLY}}
	if spec.Stdin {
		want = append(want, portMode{guestspec.PortStdin, os.O_RDONLY})
	}
	if spec.Serve != "" {
		want = want[:1] // no process, so no output
	}
	ports, err := openPorts(want, 5*time.Second)
	if p := ports[guestspec.PortControl]; p != nil {
		ctl.setPort(p)
		go ctl.listen(p)
	}
	if err != nil {
		return none, err
	}
	debugf("console ports open")

	switch spec.Serve {
	case "":
	case guestspec.ServeLayers:
		return none, serveLayers(&spec, ctl)
	default:
		return none, fmt.Errorf("init cannot serve %q", spec.Serve)
	}

	if err := mountRoot(&spec); err != nil {
		return none, err
	}
	debugf("root filesystem mounted from %d layers", spec.Layers)
	if err := prepareRoot(&spec); err != nil {
		return none, err
	}
	user, err := resolveUser(&spec)
	if err != nil {
		return none, err
	}
	if err := mountAll(&spec, user); err != nil {
		return none, err
	}
	// After the other mounts, so that a volume on /etc gets runwhp's
	// files too, and a mount of the user's own /etc/hosts, say, wins.
	if err := bindEtc(&spec); err != nil {
		return none, err
	}
	// Before the container's root covers the initramfs, where init mounts
	// the cgroup hierarchy for itself.
	mem := openMemFiles()
	ctl.mem.Store(mem)
	if err := enterRoot(); err != nil {
		return none, err
	}
	return runProcess(&spec, user, ports, ctl, mem)
}

func shutdown() {
	stopStorage()
	err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	logf("reboot failed: %v", err)
	for {
		time.Sleep(time.Hour)
	}
}

// loadModules loads the kernel modules the host put in the initramfs.
func loadModules() {
	pending, _ := filepath.Glob("/modules/*.ko")
	// A module that needs another fails until that one is loaded, and
	// names do not sort by dependency, so the failures are tried again
	// for as long as something loads.
	for len(pending) > 0 {
		var failed []string
		errs := map[string]error{}
		for _, path := range pending {
			if err := loadModule(path); err != nil {
				failed = append(failed, path)
				errs[path] = err
			} else {
				debugf("loaded module %s", filepath.Base(path))
			}
		}
		if len(failed) == len(pending) {
			for _, path := range failed {
				logf("module %s: %v", filepath.Base(path), errs[path])
			}
			return
		}
		pending = failed
	}
}

func loadModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const sysFinitModule = 313
	var noParams [1]byte
	_, _, errno := syscall.Syscall(sysFinitModule, f.Fd(), uintptr(unsafe.Pointer(&noParams[0])), 0)
	if errno != 0 && errno != syscall.EEXIST {
		return errno
	}
	return nil
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// poll calls found until it reports true or the limit passes.
func poll(limit time.Duration, found func() bool) bool {
	for deadline := time.Now().Add(limit); ; time.Sleep(2 * time.Millisecond) {
		if found() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

type portMode struct {
	name string
	flag int
}

// openPorts opens the named virtio console ports, in order. The host names
// each port shortly after the driver adds it, so this waits for the names.
func openPorts(want []portMode, limit time.Duration) (map[string]*os.File, error) {
	byName := map[string]string{}
	complete := func() bool {
		dirs, _ := filepath.Glob("/sys/class/virtio-ports/vport*")
		for _, dir := range dirs {
			if name := readTrimmed(dir + "/name"); name != "" {
				byName[name] = filepath.Base(dir)
			}
		}
		for _, p := range want {
			if byName[p.name] == "" {
				return false
			}
		}
		return true
	}
	ok := poll(limit, complete)
	ports := map[string]*os.File{}
	for _, p := range want {
		dev := byName[p.name]
		if dev == "" {
			continue
		}
		f, err := os.OpenFile("/dev/"+dev, p.flag, 0)
		if err != nil {
			return ports, fmt.Errorf("opening console port %s: %w", p.name, err)
		}
		ports[p.name] = f
	}
	if !ok {
		return ports, fmt.Errorf("console ports did not appear (found %v)", byName)
	}
	return ports, nil
}
