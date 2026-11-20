//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

const root = "/merged"

// mountRoot mounts each layer read-only and stacks them under an overlay
// whose writable upper layer is a tmpfs.
func mountRoot(layers int) error {
	devices := map[string]string{} // serial -> /dev/vdX
	want := make([]string, layers)
	for i := range want {
		want[i] = guestspec.LayerSerial(i)
	}
	complete := func() bool {
		blocks, _ := filepath.Glob("/sys/block/vd*")
		for _, b := range blocks {
			if _, known := devices[readTrimmed(b+"/serial")]; known {
				continue
			}
			if serial := readTrimmed(b + "/serial"); serial != "" {
				devices[serial] = "/dev/" + filepath.Base(b)
			}
		}
		for _, s := range want {
			if devices[s] == "" {
				return false
			}
		}
		return true
	}
	if !poll(5*time.Second, complete) {
		return fmt.Errorf("layer disks did not appear: want %d, found %v", layers, devices)
	}

	lower := make([]string, 0, layers)
	for i := layers - 1; i >= 0; i-- {
		dir := fmt.Sprintf("/layers/%d", i)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		dev := devices[guestspec.LayerSerial(i)]
		if err := syscall.Mount(dev, dir, "ext4", syscall.MS_RDONLY|syscall.MS_NODEV, ""); err != nil {
			return fmt.Errorf("mounting layer %d (%s): %w", i, dev, err)
		}
		lower = append(lower, dir)
	}
	if len(lower) == 0 {
		os.MkdirAll("/layers/empty", 0o755)
		lower = append(lower, "/layers/empty")
	}

	for _, dir := range []string{"/rw", root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := syscall.Mount("tmpfs", "/rw", "tmpfs", 0, "mode=755"); err != nil {
		return fmt.Errorf("mounting tmpfs for the writable layer: %w", err)
	}
	os.Mkdir("/rw/upper", 0o755)
	os.Mkdir("/rw/work", 0o755)
	opts := "lowerdir=" + strings.Join(lower, ":") + ",upperdir=/rw/upper,workdir=/rw/work"
	if err := syscall.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mounting overlay: %w", err)
	}
	return nil
}

// prepareRoot gives the container the usual kernel filesystems, a minimal
// /dev, a hostname, /etc/hosts, a loopback interface and, if the spec asks
// for one, a network with its /etc/resolv.conf.
func prepareRoot(spec *guestspec.Spec) error {
	in := func(p string) string { return filepath.Join(root, p) }
	type mount struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}
	const nosuid, nodev, noexec = syscall.MS_NOSUID, syscall.MS_NODEV, syscall.MS_NOEXEC
	mounts := []mount{
		{"proc", "/proc", "proc", nosuid | nodev | noexec, ""},
		{"sysfs", "/sys", "sysfs", nosuid | nodev | noexec | syscall.MS_RDONLY, ""},
		{"tmpfs", "/dev", "tmpfs", nosuid, "mode=755,size=65536k"},
		{"devpts", "/dev/pts", "devpts", nosuid | noexec, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{"tmpfs", "/dev/shm", "tmpfs", nosuid | nodev | noexec, "mode=1777,size=65536k"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(in(m.target), 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.source, in(m.target), m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mounting %s on %s: %w", m.fstype, m.target, err)
		}
		if m.target == "/dev" {
			if err := populateDev(in("/dev")); err != nil {
				return err
			}
		}
	}

	os.MkdirAll(in("/etc"), 0o755)
	if spec.Hostname != "" {
		if err := syscall.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("setting hostname: %w", err)
		}
		writeEtc(in("/etc/hostname"), spec.Hostname+"\n")
	}
	if spec.Hostname != "" || len(spec.Hosts) > 0 {
		var hosts strings.Builder
		hosts.WriteString("127.0.0.1\tlocalhost " + spec.Hostname + "\n::1\tlocalhost ip6-localhost ip6-loopback\n")
		for _, h := range spec.Hosts {
			hosts.WriteString(h + "\n")
		}
		writeEtc(in("/etc/hosts"), hosts.String())
	}
	if err := loopbackUp(); err != nil {
		logf("bringing up lo: %v", err)
	}
	if n := spec.Network; n != nil {
		if err := configureNetwork(n); err != nil {
			return err
		}
		var resolv strings.Builder
		for _, server := range n.DNS {
			fmt.Fprintf(&resolv, "nameserver %s\n", server)
		}
		writeEtc(in("/etc/resolv.conf"), resolv.String())
	}
	return nil
}

// writeEtc replaces a file in the image, even when the image has it as a
// symlink, so the write never escapes into another path.
func writeEtc(path, content string) {
	os.Remove(path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		logf("writing %s: %v", path, err)
	}
}

func populateDev(dev string) error {
	nodes := []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	for _, n := range nodes {
		devnum := int(n.major<<8 | n.minor)
		if err := syscall.Mknod(filepath.Join(dev, n.name), syscall.S_IFCHR|0o666, devnum); err != nil {
			return fmt.Errorf("creating /dev/%s: %w", n.name, err)
		}
	}
	links := map[string]string{
		"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
		"ptmx": "pts/ptmx", "core": "/proc/kcore",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
			return err
		}
	}
	return nil
}
