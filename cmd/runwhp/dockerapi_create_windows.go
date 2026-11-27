//go:build windows

package main

import (
	"fmt"
	"math"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"

	dcontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dnetwork "github.com/moby/moby/api/types/network"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/network"
)

// specFromCreate turns a container create request into the spec run
// would make of the same flags, with run's checks. What runwhp cannot do as
// asked is refused, each thing named, before anything is made; what it
// leaves out without changing what the container does is warned of.
func specFromCreate(req dcontainer.CreateRequest) (runSpec, []string, error) {
	var s runSpec
	var warnings []string
	warn := func(format string, a ...any) { warnings = append(warnings, fmt.Sprintf(format, a...)) }
	cfg := req.Config
	if cfg == nil {
		return s, nil, badRequest("config cannot be empty in order to create a container")
	}
	hc := req.HostConfig
	if hc == nil {
		hc = &dcontainer.HostConfig{}
	}
	if cfg.Image == "" {
		return s, nil, badRequest("no image given: Config.Image is empty")
	}
	if err := refuseUnsupported(cfg, hc); err != nil {
		return s, nil, err
	}

	s = runSpec{
		image: cfg.Image, command: cfg.Cmd,
		env: cfg.Env, workdir: cfg.WorkingDir, user: cfg.User, hostname: cfg.Hostname,
		interactive: cfg.OpenStdin, tty: cfg.Tty, rm: hc.AutoRemove,
		labels: cfg.Labels, stopTimeout: cfg.StopTimeout, privileged: hc.Privileged,
	}
	// A bare KEY in Env is the client's to resolve; one it did not is
	// dropped, as no environment of the client's is here.
	if cfg.Entrypoint != nil {
		s.entrypointSet = true
		if !(len(cfg.Entrypoint) == 1 && cfg.Entrypoint[0] == "") {
			s.entrypoint = cfg.Entrypoint
		}
	}
	var err error
	if s.stopSignal, err = parseStopSignal(cfg.StopSignal); err != nil {
		return s, nil, badRequest("%v", err)
	}
	if cfg.StopTimeout != nil && *cfg.StopTimeout < 0 {
		// Docker's -1 waits forever; runwhp's stop always ends.
		return s, nil, badRequest("StopTimeout %d: runwhp waits a whole number of seconds, 0 or more", *cfg.StopTimeout)
	}
	if h := cfg.Healthcheck; h != nil {
		ho := healthOverrides{test: h.Test, interval: h.Interval, timeout: h.Timeout, startPeriod: h.StartPeriod,
			startInterval: h.StartInterval, retries: h.Retries}
		if len(h.Test) == 1 && h.Test[0] == "NONE" {
			ho = healthOverrides{disable: true}
		}
		if err := ho.check(); err != nil {
			return s, nil, badRequest("%v", err)
		}
		s.health = ho
	}

	memory := ""
	if hc.Memory > 0 {
		memory = strconv.FormatInt(hc.Memory, 10)
	}
	if s.memory, err = parseMemory(memory); err != nil {
		return s, nil, badRequest("%v", err)
	}
	cpus := ""
	switch {
	case hc.NanoCPUs > 0:
		n := int(math.Ceil(float64(hc.NanoCPUs) / 1e9))
		if float64(n)*1e9 != float64(hc.NanoCPUs) {
			warn("--cpus %g rounded up to %d: runwhp gives a container whole vCPUs", float64(hc.NanoCPUs)/1e9, n)
		}
		cpus = strconv.Itoa(n)
	case hc.CPUCount > 0:
		cpus = strconv.FormatInt(hc.CPUCount, 10)
	}
	if s.cpus, err = parseCPUs(cpus, runtime.NumCPU()); err != nil {
		return s, nil, badRequest("%v", err)
	}

	if s.network, err = networkFromCreate(cfg, hc, req.NetworkingConfig); err != nil {
		return s, nil, err
	}
	if s.mounts, err = mountsFromCreate(cfg, hc); err != nil {
		return s, nil, err
	}

	var storage []string
	for k, v := range hc.StorageOpt {
		storage = append(storage, k+"="+v)
	}
	if s.storageSize, err = parseStorageOpts(storage); err != nil {
		return s, nil, badRequest("%v", err)
	}
	driver := hc.LogConfig.Type
	if driver == "" {
		// Docker's daemon's default, which the API reports runwhp's own
		// log as: it reads back as json-file's does.
		driver = container.LogDriverJSONFile
	}
	var logOpts []string
	for k, v := range hc.LogConfig.Config {
		logOpts = append(logOpts, k+"="+v)
	}
	sort.Strings(logOpts)
	if s.logs, err = parseLogOptions(driver, logOpts); err != nil {
		return s, nil, badRequest("%v", err)
	}
	if name := string(hc.RestartPolicy.Name); name != "" {
		policy := name
		if hc.RestartPolicy.MaximumRetryCount > 0 {
			policy += ":" + strconv.Itoa(hc.RestartPolicy.MaximumRetryCount)
		}
		if s.restart, err = container.ParseRestartPolicy(policy); err != nil {
			return s, nil, badRequest("%v", err)
		}
	}
	if s.restart.IsSet() && s.rm {
		return s, nil, badRequest("conflicting options: cannot specify both --restart and --rm")
	}

	if hc.Init != nil && *hc.Init {
		warn("--init: runwhp's own init is always the container's PID 1's parent, reaping what it leaves")
	}
	if hc.ShmSize != 0 {
		warn("--shm-size: /dev/shm is a tmpfs as large as the guest's memory allows")
	}
	if hc.OomScoreAdj != 0 {
		warn("--oom-score-adj: the container is the only tenant of its VM")
	}
	if len(hc.Ulimits) > 0 {
		warn("--ulimit is ignored: the guest kernel's limits apply")
	}
	if hc.MemorySwap != 0 && hc.MemorySwap != hc.Memory && hc.MemorySwap != -1 {
		warn("--memory-swap is ignored: the guest has no swap")
	}
	if hc.CPUShares != 0 || hc.BlkioWeight != 0 {
		warn("--cpu-shares and --blkio-weight are ignored: each container has a VM of its own")
	}
	if hc.CgroupnsMode != "" && hc.CgroupnsMode != "private" {
		warn("--cgroupns %s: the container's cgroup namespace is always its own", hc.CgroupnsMode)
	}
	if hc.ReadonlyRootfs {
		warn("--read-only is not supported yet: the container's root file system is writable")
	}
	return s, warnings, nil
}

