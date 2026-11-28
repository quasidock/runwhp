// Package ocispec turns what containerd hands a shim for a task, an OCI
// runtime spec, its annotations and the root file system's mounts, into
// the configuration runwhp runs a container from.
//
// nerdctl on Windows writes a spec for a Linux image much as for a Linux
// host, with a Windows section beside it: the process (args, env, cwd and
// a numeric user), mounts, and annotations, but memory and CPUs in
// windows.resources, mount destinations as Windows paths (C:\data, or
// \data for an image's VOLUME), no hostname but in an annotation, and
// published ports in network-config.json in the directory the
// nerdctl/state-dir annotation names. A Linux spec, as ctr writes one,
// is taken too.
//
// The root file system is runwhp's snapshotter's: containerd's LCOW
// layout, one mount of type lcow-layer whose source is the container's
// snapshot directory and whose parentLayerPaths option lists the layers'
// directories, nearest first, each holding layer.vhd, an ext4 image.
package ocispec

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
)

// Annotations runwhp reads.
const (
	// NetworkAnnotation turns the container's network off with "none";
	// nerdctl's own --network can only be none, and every container gets
	// runwhp's network otherwise.
	NetworkAnnotation = "io.runwhp.network"

	stateDirAnnotation   = "nerdctl/state-dir"
	hostnameAnnotation   = "nerdctl/hostname"
	extraHostsAnnotation = "nerdctl/extraHosts"
	mountsAnnotation     = "nerdctl/mounts."
)

// LayerMountType is the type of the root file system's mount.
const LayerMountType = "lcow-layer"

// Mount is a mount of the root file system, as containerd passes it.
type Mount struct {
	Type, Source string
	Options      []string
}

// Task is what containerd hands the shim to create a task from.
type Task struct {
	Namespace, ID string
	Spec          *specs.Spec
	Rootfs        []Mount
	// Stdin says the task has an input; Terminal that it runs on one.
	Stdin, Terminal bool
}

// Config is the container configuration task runs as. Memory and CPUs are
// 0 where the spec sets none, for the caller's defaults; ID, Created,
// Version, the kernel and the storage size are the caller's too.
func Config(t Task) (*container.Config, error) {
	s := t.Spec
	if s == nil || s.Process == nil {
		return nil, errors.New("the spec has no process")
	}
	p := s.Process
	cfg := &container.Config{
		Containerd: &container.ContainerdTask{Namespace: t.Namespace, ID: t.ID},
		Stdin:      t.Stdin, StdinOnce: t.Stdin, TTY: t.Terminal || p.Terminal,
		// containerd takes the container's output; runwhp keeps no log of
		// its own.
		LogDriver: container.LogDriverNone,
	}
	var err error
	if cfg.Args, err = args(p); err != nil {
		return nil, err
	}
	cfg.Env = env(p.Env)
	cfg.Cwd = "/"
	if p.Cwd != "" {
		if cfg.Cwd, err = linuxPath(p.Cwd); err != nil {
			return nil, fmt.Errorf("working directory: %w", err)
		}
	}
	cfg.User = user(p.User)
	cfg.Hostname = s.Hostname
	if cfg.Hostname == "" {
		cfg.Hostname = s.Annotations[hostnameAnnotation]
	}
	cfg.Memory, cfg.CPUs = resources(s)
	if cfg.LayerFiles, cfg.RWPath, err = rootfs(t.Rootfs); err != nil {
		return nil, err
	}
	if err := network(cfg, s.Annotations); err != nil {
		return nil, err
	}
	if err := mounts(cfg, s); err != nil {
		return nil, err
	}
	return cfg, nil
}

// args is the process's arguments: Args, or a Windows image's CommandLine,
// split as Windows splits it.
func args(p *specs.Process) ([]string, error) {
	if len(p.Args) > 0 {
		return p.Args, nil
	}
	if p.CommandLine != "" {
		return strings.Fields(p.CommandLine), nil
	}
	return nil, errors.New("the spec's process has no arguments")
}

