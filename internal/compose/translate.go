package compose

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// Container is what runwhp makes for one of a service's containers, in
// terms of run's flags, before anything is looked up in the store.
type Container struct {
	Service string
	Image   string
	// Entrypoint replaces the image's when EntrypointSet; Command its Cmd
	// when not nil.
	Entrypoint    []string
	EntrypointSet bool
	Command       []string
	Env           []string // KEY=VALUE, sorted
	WorkingDir    string
	User          string
	Hostname      string
	TTY           bool
	StdinOpen     bool

	Memory int64 // bytes; 0 is runwhp's default
	CPUs   int   // 0 is runwhp's default

	// NoNetwork is network_mode: none; DefaultNetwork network_mode:
	// bridge, runwhp's default network. Otherwise the container is on
	// Networks.
	NoNetwork      bool
	DefaultNetwork bool
	Networks       []Attachment
	Ports          []Port
	Expose         []string // as --expose takes them
	ExtraHosts     []string // NAME:IP, as --add-host takes them
	Mounts         []Mount

	Restart     string // as --restart takes it
	Health      *Health
	StopSignal  string
	StopTimeout *int // seconds
	StorageSize string
	Labels      map[string]string // the service's own
	LogDriver   string            // local or none
	LogOpts     []string          // KEY=VALUE, as --log-opt takes them
	PullPolicy  string            // missing, always, never or build
	Replicas    int
	Attach      bool // up follows its output
}

// Attachment is a network a container joins: its name in the store, and
// the container's aliases and address there.
type Attachment struct {
	Name    string
	Aliases []string
	IPv4    string
}

// Port is a published port.
type Port struct {
	HostIP   string // empty: runwhp's default, 127.0.0.1
	HostPort uint16 // 0: any free port, as "80" alone asks
	Target   uint16
}

// The kinds of Mount.
const (
	MountBind   = "bind"
	MountVolume = "volume" // named; anonymous without a source
	MountTmpfs  = "tmpfs"
)

// Mount is a bind mount, a volume or a tmpfs.
type Mount struct {
	Kind      string
	Source    string // bind: an absolute host path; volume: its name in the store
	Target    string
	ReadOnly  bool
	Create    bool // bind: make a missing source directory
	Delegated bool // bind: consistency: delegated
	NoCopy    bool
	Options   string // tmpfs: size=...,mode=...
}

// Health is a service's healthcheck, over its image's.
type Health struct {
	Test                                          []string
	Interval, Timeout, StartPeriod, StartInterval time.Duration
	Retries                                       int
	Disable                                       bool
}

// Problems collects what is wrong with a project, and what runwhp leaves
// out of it.
type Problems struct {
	Errors   []error
	Warnings []string
}

func (p *Problems) refuse(format string, a ...any) {
	p.Errors = append(p.Errors, fmt.Errorf(format, a...))
}
func (p *Problems) warn(format string, a ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, a...))
}

// Err is the project's errors together, or nil.
func (p *Problems) Err() error { return errors.Join(p.Errors...) }