// unconfinedSecurityOpt reports whether a --security-opt asks for nothing
// runwhp's containers do not have already: SELinux labels, and no seccomp
// profile, AppArmor profile or no_new_privileges.
func unconfinedSecurityOpt(o string) bool {
	if strings.HasPrefix(o, "label=") || strings.HasPrefix(o, "label:") {
		return true
	}
	switch strings.ReplaceAll(o, ":", "=") {
	case "seccomp=unconfined", "apparmor=unconfined", "no-new-privileges=false":
		return true
	}
	return false
}

// refuseUnsupported refuses settings runwhp cannot honour, each named as
// the docker CLI names it.
func refuseUnsupported(cfg *dcontainer.Config, hc *dcontainer.HostConfig) error {
	var refused []string
	add := func(cond bool, what string) {
		if cond {
			refused = append(refused, what)
		}
	}
	add(len(hc.CapAdd) > 0, "--cap-add")
	add(len(hc.Devices) > 0, "--device")
	add(len(hc.DeviceRequests) > 0, "--gpus")
	add(len(hc.DeviceCgroupRules) > 0, "--device-cgroup-rule")
	add(hc.PidMode != "" && hc.PidMode != "private", "--pid "+string(hc.PidMode))
	add(hc.IpcMode != "" && hc.IpcMode != "private" && hc.IpcMode != "shareable", "--ipc "+string(hc.IpcMode))
	add(hc.UTSMode != "", "--uts "+string(hc.UTSMode))
	add(hc.UsernsMode != "", "--userns "+string(hc.UsernsMode))
	add(len(hc.DNS) > 0, "--dns")
	add(len(hc.DNSSearch) > 0, "--dns-search")
	add(len(hc.DNSOptions) > 0, "--dns-option")
	add(len(hc.Links) > 0, "--link")
	add(len(hc.VolumesFrom) > 0, "--volumes-from")
	add(hc.VolumeDriver != "" && hc.VolumeDriver != "local", "--volume-driver "+hc.VolumeDriver)
	add(hc.CpusetCpus != "" || hc.CpusetMems != "", "--cpuset-cpus and --cpuset-mems")
	add(hc.CPUQuota != 0 || hc.CPUPeriod != 0, "--cpu-quota and --cpu-period")
	add(hc.CPURealtimePeriod != 0 || hc.CPURealtimeRuntime != 0, "--cpu-rt-period and --cpu-rt-runtime")
	add(len(hc.Sysctls) > 0, "--sysctl")
	for _, o := range hc.SecurityOpt {
		// SELinux labels, as Dev Containers' docker-outside-of-docker
		// asks label=disable: the guest has no SELinux, and Docker on a
		// host without it takes them and does nothing. Nor has the
		// container a seccomp profile or AppArmor, which tools that run
		// containers in containers ask be unconfined.
		if !unconfinedSecurityOpt(o) {
			add(true, "--security-opt "+o)
		}
	}
	add(hc.Runtime != "" && hc.Runtime != "runwhp" && hc.Runtime != "runc", "--runtime "+hc.Runtime)
	add(len(hc.GroupAdd) > 0, "--group-add")
	add(hc.CgroupParent != "", "--cgroup-parent")
	add(hc.PidsLimit != nil && *hc.PidsLimit > 0, "--pids-limit")
	add(hc.Isolation != "" && hc.Isolation != "default", "--isolation "+string(hc.Isolation))
	add(cfg.Domainname != "", "--domainname")
	if len(refused) == 0 {
		return nil
	}
	return badRequest("runwhp cannot run this container as asked: %s %s not supported", strings.Join(refused, ", "), pluralIs(len(refused)))
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// networkFromCreate reads a create request's network mode, endpoints,
// published ports and extra hosts as run reads --network, -p and
// --add-host.
func networkFromCreate(cfg *dcontainer.Config, hc *dcontainer.HostConfig, nc *dnetwork.NetworkingConfig) (networkRequest, error) {
	var f networkFlags
	mode := string(hc.NetworkMode)
	if cfg.NetworkDisabled {
		mode = "none"
	}
	switch {
	case mode == "host":
		return networkRequest{}, badRequest("--network host is not supported: each container has a VM, and a network, of its own")
	case strings.HasPrefix(mode, "container:"):
		return networkRequest{}, badRequest("--network %s is not supported: containers cannot share a VM's network", mode)
	}
	var endpoints map[string]*dnetwork.EndpointSettings
	if nc != nil {
		endpoints = nc.EndpointsConfig
	}
	names := make([]string, 0, len(endpoints)+1)
	if mode != "" && mode != "default" {
		names = append(names, mode)
	}
	for name := range endpoints {
		if !strings.EqualFold(name, mode) {
			names = append(names, name)
		}
	}
	sort.Strings(names[min(1, len(names)):])
	for _, name := range names {
		var want []string
		builtin := name == "none" || name == "bridge" || name == "default" || strings.EqualFold(name, network.User)
		want = append(want, "name="+name)
		if e := endpoints[name]; e != nil {
			if len(e.Links) > 0 {
				return networkRequest{}, badRequest("--link is not supported")
			}
			if e.MacAddress.String() != "" {
				return networkRequest{}, badRequest("--mac-address is not supported")
			}
			if e.IPAMConfig != nil {
				if e.IPAMConfig.IPv6Address.IsValid() || len(e.IPAMConfig.LinkLocalIPs) > 0 {
					return networkRequest{}, badRequest("--ip6 and --link-local-ip are not supported: the network is IPv4 only")
				}
				if e.IPAMConfig.IPv4Address.IsValid() {
					want = append(want, "ip="+e.IPAMConfig.IPv4Address.String())
				}
			}
			for _, a := range e.Aliases {
				if builtin {
					break
				}
				want = append(want, "alias="+a)
			}
		}
		if name == "bridge" || name == "default" {
			if len(want) == 1 {
				continue // runwhp's default network
			}
			want[0] = "name=" + network.User
		}
		if len(want) == 1 {
			f.modes = append(f.modes, name)
		} else {
			f.modes = append(f.modes, strings.Join(want, ","))
		}
	}
	for port, bindings := range hc.PortBindings {
		if port.Proto() != dnetwork.TCP {
			return networkRequest{}, badRequest("-p %s: only tcp ports can be published so far", port)
		}
		// A port with no bindings is only exposed, as Docker's daemon
		// takes it, unless -P publishes it below.
		for _, b := range bindings {
			host := "127.0.0.1"
			if b.HostIP.IsValid() {
				host = b.HostIP.String()
				if b.HostIP.Is6() {
					host = "[" + host + "]"
				}
			}
			// An empty host port, or 0, is a random one.
			f.publish = append(f.publish, fmt.Sprintf("%s:%s:%d", host, b.HostPort, port.Num()))
		}
		f.expose = append(f.expose, port.String())
	}
	if cfg != nil {
		for port := range cfg.ExposedPorts {
			f.expose = append(f.expose, port.String())
		}
	}
	f.publishAll = hc.PublishAllPorts
	sort.Strings(f.publish)
	f.addHosts = hc.ExtraHosts
	r, err := parseNetworkRequest(f)
	if err != nil {
		return r, badRequest("%v", err)
	}
	return r, nil
}

// mountsFromCreate reads Binds, Mounts, Tmpfs and the anonymous volumes
// Config.Volumes asks for, as run reads -v, --mount and --tmpfs, with no
// working directory to resolve relative paths against.
func mountsFromCreate(cfg *dcontainer.Config, hc *dcontainer.HostConfig) ([]mountSpec, error) {
	var specs []mountSpec
	targets := map[string]bool{}
	addSpec := func(m mountSpec) {
		specs = append(specs, m)
		targets[m.target] = true
	}
	for _, b := range hc.Binds {
		m, err := parseVolume(b, "")
		if err != nil {
			return nil, badRequest("%v", err)
		}
		addSpec(m)
	}
	for _, mt := range hc.Mounts {
		m, err := mountFromAPI(mt)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		addSpec(m)
	}
	tmpfs := make([]string, 0, len(hc.Tmpfs))
	for target := range hc.Tmpfs {
		tmpfs = append(tmpfs, target)
	}
	sort.Strings(tmpfs)
	for _, target := range tmpfs {
		spec := target
		if opts := hc.Tmpfs[target]; opts != "" {
			spec += ":" + opts
		}
		m, err := parseTmpfs(spec)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		addSpec(m)
	}
	anon := make([]string, 0, len(cfg.Volumes))
	for target := range cfg.Volumes {
		anon = append(anon, target)
	}
	sort.Strings(anon)
	for _, target := range anon {
		t, err := containerPath(target)
		if err != nil {
			return nil, badRequest("-v %s: %v", target, err)
		}
		if targets[t] {
			continue
		}
		addSpec(mountSpec{kind: guestspec.MountVolume, target: t})
	}
	return specs, nil
}

// mountFromAPI reads one of HostConfig.Mounts as --mount would.
func mountFromAPI(mt mount.Mount) (mountSpec, error) {
	what := fmt.Sprintf("--mount type=%s,target=%s", mt.Type, mt.Target)
	if mt.Target == "" {
		return mountSpec{}, fmt.Errorf("%s: target is required", what)
	}
	target, err := containerPath(mt.Target)
	if err != nil {
		return mountSpec{}, fmt.Errorf("%s: %w", what, err)
	}
	m := mountSpec{target: target, readOnly: mt.ReadOnly, delegated: mt.Consistency == mount.ConsistencyDelegated}
	switch mt.Type {
	case mount.TypeBind, mount.TypeNamedPipe:
		if mt.Source == "" {
			return mountSpec{}, fmt.Errorf("%s: source is required", what)
		}
		if pipe, ok := socketSource(mt.Source); ok {
			m.kind, m.source, m.pipe, m.delegated = guestspec.MountSocket, mt.Source, pipe, false
			break
		}
		if mt.Type == mount.TypeNamedPipe {
			return mountSpec{}, fmt.Errorf("%s: source %s is not a named pipe, such as //./pipe/docker_engine", what, mt.Source)
		}
		m.kind = guestspec.MountBind
		if m.source, err = hostPath(mt.Source, ""); err != nil {
			return mountSpec{}, fmt.Errorf("%s: %w", what, err)
		}
		if o := mt.BindOptions; o != nil {
			if o.Propagation != "" && o.Propagation != mount.PropagationPrivate && o.Propagation != mount.PropagationRPrivate {
				return mountSpec{}, fmt.Errorf("%s: bind-propagation=%s is not supported", what, o.Propagation)
			}
			m.create = o.CreateMountpoint
		}
	case mount.TypeVolume:
		m.kind = guestspec.MountVolume
		m.source = mt.Source
		if o := mt.VolumeOptions; o != nil {
			m.noCopy = o.NoCopy
			// Given to the volume if it is made now, as Docker gives them;
			// Testcontainers' Ryuk finds its volumes by them.
			m.labels = o.Labels
			if o.Subpath != "" {
				return mountSpec{}, fmt.Errorf("%s: volume-subpath is not supported", what)
			}
			if d := o.DriverConfig; d != nil {
				if d.Name != "" && d.Name != "local" {
					return mountSpec{}, fmt.Errorf("%s: volume-driver=%s is not supported", what, d.Name)
				}
				for k, v := range d.Options {
					switch k {
					case "size":
						m.size = v
					case "shared":
						shared, err := parseShared(v)
						if err != nil {
							return mountSpec{}, fmt.Errorf("%s: %v", what, err)
						}
						m.shared = shared
					default:
						return mountSpec{}, fmt.Errorf("%s: volume-opt=%s is not supported; runwhp knows volume-opt=size=SIZE and volume-opt=shared=true", what, k)
					}
				}
			}
		}
	case mount.TypeTmpfs:
		if mt.Source != "" {
			return mountSpec{}, fmt.Errorf("%s: a tmpfs has no source", what)
		}
		m.kind = guestspec.MountTmpfs
		var opts []string
		if o := mt.TmpfsOptions; o != nil {
			if o.SizeBytes > 0 {
				opts = append(opts, fmt.Sprintf("size=%d", o.SizeBytes))
			}
			if o.Mode != 0 {
				opts = append(opts, fmt.Sprintf("mode=%o", o.Mode.Perm()|o.Mode&(1<<9)))
			}
			for _, kv := range o.Options {
				opts = append(opts, strings.Join(kv, "="))
			}
		}
		if m.options, err = tmpfsOptions(opts, mt.ReadOnly); err != nil {
			return mountSpec{}, fmt.Errorf("%s: %w", what, err)
		}
	default:
		return mountSpec{}, fmt.Errorf("%s: type %s is not supported; use bind, volume, tmpfs or npipe", what, mt.Type)
	}
	if m.kind == guestspec.MountVolume && m.source != "" && path.IsAbs(m.source) {
		return mountSpec{}, fmt.Errorf("%s: %q is not a volume name", what, m.source)
	}
	return m, nil
}
