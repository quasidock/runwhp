//go:build linux

package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// root is where init mounts the container's root filesystem, and "/" once
// enterRoot has made it init's own.
var root = "/merged"

// enterRoot makes the container's root filesystem init's root, as
// switch_root does: it moves the overlay, with everything mounted in it, over
// the initramfs and changes root into it. The container's processes then have
// their mount namespace's real root instead of a chroot, which the kernel
// requires before it lets them create user namespaces or pivot_root, as
// container tools do.
func enterRoot() error {
	if err := syscall.Chdir(root); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("moving the root filesystem over the initramfs: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return err
	}
	if err := syscall.Chdir("/"); err != nil {
		return err
	}
	root = "/"
	return nil
}

// within reports whether p, a clean absolute path, is root or below it.
func within(p string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// waitDisks waits until a virtio disk with each of the serials has
// appeared, and returns their device paths by serial.
func waitDisks(serials []string) (map[string]string, error) {
	devices := map[string]string{} // serial -> /dev/vdX
	complete := func() bool {
		blocks, _ := filepath.Glob("/sys/block/vd*")
		for _, b := range blocks {
			if serial := readTrimmed(b + "/serial"); serial != "" && devices[serial] == "" {
				devices[serial] = "/dev/" + filepath.Base(b)
			}
		}
		for _, s := range serials {
			if devices[s] == "" {
				return false
			}
		}
		return true
	}
	if !poll(5*time.Second, complete) {
		var missing []string
		for _, s := range serials {
			if devices[s] == "" {
				missing = append(missing, s)
			}
		}
		return nil, fmt.Errorf("disks did not appear: %s missing, found %v", strings.Join(missing, ", "), devices)
	}
	return devices, nil
}

// mountRoot mounts each layer read-only and stacks them under an overlay
// whose writable upper layer is the scratch disk, or a tmpfs without one.
func mountRoot(spec *guestspec.Spec) error {
	serials := make([]string, spec.Layers)
	for i := range serials {
		serials[i] = guestspec.LayerSerial(i)
	}
	if spec.Scratch {
		serials = append(serials, guestspec.ScratchSerial)
	}
	devices, err := waitDisks(serials)
	if err != nil {
		return err
	}

	lower := make([]string, 0, spec.Layers)
	for i := spec.Layers - 1; i >= 0; i-- {
		dir, err := mountLayer(i, devices[guestspec.LayerSerial(i)])
		if err != nil {
			return err
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
	opts := "lowerdir=" + strings.Join(lower, ":") + ",upperdir=/rw/upper,workdir=/rw/work"
	if spec.Scratch {
		dev := devices[guestspec.ScratchSerial]
		replay := spec.KeepLayer && needsRecovery(dev)
		if err := syscall.Mount(dev, "/rw", "ext4", 0, scratchMountOpts); err != nil {
			return fmt.Errorf("mounting the scratch disk (%s): %w", dev, err)
		}
		if spec.KeepLayer {
			// The layer outlives the run, so stopStorage freezes it.
			fd, err := syscall.Open("/rw", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			storage.rw = fd
			openRCUExpedited()
			if replay {
				debugf("writable layer (%s): replayed its journal", dev)
			}
		} else {
			// The scratch disk is thrown away after the run, so syncs on
			// the container's root need not reach it.
			opts += overlayScratchOpts
		}
		debugf("writable layer on the scratch disk, %s (kept %v)", dev, spec.KeepLayer)
	} else if err := syscall.Mount("tmpfs", "/rw", "tmpfs", 0, "mode=755"); err != nil {
		return fmt.Errorf("mounting tmpfs for the writable layer: %w", err)
	}
	os.Mkdir("/rw/upper", 0o755)
	os.Mkdir("/rw/work", 0o755)
	if err := syscall.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mounting overlay: %w", err)
	}
	// The mount points init makes for itself go in a layer of their own,
	// as Docker's go in its init layer, so they are not the container's
	// changes.
	plan, err := planInitLayer(spec)
	if err != nil {
		return err
	}
	if len(plan) > 0 {
		if err := syscall.Unmount(root, 0); err != nil {
			return fmt.Errorf("unmounting overlay: %w", err)
		}
		// A volatile overlay leaves work/incompat/volatile behind, even
		// unmounted, and overlayfs refuses to mount over it. This one was
		// mounted a moment ago, on a scratch disk made for this run.
		if err := os.RemoveAll("/rw/work/work/incompat"); err != nil {
			return err
		}
		if err := os.MkdirAll(initLayerDir, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount("tmpfs", initLayerDir, "tmpfs", 0, "mode=755"); err != nil {
			return fmt.Errorf("mounting tmpfs for the init layer: %w", err)
		}
		if err := makeInitLayer(initLayerDir, plan); err != nil {
			return fmt.Errorf("making the init layer: %w", err)
		}
		lower = append([]string{initLayerDir}, lower...)
		opts = "lowerdir=" + strings.Join(lower, ":") + opts[strings.Index(opts, ",upperdir="):]
		if err := syscall.Mount("overlay", root, "overlay", 0, opts); err != nil {
			return fmt.Errorf("mounting overlay: %w", err)
		}
		debugf("init layer: %d mount points", len(plan))
	}
	openLayerRoots(lower)
	return nil
}

// mountLayer mounts layer i's disk, dev, read-only at /layers/<i>.
func mountLayer(i int, dev string) (string, error) {
	dir := fmt.Sprintf("/layers/%d", i)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := syscall.Mount(dev, dir, "ext4", syscall.MS_RDONLY|syscall.MS_NODEV, ""); err != nil {
		return "", fmt.Errorf("mounting layer %d (%s): %w", i, dev, err)
	}
	return dir, nil
}

// Mount options for the scratch disk's ext4 and the overlay above it.
// discard gives the host back the space of what the container deletes
// during the run. volatile drops the overlay's syncs, which a scratch disk
// thrown away after the run does not need. A kept one cannot have it: a
// volatile overlay leaves a mark in its work directory that only an unmount
// removes, and refuses to be mounted again while it is there.
const (
	scratchMountOpts   = "discard"
	overlayScratchOpts = ",volatile"
)

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
	sysFlags := uintptr(nosuid | nodev | noexec | syscall.MS_RDONLY)
	if spec.Privileged {
		sysFlags &^= syscall.MS_RDONLY
	}
	mounts := []mount{
		{"proc", "/proc", "proc", nosuid | nodev | noexec, ""},
		{"sysfs", "/sys", "sysfs", sysFlags, ""},
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
			if err := populateDev(in("/dev"), spec.Privileged); err != nil {
				return err
			}
		}
	}

	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		return err
	}
	if spec.Hostname != "" {
		if err := syscall.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("setting hostname: %w", err)
		}
		writeEtc("hostname", spec.Hostname+"\n")
	}
	if spec.Hostname != "" || len(spec.Hosts) > 0 {
		writeEtc("hosts", hostsFile(spec))
	}
	if err := loopbackUp(); err != nil {
		logf("bringing up lo: %v", err)
	}
	// The embedded DNS needs only lo, so it starts while the interfaces
	// are set up.
	embedded := make(chan bool, 1)
	if r := spec.Resolv; r != nil && r.Embedded != "" && spec.Serve == "" {
		go func() { embedded <- embeddedDNSUp(r) }()
	} else {
		// A guest serving a stopped container's files serves no DNS, but
		// shows the container the resolv.conf it has when it runs.
		embedded <- r != nil && r.Embedded != ""
	}
	// A guest serving a stopped container's files has no network devices,
	// but shows the container its /etc/resolv.conf.
	if len(spec.Networks) > 0 && spec.Serve == "" {
		if err := configureNetworks(spec.Networks); err != nil {
			return err
		}
	}
	if r := spec.Resolv; r != nil {
		writeEtc("resolv.conf", resolvFile(r, <-embedded))
	}
	return nil
}