// Translate turns a service into the container runwhp makes for it,
// noting in pr what runwhp cannot do as the service asks.
func Translate(p *types.Project, s types.ServiceConfig, pr *Problems) Container {
	c := Container{
		Service: s.Name, Image: ImageName(p.Name, s),
		Entrypoint: s.Entrypoint, EntrypointSet: s.Entrypoint != nil, Command: s.Command,
		WorkingDir: s.WorkingDir, User: s.User, Hostname: s.Hostname, TTY: s.Tty, StdinOpen: s.StdinOpen,
		Labels: maps.Clone(map[string]string(s.Labels)), Replicas: s.GetScale(), StopSignal: s.StopSignal,
		Attach: s.Attach == nil || *s.Attach,
	}
	for _, k := range sortedKeys(s.Environment) {
		if v := s.Environment[k]; v != nil {
			c.Env = append(c.Env, k+"="+*v)
		}
	}
	refuse := func(key, why string) { pr.refuse("service %q: %s is not supported: %s", s.Name, key, why) }
	ignore := func(key string) {
		pr.warn("service %q: %s is ignored: runwhp's containers do not have it", s.Name, key)
	}

	// What runwhp cannot do, and the service needs done.
	switch m := s.NetworkMode; {
	case m == "" || m == "default":
	case m == "none":
		c.NoNetwork = true
	case m == "bridge":
		c.DefaultNetwork = true
	default:
		refuse("network_mode "+m, "every container has a VM, and a network, of its own")
	}
	if len(s.Devices) > 0 || len(s.DeviceCgroupRules) > 0 {
		refuse("devices", "a container's VM has no devices of the host's")
	}
	if len(s.Gpus) > 0 {
		refuse("gpus", "a container's VM has no GPU")
	}
	if len(s.VolumesFrom) > 0 {
		refuse("volumes_from", "a container's volumes are its VM's")
	}
	if s.Ipc != "" && s.Ipc != "private" && s.Ipc != "shareable" {
		refuse("ipc "+s.Ipc, "containers' VMs share no memory")
	}
	if s.Pid != "" {
		refuse("pid "+s.Pid, "containers' VMs share no processes")
	}
	if s.Uts != "" {
		refuse("uts "+s.Uts, "a container's VM has a host name of its own")
	}
	if s.Provider != nil {
		refuse("provider", "runwhp runs containers only")
	}
	if len(s.Models) > 0 {
		refuse("models", "runwhp runs containers only")
	}
	if len(s.PreStart) > 0 || len(s.PostStart) > 0 || len(s.PreStop) > 0 {
		refuse("lifecycle hooks", "not implemented")
	}
	if s.UseAPISocket {
		refuse("use_api_socket", "runwhp has no Docker API")
	}
	if pl := strings.ToLower(s.Platform); pl != "" && pl != "linux/amd64" && pl != "linux/x86_64" {
		refuse("platform "+s.Platform, "runwhp runs linux/amd64 only")
	}

	// What runwhp leaves out, and the service may do without.
	for _, f := range []struct {
		key string
		set bool
	}{
		{"privileged", s.Privileged}, {"cap_add", len(s.CapAdd) > 0}, {"cap_drop", len(s.CapDrop) > 0},
		{"security_opt", len(s.SecurityOpt) > 0}, {"ulimits", len(s.Ulimits) > 0}, {"sysctls", len(s.Sysctls) > 0},
		{"shm_size", s.ShmSize != 0}, {"init", s.Init != nil && *s.Init}, {"userns_mode", s.UserNSMode != ""},
		{"oom_kill_disable", s.OomKillDisable}, {"oom_score_adj", s.OomScoreAdj != 0}, {"pids_limit", s.PidsLimit != 0},
		{"cpu_shares", s.CPUShares != 0}, {"cpuset", s.CPUSet != ""}, {"cpu_quota", s.CPUQuota != 0}, {"cpu_period", s.CPUPeriod != 0},
		{"cpu_count", s.CPUCount != 0}, {"cpu_percent", s.CPUPercent != 0}, {"cpu_rt_period", s.CPURTPeriod != 0}, {"cpu_rt_runtime", s.CPURTRuntime != 0},
		{"blkio_config", s.BlkioConfig != nil}, {"mem_reservation", s.MemReservation != 0}, {"memswap_limit", s.MemSwapLimit != 0},
		{"mem_swappiness", s.MemSwappiness != 0}, {"cgroup_parent", s.CgroupParent != ""}, {"cgroup", s.Cgroup != ""},
		{"group_add", len(s.GroupAdd) > 0}, {"isolation", s.Isolation != ""}, {"read_only", s.ReadOnly},
		{"dns", len(s.DNS) > 0}, {"dns_search", len(s.DNSSearch) > 0}, {"dns_opt", len(s.DNSOpts) > 0},
		{"domainname", s.DomainName != ""}, {"mac_address", s.MacAddress != ""}, {"external_links", len(s.ExternalLinks) > 0},
		{"credential_spec", s.CredentialSpec != nil}, {"runtime", s.Runtime != ""}, {"umask", s.Umask != nil},
		{"develop", s.Develop != nil},
	} {
		if f.set {
			ignore(f.key)
		}
	}

	c.Memory = int64(s.MemLimit)
	cpus := float64(s.CPUS)
	if d := s.Deploy; d != nil {
		if d.Resources.Limits != nil {
			if c.Memory == 0 {
				c.Memory = int64(d.Resources.Limits.MemoryBytes)
			}
			if cpus == 0 {
				cpus = float64(d.Resources.Limits.NanoCPUs)
			}
		}
		if d.Resources.Reservations != nil {
			ignore("deploy.resources.reservations")
		}
		if d.Mode == "global" {
			pr.warn("service %q: deploy.mode global runs one container, as replicated does with one replica", s.Name)
		}
	}
	if cpus > 0 {
		c.CPUs = int(math.Ceil(cpus))
		if float64(c.CPUs) != cpus {
			pr.warn("service %q: cpus %g is %d here: runwhp gives a container whole vCPUs", s.Name, cpus, c.CPUs)
		}
	}
	if size, ok := s.StorageOpt["size"]; ok {
		c.StorageSize = size
	}
	if len(s.StorageOpt) > 1 || len(s.StorageOpt) == 1 && c.StorageSize == "" {
		ignore("storage_opt other than size")
	}

	c.Restart = restartPolicy(s, pr)
	if s.StopGracePeriod != nil {
		secs := int(math.Ceil(time.Duration(*s.StopGracePeriod).Seconds()))
		c.StopTimeout = &secs
	}
	if h := s.HealthCheck; h != nil {
		c.Health = &Health{Test: h.Test, Disable: h.Disable}
		for _, d := range []struct {
			from *types.Duration
			to   *time.Duration
		}{{h.Interval, &c.Health.Interval}, {h.Timeout, &c.Health.Timeout}, {h.StartPeriod, &c.Health.StartPeriod}, {h.StartInterval, &c.Health.StartInterval}} {
			if d.from != nil {
				*d.to = time.Duration(*d.from)
			}
		}
		if h.Retries != nil {
			c.Health.Retries = int(*h.Retries)
		}
	}
	c.LogDriver, c.LogOpts = logging(s, pr)
	c.PullPolicy = pullPolicy(s, pr)

	networks(p, s, &c, pr)
	for _, port := range s.Ports {
		pt, err := publish(port)
		if err != nil {
			pr.refuse("service %q: %v", s.Name, err)
			continue
		}
		c.Ports = append(c.Ports, pt)
	}
	c.Expose = append(c.Expose, s.Expose...)
	for _, name := range sortedKeys(s.ExtraHosts) {
		for _, ip := range s.ExtraHosts[name] {
			c.ExtraHosts = append(c.ExtraHosts, name+":"+ip)
		}
	}
	mounts(p, s, &c, pr)
	return c
}