// env is the process's environment, without an empty HOSTNAME, which
// nerdctl sets when it was given no --hostname, and which would hide the
// one init sets.
func env(e []string) []string {
	var out []string
	for _, kv := range e {
		if kv == "HOSTNAME=" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// user is the process's user as runwhp takes one: a name, which init
// resolves in the image, or uid:gid; empty for root.
func user(u specs.User) string {
	if u.Username != "" {
		return u.Username
	}
	if u.UID == 0 && u.GID == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", u.UID, u.GID)
}

// resources reads memory and CPUs from the spec's Windows section, where
// nerdctl puts them, else its Linux one. runwhp counts whole vCPUs, so a
// quota rounds up.
func resources(s *specs.Spec) (memory uint64, cpus int) {
	if w := s.Windows; w != nil && w.Resources != nil {
		if m := w.Resources.Memory; m != nil && m.Limit != nil {
			memory = *m.Limit
		}
		if c := w.Resources.CPU; c != nil && c.Count != nil {
			cpus = int(*c.Count)
		}
	}
	if l := s.Linux; l != nil && l.Resources != nil {
		if m := l.Resources.Memory; memory == 0 && m != nil && m.Limit != nil && *m.Limit > 0 {
			memory = uint64(*m.Limit)
		}
		if c := l.Resources.CPU; cpus == 0 && c != nil && c.Quota != nil && c.Period != nil && *c.Quota > 0 && *c.Period > 0 {
			cpus = int(math.Ceil(float64(*c.Quota) / float64(*c.Period)))
		}
	}
	return memory, cpus
}

// rootfs reads the root file system's mount: the layers' ext4 images,
// bottom first, and the writable layer, kept in the container's snapshot
// directory.
func rootfs(ms []Mount) (layers []string, rw string, err error) {
	if len(ms) != 1 || ms[0].Type != LayerMountType {
		var types []string
		for _, m := range ms {
			types = append(types, m.Type)
		}
		return nil, "", fmt.Errorf("the root file system must be one %s mount, from runwhp's snapshotter (--snapshotter runwhp); got %d mounts %q", LayerMountType, len(ms), types)
	}
	m := ms[0]
	var parents []string
	for _, o := range m.Options {
		if v, ok := strings.CutPrefix(o, "parentLayerPaths="); ok {
			if err := json.Unmarshal([]byte(v), &parents); err != nil {
				return nil, "", fmt.Errorf("the root file system's parentLayerPaths: %w", err)
			}
		}
	}
	if len(parents) == 0 {
		return nil, "", errors.New("the root file system has no image layers")
	}
	for i := len(parents) - 1; i >= 0; i-- {
		layers = append(layers, filepath.Join(parents[i], "layer.vhd"))
	}
	return layers, filepath.Join(m.Source, "rw.ext4"), nil
}

// portMapping is one of nerdctl's published ports, as network-config.json
// keeps them.
type portMapping struct {
	HostPort      int    `json:"HostPort"`
	ContainerPort int    `json:"ContainerPort"`
	Protocol      string `json:"Protocol"`
	HostIP        string `json:"HostIP"`
}

// network gives the container runwhp's network, unless NetworkAnnotation
// says none, with the ports nerdctl published and the hosts it added.
func network(cfg *container.Config, ann map[string]string) error {
	switch v := ann[NetworkAnnotation]; v {
	case "", "user":
		cfg.Network = true
	case "none":
	default:
		return fmt.Errorf("%s=%q: want user or none", NetworkAnnotation, v)
	}
	if v := ann[extraHostsAnnotation]; v != "" {
		var hosts []string
		if err := json.Unmarshal([]byte(v), &hosts); err != nil {
			return fmt.Errorf("%s: %w", extraHostsAnnotation, err)
		}
		for _, h := range hosts {
			line, gateway, err := netstack.HostsLine(h)
			if err != nil {
				return err
			}
			// nerdctl resolves host-gateway itself, to the address its
			// host_gateway_ip setting gives: runwhp's gateway's, which
			// then reaches the host as runwhp's host-gateway does.
			gateway = gateway || strings.HasPrefix(line, netstack.Gateway.String()+"\t")
			cfg.Hosts = append(cfg.Hosts, line)
			cfg.HostGateway = cfg.HostGateway || gateway
		}
	}
	dir := ann[stateDirAnnotation]
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "network-config.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var nc struct {
		PortMappings []portMapping `json:"portMappings"`
	}
	if err := json.Unmarshal(data, &nc); err != nil {
		return fmt.Errorf("nerdctl's network-config.json: %w", err)
	}
	for _, pm := range nc.PortMappings {
		proto, err := netstack.ParseProto(pm.Protocol)
		if err != nil {
			return fmt.Errorf("-p %d:%d/%s: %v", pm.HostPort, pm.ContainerPort, pm.Protocol, err)
		}
		// A host port of 0 is any free one.
		if pm.HostPort < 0 || pm.HostPort > 65535 || pm.ContainerPort < 1 || pm.ContainerPort > 65535 {
			return fmt.Errorf("-p %d:%d: give ports from 1 to 65535, or host port 0 for any", pm.HostPort, pm.ContainerPort)
		}
		ip := netip.IPv4Unspecified()
		if pm.HostIP != "" {
			if ip, err = netip.ParseAddr(pm.HostIP); err != nil {
				return fmt.Errorf("-p %s:%d:%d: %w", pm.HostIP, pm.HostPort, pm.ContainerPort, err)
			}
		}
		cfg.Publish = append(cfg.Publish, netstack.Port{Host: netip.AddrPortFrom(ip, uint16(pm.HostPort)), Guest: uint16(pm.ContainerPort), Proto: proto})
	}
	if !cfg.Network && (len(cfg.Publish) > 0 || cfg.HostGateway) {
		return fmt.Errorf("%s=none cannot be combined with -p or --add-host NAME:host-gateway", NetworkAnnotation)
	}
	return nil
}

// initMounts are the mounts init makes itself, which the spec may list.
var initMounts = map[string]bool{
	"/proc": true, "/sys": true, "/sys/fs/cgroup": true, "/dev": true, "/dev/pts": true,
	"/dev/shm": true, "/dev/mqueue": true,
}

// nerdctlMount is the part of a nerdctl/mounts.N annotation runwhp reads.
type nerdctlMount struct {
	Type        string `json:"Type"`
	Destination string `json:"Destination"`
}

// mounts turns the spec's mounts into shares of host directories and
// tmpfs mounts. Volumes, nerdctl's named ones and an image's, are host
// directories too, which init fills from the image while they are empty.
func mounts(cfg *container.Config, s *specs.Spec) error {
	volumes := map[string]bool{} // destinations, as the spec gives them
	for k, v := range s.Annotations {
		if !strings.HasPrefix(k, mountsAnnotation) {
			continue
		}
		var m nerdctlMount
		if json.Unmarshal([]byte(v), &m) == nil && m.Type == "volume" {
			volumes[m.Destination] = true
		}
	}
	// The mounts to make, by target. nerdctl on Windows does not see that
	// -v C:\data and an image's VOLUME /data, which it writes as \data,
	// are the same, and asks for both; -v's, which alone has a drive
	// letter, wins, as Docker's user mounts do.
	type candidate struct {
		m      specs.Mount
		target string
	}
	var order []string
	chosen := map[string]candidate{}
	for _, m := range s.Mounts {
		target, err := linuxPath(m.Destination)
		if err != nil {
			return fmt.Errorf("mount %s: %w", m.Destination, err)
		}
		if initMounts[target] {
			continue
		}
		switch m.Type {
		case "proc", "sysfs", "devpts", "mqueue", "cgroup", "cgroup2":
			continue
		}
		if prev, ok := chosen[target]; ok {
			prevUser, user := hasDrive(prev.m.Destination), hasDrive(m.Destination)
			if prevUser == user {
				return fmt.Errorf("%s is mounted more than once", target)
			}
			if user {
				chosen[target] = candidate{m, target}
			}
			continue
		}
		chosen[target] = candidate{m, target}
		order = append(order, target)
	}
	var shares []container.Share
	var gms []guestspec.Mount
	for _, target := range order {
		m := chosen[target].m
		readOnly := false
		for _, o := range m.Options {
			readOnly = readOnly || o == "ro"
		}
		if m.Type == "tmpfs" {
			gms = append(gms, guestspec.Mount{Kind: guestspec.MountTmpfs, Target: target, ReadOnly: readOnly, Options: tmpfsOptions(m.Options)})
			continue
		}
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("mount %s: source %q is not a Windows path", m.Destination, m.Source)
		}
		st, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("bind source %s: %w", m.Source, err)
		}
		gm := guestspec.Mount{Kind: guestspec.MountBind, Target: target, Share: len(shares), File: !st.IsDir(), ReadOnly: readOnly,
			CopyUp: volumes[m.Destination] && st.IsDir()}
		shares = append(shares, container.Share{Path: m.Source, File: gm.File, ReadOnly: readOnly, Target: target})
		gms = append(gms, gm)
	}
	// Parents are mounted before what is mounted inside them.
	sort.SliceStable(gms, func(i, j int) bool {
		return strings.Count(gms[i].Target, "/") < strings.Count(gms[j].Target, "/")
	})
	cfg.Shares, cfg.Mounts = shares, gms
	return nil
}