// embeddedDNSUp starts the embedded DNS a container on a user-defined
// network gets, and reports whether resolv.conf should name it. Should the
// server fail to start, as on a kernel without nftables' NAT, the container
// asks the host's server at its gateway itself, as it did before there was
// one.
func embeddedDNSUp(r *guestspec.Resolv) bool {
	up, err := netip.ParseAddr(r.Embedded)
	if err != nil {
		logf("embedded DNS: %v", err)
		return false
	}
	start := time.Now()
	if err := startEmbeddedDNS(up); err != nil {
		logf("embedded DNS at %s: %v; resolv.conf names %s", guestspec.EmbeddedDNS, err, strings.Join(r.Nameservers, ", "))
		return false
	}
	debugf("embedded DNS at %s in %d µs", guestspec.EmbeddedDNS, time.Since(start).Microseconds())
	return true
}

// hostsFile is the container's /etc/hosts: on a user-defined network,
// Docker's, which names the container at its addresses; otherwise runwhp's
// own, which names it at 127.0.0.1.
func hostsFile(spec *guestspec.Spec) string {
	var b strings.Builder
	if len(spec.HostnameAddrs) == 0 {
		b.WriteString("127.0.0.1\tlocalhost " + spec.Hostname + "\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	} else {
		b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n" +
			"fe00::\tip6-localnet\nff00::\tip6-mcastprefix\nff02::1\tip6-allnodes\nff02::2\tip6-allrouters\n")
	}
	for _, h := range spec.Hosts {
		b.WriteString(h + "\n")
	}
	if spec.Hostname != "" {
		for _, a := range spec.HostnameAddrs {
			b.WriteString(a + "\t" + spec.Hostname + "\n")
		}
	}
	return b.String()
}