// restartPolicy is a service's restart policy as --restart takes it:
// restart's, or deploy.restart_policy's.
func restartPolicy(s types.ServiceConfig, pr *Problems) string {
	if s.Restart != "" {
		return s.Restart
	}
	if s.Deploy == nil || s.Deploy.RestartPolicy == nil {
		return "no"
	}
	rp := s.Deploy.RestartPolicy
	if rp.Delay != nil || rp.Window != nil {
		pr.warn("service %q: deploy.restart_policy's delay and window are ignored: runwhp waits as Docker's restart policies do", s.Name)
	}
	switch rp.Condition {
	case "none":
		return "no"
	case "on-failure":
		if rp.MaxAttempts != nil {
			return fmt.Sprintf("on-failure:%d", *rp.MaxAttempts)
		}
		return "on-failure"
	}
	return "always"
}

// logging is a service's log driver and options, as runwhp's --log-driver
// and --log-opt take them.
func logging(s types.ServiceConfig, pr *Problems) (string, []string) {
	driver, opts := s.LogDriver, s.LogOpt
	if s.Logging != nil {
		driver, opts = cmp.Or(s.Logging.Driver, driver), s.Logging.Options
	}
	switch driver {
	case "", "local", "json-file":
	case "none":
		return "none", nil
	default:
		pr.warn("service %q: logging driver %s is runwhp's local log here", s.Name, driver)
	}
	var out []string
	for _, k := range sortedKeys(opts) {
		switch k {
		case "max-size", "max-file":
			out = append(out, k+"="+opts[k])
		default:
			pr.warn("service %q: log option %s is ignored", s.Name, k)
		}
	}
	return "local", out
}

