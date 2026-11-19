//go:build linux

// init is PID 1 for the runwhp boot spike. It proves that userspace runs, that
// timers and device interrupts work, exercises any virtio block devices, and
// then asks the kernel to reset so the VMM can exit.
//
// Invoked as /bin/hello it plays the part of a container process instead.
package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var kmsg *os.File

// say prints to the console tty, falling back to the kernel log if the tty
// rejects writes (which happens when the UART has no working interrupt).
func say(format string, a ...any) {
	msg := "runwhp-init: " + fmt.Sprintf(format, a...) + "\n"
	if kmsg == nil {
		_, err := os.Stdout.WriteString(msg)
		if err == nil {
			return
		}
		kmsg, _ = os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
		if kmsg != nil {
			fmt.Fprintf(kmsg, "runwhp-init: console tty not writable (%v); using /dev/kmsg\n", err)
		}
	}
	if kmsg != nil {
		kmsg.WriteString(msg)
	}
}

func field(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return "?"
}

func first(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(data))
}

func cstr(b []int8) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}

func main() {
	if os.Args[0] == "/bin/hello" {
		container()
		return
	}
	syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	syscall.Mount("proc", "/proc", "proc", 0, "")
	syscall.Mount("sysfs", "/sys", "sysfs", 0, "")

	var uts syscall.Utsname
	syscall.Uname(&uts)
	say("hello from Linux userspace, pid %d", os.Getpid())
	say("kernel %s %s", cstr(uts.Release[:]), cstr(uts.Machine[:]))
	say("cpu: %s", field("/proc/cpuinfo", "model name"))
	say("memory: %s", field("/proc/meminfo", "MemTotal"))
	say("clocksource: %s", first("/sys/devices/system/clocksource/clocksource0/current_clocksource"))
	say("uptime at init: %s s", strings.Fields(first("/proc/uptime") + " ?")[0])

	begin := time.Now()
	time.Sleep(250 * time.Millisecond)
	say("timer check: slept 250 ms, measured %d ms", time.Since(begin).Milliseconds())

	loadModules()
	if waitFor("/dev/vda", 2*time.Second) {
		layerTest("/dev/vda")
	}
	if waitFor("/dev/vdb", 100*time.Millisecond) {
		scratchTest("/dev/vdb", 128)
	}

	say("done, resetting")
	syscall.Sync()
	err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	say("reboot failed: %v", err)
	for {
		time.Sleep(time.Hour)
	}
}

func loadModules() {
	paths, _ := filepath.Glob("/modules/*.ko")
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			say("module %s: %v", path, err)
			continue
		}
		const sysFinitModule = 313
		var noParams [1]byte
		_, _, errno := syscall.Syscall(sysFinitModule, f.Fd(), uintptr(unsafe.Pointer(&noParams[0])), 0)
		f.Close()
		if errno != 0 {
			say("module %s: %v", filepath.Base(path), errno)
		} else {
			say("loaded module %s", filepath.Base(path))
		}
	}
}

func waitFor(path string, limit time.Duration) bool {
	for deadline := time.Now().Add(limit); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func describe(dev string) string {
	name := filepath.Base(dev)
	mode := "read-write"
	if first("/sys/block/"+name+"/ro") == "1" {
		mode = "read-only"
	}
	var sectors int
	fmt.Sscan(first("/sys/block/"+name+"/size"), &sectors)
	return fmt.Sprintf("%s: %d MiB, %s", dev, sectors/2048, mode)
}

// layerTest treats the device as a read-only container image layer: mount it,
// put a writable overlay on top, and run a process chrooted into the result.
func layerTest(dev string) {
	say("layer %s", describe(dev))
	for _, dir := range []string{"/lower", "/rw", "/merged"} {
		os.Mkdir(dir, 0o755)
	}
	if err := syscall.Mount(dev, "/lower", "ext4", syscall.MS_RDONLY, ""); err != nil {
		say("mount ext4 layer: %v", err)
		return
	}
	if err := syscall.Mount("tmpfs", "/rw", "tmpfs", 0, ""); err != nil {
		say("mount tmpfs: %v", err)
		return
	}
	os.Mkdir("/rw/upper", 0o755)
	os.Mkdir("/rw/work", 0o755)
	if err := syscall.Mount("overlay", "/merged", "overlay", 0, "lowerdir=/lower,upperdir=/rw/upper,workdir=/rw/work"); err != nil {
		say("mount overlay: %v", err)
		return
	}
	say("overlay root mounted: ext4 layer below, tmpfs above")

	cmd := exec.Command("/bin/hello")
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: "/merged"}
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	if kmsg != nil {
		cmd.Stdout, cmd.Stderr = kmsg, kmsg
	}
	if err := cmd.Run(); err != nil {
		say("container process: %v", err)
		return
	}
	_, upperErr := os.Stat("/rw/upper/written-by-container")
	_, lowerErr := os.Stat("/lower/written-by-container")
	say("after exit: file is in the writable upper layer: %v, image layer untouched: %v", upperErr == nil, lowerErr != nil)
}

// container is what runs chrooted inside the overlay root.
func container() {
	entries, _ := os.ReadDir("/")
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	fmt.Printf("  container: pid %d, root contains: %s\n", os.Getpid(), strings.Join(names, " "))
	fmt.Printf("  container: /etc/os-release says: %s\n", first("/etc/os-release"))
	err := os.WriteFile("/written-by-container", []byte("hello\n"), 0o644)
	fmt.Printf("  container: wrote /written-by-container: %v\n", err == nil)
}

// scratchTest writes a pseudo-random pattern straight to the block device,
// reads it back with the page cache bypassed, and compares digests.
func scratchTest(dev string, mib int) {
	say("scratch %s", describe(dev))
	const chunk = 1 << 20
	raw := make([]byte, chunk+4096)
	off := 4096 - int(uintptr(unsafe.Pointer(&raw[0]))&4095) // O_DIRECT needs an aligned buffer
	buf := raw[off : off+chunk]

	f, err := os.OpenFile(dev, os.O_RDWR|syscall.O_DIRECT, 0)
	if err != nil {
		say("open %s: %v", dev, err)
		return
	}
	defer f.Close()

	seed := uint64(time.Now().UnixNano()) | 1
	fill := func() {
		for i := 0; i < chunk; i += 8 {
			seed ^= seed << 13
			seed ^= seed >> 7
			seed ^= seed << 17
			*(*uint64)(unsafe.Pointer(&buf[i])) = seed
		}
	}
	wrote := sha256.New()
	begin := time.Now()
	for i := 0; i < mib; i++ {
		fill()
		wrote.Write(buf)
		if _, err := f.WriteAt(buf, int64(i)*chunk); err != nil {
			say("write: %v", err)
			return
		}
	}
	if err := f.Sync(); err != nil {
		say("flush: %v", err)
		return
	}
	writeTime := time.Since(begin)

	read := sha256.New()
	begin = time.Now()
	for i := 0; i < mib; i++ {
		if _, err := f.ReadAt(buf, int64(i)*chunk); err != nil {
			say("read: %v", err)
			return
		}
		read.Write(buf)
	}
	readTime := time.Since(begin)

	rate := func(d time.Duration) float64 { return float64(mib) / d.Seconds() }
	say("wrote %d MiB at %.0f MiB/s, read back at %.0f MiB/s (includes hashing), contents match: %v",
		mib, rate(writeTime), rate(readTime), bytes.Equal(wrote.Sum(nil), read.Sum(nil)))
}