// tmpfsOptions keeps the tmpfs mount options init passes on.
func tmpfsOptions(opts []string) string {
	var keep []string
	for _, o := range opts {
		name, _, _ := strings.Cut(o, "=")
		switch name {
		case "noexec", "nosuid", "nodev", "size", "mode", "uid", "gid", "nr_inodes":
			keep = append(keep, o)
		}
	}
	return strings.Join(keep, ",")
}

// linuxPath turns a destination as nerdctl writes it on Windows, C:\data
// for -v or \data for an image's VOLUME, into the container's /data: the
// drive letter goes, and backslashes become slashes. A Linux path is
// taken as it is.
func linuxPath(p string) (string, error) {
	if hasDrive(p) {
		p = p[2:]
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	return path.Clean(p), nil
}

// Exec is the exec for process p of a task, as containerd's Exec gives it:
// the container's process with the exec's arguments, and its own
// terminal.
func Exec(p *specs.Process, terminal bool) (*guestspec.Exec, error) {
	if p == nil {
		return nil, errors.New("the exec has no process")
	}
	a, err := args(p)
	if err != nil {
		return nil, err
	}
	ex := &guestspec.Exec{Args: a, Env: env(p.Env), User: user(p.User), TTY: terminal || p.Terminal}
	if p.Cwd != "" {
		if ex.Cwd, err = linuxPath(p.Cwd); err != nil {
			return nil, fmt.Errorf("working directory: %w", err)
		}
	}
	if ex.TTY && p.ConsoleSize != nil {
		ex.Rows, ex.Cols = uint16(p.ConsoleSize.Height), uint16(p.ConsoleSize.Width)
	}
	return ex, nil
}

// hasDrive says p starts with a drive letter, as nerdctl's -v destinations
// on Windows do, and its image volumes' do not.
func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z')
}