// pullPolicy is when to pull or build a service's image.
func pullPolicy(s types.ServiceConfig, pr *Problems) string {
	switch p := s.PullPolicy; {
	case p == "" || p == "missing" || p == "if_not_present":
		return "missing"
	case p == "always" || p == "never" || p == "build":
		return p
	default:
		pr.warn("service %q: pull_policy %s pulls a missing image only, here", s.Name, p)
		return "missing"
	}
}

// networks fills a container's networks: each with the service's name as
// an alias, as Docker Compose gives it, and the aliases links ask for.
func networks(p *types.Project, s types.ServiceConfig, c *Container, pr *Problems) {
	if c.NoNetwork || c.DefaultNetwork {
		return
	}
	var linkAliases []string
	for _, other := range sortedKeys(p.Services) {
		for _, l := range p.Services[other].Links {
			target, alias, _ := strings.Cut(l, ":")
			if target == s.Name && alias != "" && alias != s.Name && !slices.Contains(linkAliases, alias) {
				linkAliases = append(linkAliases, alias)
			}
		}
	}
	for _, key := range sortedKeys(s.Networks) {
		n, ok := p.Networks[key]
		if !ok {
			pr.refuse("service %q refers to undefined network %s", s.Name, key)
			continue
		}
		a := Attachment{Name: cmp.Or(n.Name, p.Name+"_"+key)}
		sn := s.Networks[key]
		if sn == nil || sn.UseServiceNameAlias == nil || *sn.UseServiceNameAlias {
			a.Aliases = append(a.Aliases, s.Name)
		}
		if sn != nil {
			for _, al := range sn.Aliases {
				if !slices.Contains(a.Aliases, al) {
					a.Aliases = append(a.Aliases, al)
				}
			}
			a.IPv4 = sn.Ipv4Address
			if sn.Ipv6Address != "" || len(sn.LinkLocalIPs) > 0 {
				pr.refuse("service %q: network %s: IPv6 and link-local addresses are not supported: runwhp's networks are IPv4", s.Name, key)
			}
			if sn.MacAddress != "" || len(sn.DriverOpts) > 0 || sn.InterfaceName != "" || sn.Priority != 0 || sn.GatewayPriority != 0 {
				pr.warn("service %q: network %s: mac_address, driver_opts, interface_name and priorities are ignored", s.Name, key)
			}
		}
		for _, al := range linkAliases {
			if !slices.Contains(a.Aliases, al) {
				a.Aliases = append(a.Aliases, al)
			}
		}
		c.Networks = append(c.Networks, a)
	}
}

