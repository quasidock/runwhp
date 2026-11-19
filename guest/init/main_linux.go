//go:build linux

// runwhp-init is PID 1 inside a runwhp guest. It mounts the image layers as
// an overlay, runs the container process in it with its output wired to the
// host through virtio console ports, reports how the process ended, and
// resets the machine so the VMM exits.
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

	status := guestspec.Status{Exit: guestspec.ExitRuntime}
	control, err := start(begin)
	if err != nil {
		var f *failure
		if e, ok := err.(*failure); ok {
			f = e
		} else {
			f = &failure{guestspec.ExitRuntime, err}
		}
		logf("%v", f.err)
		status = guestspec.Status{Exit: f.code, Error: f.err.Error()}
	} else {
		status = control.status
	}
	if control != nil && control.port != nil {
		line, _ := json.Marshal(status)
		if _, err := control.port.Write(append(line, '\n')); err != nil {
			logf("reporting status: %v", err)
		}
	}
	shutdown()
}

type outcome struct {
	port   *os.File
	status guestspec.Status
}

func start(begin time.Time) (*outcome, error) {
	data, err := os.ReadFile("/" + guestspec.SpecPath)
	if err != nil {
		return nil, err
	}
	var spec guestspec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("reading %s: %w", guestspec.SpecPath, err)
	}
	debug = spec.Debug
	debugf("started %d ms after the kernel handed over", time.Since(begin).Milliseconds())
	loadModules()

	ports, err := openPorts([]string{guestspec.PortStdout, guestspec.PortStderr, guestspec.PortControl}, 5*time.Second)
	out := &outcome{}
	if p := ports[guestspec.PortControl]; p != nil {
		out.port = p
	}
	if err != nil {
		return out, err
	}
	debugf("console ports open")

	if err := mountRoot(spec.Layers); err != nil {
		return out, err
	}
	debugf("root filesystem mounted from %d layers", spec.Layers)
	if err := prepareRoot(spec.Hostname); err != nil {
		return out, err
	}
	out.status, err = runProcess(&spec, ports[guestspec.PortStdout], ports[guestspec.PortStderr])
	return out, err
}

func shutdown() {
	syscall.Sync()
	err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	logf("reboot failed: %v", err)
	for {
		time.Sleep(time.Hour)
	}
}

// loadModules loads the kernel modules the host put in the initramfs.
func loadModules() {
	paths, _ := filepath.Glob("/modules/*.ko")
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			logf("module %s: %v", path, err)
			continue
		}
		const sysFinitModule = 313
		var noParams [1]byte
		_, _, errno := syscall.Syscall(sysFinitModule, f.Fd(), uintptr(unsafe.Pointer(&noParams[0])), 0)
		f.Close()
		if errno != 0 && errno != syscall.EEXIST {
			logf("module %s: %v", filepath.Base(path), errno)
		} else {
			debugf("loaded module %s", filepath.Base(path))
		}
	}
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

// openPorts opens the named virtio console ports for writing. The host names
// each port shortly after the driver adds it, so this waits for the names.
func openPorts(names []string, limit time.Duration) (map[string]*os.File, error) {
	byName := map[string]string{}
	complete := func() bool {
		dirs, _ := filepath.Glob("/sys/class/virtio-ports/vport*")
		for _, dir := range dirs {
			if name := readTrimmed(dir + "/name"); name != "" {
				byName[name] = filepath.Base(dir)
			}
		}
		for _, n := range names {
			if byName[n] == "" {
				return false
			}
		}
		return true
	}
	ok := poll(limit, complete)
	ports := map[string]*os.File{}
	for _, n := range names {
		dev := byName[n]
		if dev == "" {
			continue
		}
		f, err := os.OpenFile("/dev/"+dev, os.O_WRONLY, 0)
		if err != nil {
			return ports, fmt.Errorf("opening console port %s: %w", n, err)
		}
		ports[n] = f
	}
	if !ok {
		return ports, fmt.Errorf("console ports did not appear (found %v)", byName)
	}
	return ports, nil
}