// resolvFile is the container's /etc/resolv.conf, naming the embedded DNS
// server rather than r's if embedded.
func resolvFile(r *guestspec.Resolv, embedded bool) string {
	var b strings.Builder
	servers := r.Nameservers
	if embedded {
		servers = []string{guestspec.EmbeddedDNS}
	}
	for _, server := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", server)
	}
	if len(r.Search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(r.Search, " "))
	}
	if len(r.Options) > 0 {
		fmt.Fprintf(&b, "options %s\n", strings.Join(r.Options, " "))
	}
	return b.String()
}

// etcDir holds, in the initramfs, the files bindEtc binds over the
// image's /etc/hostname, /etc/hosts and /etc/resolv.conf.
const etcDir = "/runwhp/etc"

// etcFiles are the files of etcDir, in the order bindEtc binds them.
var etcFiles = []string{"hostname", "hosts", "resolv.conf"}

func writeEtc(name, content string) {
	if err := os.WriteFile(filepath.Join(etcDir, name), []byte(content), 0o644); err != nil {
		logf("writing %s: %v", name, err)
	}
}

// bindEtc binds the files prepareRoot wrote over the image's, as Docker
// does, rather than writing them into the writable layer, which would keep
// them in a container's changes. A mount the spec makes at one of those
// paths wins, and none is bound where the host's files are, so that runwhp
// never makes files on the host.
func bindEtc(spec *guestspec.Spec) error {
	for _, name := range etcFiles {
		src := filepath.Join(etcDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		target := "/etc/" + name
		if mountedOver(spec, target) {
			continue
		}
		p, err := resolveIn(target)
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		if err := makeMountPoint(p, true); err != nil {
			logf("not binding %s: %v", target, err)
			continue
		}
		if err := syscall.Mount(src, p, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding %s: %w", target, err)
		}
	}
	return nil
}

// mountedOver reports whether the spec mounts something at target, or binds
// host files at or above it.
func mountedOver(spec *guestspec.Spec, target string) bool {
	for _, m := range spec.Mounts {
		if m.Target == target {
			return true
		}
		if m.Kind == guestspec.MountBind && (m.Target == "/" || strings.HasPrefix(target, m.Target+"/")) {
			return true
		}
	}
	return false
}

// populateDev makes the container's /dev: the usual devices, and, for a
// privileged container, those of FUSE, TUN and the kernel log, as Docker's
// --privileged gives the host's. Not the guest's devtmpfs: its disks and
// console ports are runwhp's, not the container's.
func populateDev(dev string, privileged bool) error {
	type node struct {
		name         string
		major, minor uint32
		mode         uint32
	}
	nodes := []node{
		{"null", 1, 3, 0o666}, {"zero", 1, 5, 0o666}, {"full", 1, 7, 0o666}, {"random", 1, 8, 0o666}, {"urandom", 1, 9, 0o666}, {"tty", 5, 0, 0o666},
	}
	if privileged {
		nodes = append(nodes, node{"fuse", 10, 229, 0o666}, node{"net/tun", 10, 200, 0o666}, node{"kmsg", 1, 11, 0o644})
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	for _, n := range nodes {
		p := filepath.Join(dev, n.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		devnum := int(n.major<<8 | n.minor)
		if err := syscall.Mknod(p, syscall.S_IFCHR|n.mode, devnum); err != nil {
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