// publish turns a published port into runwhp's: TCP, on the host port
// given, or any free one.
func publish(p types.ServicePortConfig) (Port, error) {
	if p.Protocol != "" && p.Protocol != "tcp" {
		return Port{}, fmt.Errorf("port %d/%s cannot be published: runwhp publishes TCP ports only", p.Target, p.Protocol)
	}
	var hp uint64
	if p.Published != "" {
		var err error
		if hp, err = strconv.ParseUint(p.Published, 10, 16); err != nil {
			return Port{}, fmt.Errorf("port %d: host port %q is not supported: give one port, not a range", p.Target, p.Published)
		}
	}
	if p.Target == 0 || p.Target > math.MaxUint16 {
		return Port{}, fmt.Errorf("port %d is not a port", p.Target)
	}
	return Port{HostIP: p.HostIP, HostPort: uint16(hp), Target: uint16(p.Target)}, nil
}

// mounts fills a container's mounts: volumes, tmpfs, secrets and configs.
func mounts(p *types.Project, s types.ServiceConfig, c *Container, pr *Problems) {
	for _, v := range s.Volumes {
		m := Mount{Target: v.Target, ReadOnly: v.ReadOnly}
		switch v.Type {
		case types.VolumeTypeBind:
			m.Kind, m.Source, m.Create = MountBind, v.Source, true
			if v.Bind != nil {
				m.Create = bool(v.Bind.CreateHostPath) // an OptOut: true unless create_host_path: false
				if v.Bind.Propagation != "" || v.Bind.SELinux != "" {
					pr.warn("service %q: %s: bind propagation and SELinux labels are ignored", s.Name, v.Target)
				}
			}
			m.Delegated = v.Consistency == "delegated"
		case types.VolumeTypeVolume:
			m.Kind = MountVolume
			if v.Source != "" {
				vc, ok := p.Volumes[v.Source]
				if !ok {
					pr.refuse("service %q refers to undefined volume %s", s.Name, v.Source)
					continue
				}
				m.Source = cmp.Or(vc.Name, p.Name+"_"+v.Source)
			}
			if v.Volume != nil {
				m.NoCopy = v.Volume.NoCopy
				if v.Volume.Subpath != "" {
					pr.refuse("service %q: %s: volume subpath is not supported", s.Name, v.Target)
				}
			}
		case types.VolumeTypeTmpfs:
			m.Kind = MountTmpfs
			var opts []string
			if v.Tmpfs != nil {
				if v.Tmpfs.Size != 0 {
					opts = append(opts, fmt.Sprintf("size=%d", v.Tmpfs.Size))
				}
				if v.Tmpfs.Mode != 0 {
					opts = append(opts, fmt.Sprintf("mode=%o", v.Tmpfs.Mode))
				}
			}
			m.Options = strings.Join(opts, ",")
		default:
			pr.refuse("service %q: %s: volumes of type %s are not supported", s.Name, v.Target, v.Type)
			continue
		}
		c.Mounts = append(c.Mounts, m)
	}
	for _, t := range s.Tmpfs {
		target, opts, _ := strings.Cut(t, ":")
		c.Mounts = append(c.Mounts, Mount{Kind: MountTmpfs, Target: target, Options: opts})
	}
	for _, sec := range s.Secrets {
		file(s, c, pr, "secret", types.FileReferenceConfig(sec), types.FileObjectConfig(p.Secrets[sec.Source]), "/run/secrets/")
	}
	for _, cf := range s.Configs {
		file(s, c, pr, "config", types.FileReferenceConfig(cf), types.FileObjectConfig(p.Configs[cf.Source]), "/")
	}
}

// file mounts a secret or a config from a file, read-only, as Docker
// Compose binds one.
func file(s types.ServiceConfig, c *Container, pr *Problems, kind string, ref types.FileReferenceConfig, obj types.FileObjectConfig, dir string) {
	if obj.File == "" {
		pr.refuse("service %q: %s %s: only %ss from a file are supported", s.Name, kind, ref.Source, kind)
		return
	}
	target := ref.Target
	switch {
	case target == "":
		target = dir + ref.Source
	case !path.IsAbs(target):
		target = dir + target
	}
	if ref.UID != "" || ref.GID != "" || ref.Mode != nil {
		pr.warn("service %q: %s %s: uid, gid and mode are ignored", s.Name, kind, ref.Source)
	}
	c.Mounts = append(c.Mounts, Mount{Kind: MountBind, Source: obj.File, Target: target, ReadOnly: true})
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	return slices.Sorted(maps.Keys(m))
}

// Build is how to build a service's image, in terms of build's flags.
type Build struct {
	Context    string // an absolute directory
	Dockerfile string // an absolute path, or empty with Inline
	Inline     string // dockerfile_inline
	Args       map[string]string
	Target     string
	NoCache    bool
	Pull       bool
	NoNetwork  bool
	Labels     map[string]string
	Hosts      []string          // NAME:IP
	Secrets    map[string]string // id, and the file that holds it
	Tags       []string          // the image's names: the service's image first
}

// TranslateBuild is how to build a service's image, or nil if it has no
// build section.
func TranslateBuild(p *types.Project, s types.ServiceConfig, pr *Problems) *Build {
	bc := s.Build
	if bc == nil {
		return nil
	}
	b := &Build{Context: bc.Context, Inline: bc.DockerfileInline, Target: bc.Target, NoCache: bc.NoCache, Pull: bc.Pull,
		NoNetwork: bc.Network == "none", Labels: maps.Clone(map[string]string(bc.Labels)), Args: map[string]string{}, Secrets: map[string]string{}}
	refuse := func(what string) { pr.refuse("service %q: build %s is not supported", s.Name, what) }
	if strings.Contains(bc.Context, "://") || strings.HasPrefix(bc.Context, "git@") || strings.HasSuffix(bc.Context, ".git") {
		refuse("from a git or URL context")
	}
	if len(bc.SSH) > 0 {
		refuse("ssh")
	}
	if len(bc.AdditionalContexts) > 0 {
		refuse("additional_contexts")
	}
	if len(bc.CacheTo) > 0 {
		refuse("cache_to")
	}
	for _, pl := range bc.Platforms {
		if pl = strings.ToLower(pl); pl != "linux/amd64" && pl != "linux/x86_64" {
			refuse("platform " + pl)
		}
	}
	if n := bc.Network; n != "" && n != "default" && n != "host" && n != "none" {
		refuse("network " + n)
	}
	for _, f := range []struct {
		key string
		set bool
	}{{"cache_from", len(bc.CacheFrom) > 0}, {"entitlements", len(bc.Entitlements) > 0}, {"privileged", bc.Privileged},
		{"shm_size", bc.ShmSize != 0}, {"ulimits", len(bc.Ulimits) > 0}, {"isolation", bc.Isolation != ""},
		{"provenance", bc.Provenance != ""}, {"sbom", bc.SBOM != ""}, {"no_cache_filter", len(bc.NoCacheFilter) > 0}} {
		if f.set {
			pr.warn("service %q: build %s is ignored", s.Name, f.key)
		}
	}
	if b.Inline == "" {
		b.Dockerfile = cmp.Or(bc.Dockerfile, "Dockerfile")
		if !filepath.IsAbs(b.Dockerfile) {
			b.Dockerfile = filepath.Join(b.Context, b.Dockerfile)
		}
	}
	for _, k := range sortedKeys(bc.Args) {
		if v := bc.Args[k]; v != nil {
			b.Args[k] = *v
		}
	}
	for _, name := range sortedKeys(bc.ExtraHosts) {
		for _, ip := range bc.ExtraHosts[name] {
			b.Hosts = append(b.Hosts, name+":"+ip)
		}
	}
	for _, sec := range bc.Secrets {
		obj := types.FileObjectConfig(p.Secrets[sec.Source])
		if obj.File == "" {
			pr.refuse("service %q: build secret %s: only secrets from a file are supported", s.Name, sec.Source)
			continue
		}
		b.Secrets[cmp.Or(sec.Target, sec.Source)] = obj.File
	}
	b.Tags = append([]string{ImageName(p.Name, s)}, bc.Tags...)
	return b
}
