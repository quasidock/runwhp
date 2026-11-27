//go:build windows

package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/volume"
)

// mountSpec is one mount from -v, --mount or --tmpfs.
type mountSpec struct {
	kind   string // guestspec.MountBind, MountVolume (anonymous when source is empty), MountTmpfs or MountSocket
	source string // bind: absolute host path; volume: its name; socket: as given
	// pipe is a socket's named pipe on the host; empty, for Docker's
	// socket, the Engine API's, which planMounts is given.
	pipe     string
	target   string // clean absolute container path
	readOnly bool
	create   bool // bind: create a missing source directory, as -v does
	// delegated is set for a bind mount whose writes the guest may cache:
	// consistency=delegated, as Docker Desktop for Mac named it.
	delegated bool
	noCopy    bool              // volume: leave a new volume empty
	size      string            // volume: its size, should it be created
	shared    bool              // volume: make it shared
	labels    map[string]string // volume: its labels, should it be created
	options   string            // tmpfs: mount options
}

// parseVolume reads -v SOURCE:CONTAINERPATH[:OPTIONS], where SOURCE is a
// host path or a volume name, or -v CONTAINERPATH alone for an anonymous
// volume. The colon of a drive letter belongs to SOURCE.
func parseVolume(s, cwd string) (mountSpec, error) {
	if strings.HasPrefix(s, "/") && !strings.Contains(s, ":") {
		target, err := containerPath(s)
		if err != nil {
			return mountSpec{}, fmt.Errorf("-v %s: %w", s, err)
		}
		return mountSpec{kind: guestspec.MountVolume, target: target}, nil
	}
	usage := fmt.Errorf("-v %s: want HOSTPATH:CONTAINERPATH[:ro], VOLUME:CONTAINERPATH[:ro] or CONTAINERPATH", s)
	skip := 0
	if len(s) >= 2 && s[1] == ':' && isLetter(s[0]) {
		skip = 2
	}
	i := strings.IndexByte(s[skip:], ':')
	if i < 0 {
		return mountSpec{}, usage
	}
	src, rest := s[:skip+i], s[skip+i+1:]
	dst, opts, _ := strings.Cut(rest, ":")
	m := mountSpec{kind: guestspec.MountBind, create: true}
	var err error
	if pipe, ok := socketSource(src); ok {
		m = mountSpec{kind: guestspec.MountSocket, source: src, pipe: pipe}
	} else if isVolumeName(src) {
		if err := volume.CheckName(src); err != nil {
			return mountSpec{}, fmt.Errorf("-v %s: %w", s, err)
		}
		m = mountSpec{kind: guestspec.MountVolume, source: src}
	} else if m.source, err = hostPath(src, cwd); err != nil {
		return mountSpec{}, fmt.Errorf("-v %s: %w", s, err)
	}
	if m.target, err = containerPath(dst); err != nil {
		return mountSpec{}, fmt.Errorf("-v %s: %w", s, err)
	}
	if opts == "" {
		return m, nil
	}
	for _, o := range strings.Split(opts, ",") {
		switch {
		case o == "ro" || o == "readonly":
			m.readOnly = true
		case o == "rw":
			m.readOnly = false
		case o == "nocopy" && m.kind == guestspec.MountVolume:
			m.noCopy = true
		case o == "delegated":
			m.delegated = true
		case o == "z", o == "Z", o == "consistent", o == "cached", o == "private", o == "rprivate":
			// SELinux labels, the consistency every bind mount here has
			// unless delegated, and the propagation every mount here has.
		default:
			return mountSpec{}, fmt.Errorf("-v %s: option %q is not supported", s, o)
		}
	}
	return m, nil
}

// isVolumeName tells a volume name from a host path: a name has no path
// separators, no drive and no leading dot.
func isVolumeName(s string) bool {
	return s != "" && !strings.ContainsAny(s, `/\:`) && s[0] != '.'
}

// parseMount reads --mount, Docker's long form of -v and --tmpfs:
// type=bind,source=HOSTPATH,target=PATH[,readonly][,consistency=delegated], type=volume[,source=NAME],
// target=PATH[,readonly][,volume-nocopy][,volume-opt=size=SIZE], and
// type=tmpfs,target=PATH[,tmpfs-size=SIZE][,tmpfs-mode=MODE].
func parseMount(s, cwd string) (mountSpec, error) {
	fields, err := csv.NewReader(strings.NewReader(s)).Read()
	if err != nil {
		return mountSpec{}, fmt.Errorf("--mount %s: %w", s, err)
	}
	bad := func(format string, a ...any) (mountSpec, error) {
		return mountSpec{}, fmt.Errorf("--mount %s: %s", s, fmt.Sprintf(format, a...))
	}
	var m mountSpec
	var typ, src, dst string
	var tmpfsOpts []string
	only := map[string]string{} // option -> the type it belongs to
	for _, f := range fields {
		key, val, hasVal := strings.Cut(f, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		boolVal := func() (bool, error) {
			if !hasVal {
				return true, nil
			}
			b, err := strconv.ParseBool(val)
			if err != nil {
				return false, fmt.Errorf("--mount %s: %s=%q is not a boolean", s, key, val)
			}
			return b, nil
		}
		switch key {
		case "type":
			typ = val
		case "source", "src":
			src = val
		case "target", "destination", "dst":
			dst = val
		case "readonly", "ro":
			if m.readOnly, err = boolVal(); err != nil {
				return mountSpec{}, err
			}
		case "consistency":
			switch val {
			case "delegated":
				m.delegated = true
			case "consistent", "cached", "default":
			default:
				return bad("consistency=%s is not supported; want consistent, cached, delegated or default", val)
			}
		case "bind-propagation":
			only[key] = "bind"
			if val != "private" && val != "rprivate" {
				return bad("bind-propagation=%s is not supported", val)
			}
		case "volume-nocopy":
			only[key] = "volume"
			if m.noCopy, err = boolVal(); err != nil {
				return mountSpec{}, err
			}
		case "volume-opt":
			only[key] = "volume"
			k, v, _ := strings.Cut(val, "=")
			switch k {
			case "size":
				if _, err := parseDiskSize(v); err != nil {
					return bad("%v", err)
				}
				m.size = v
			case "shared":
				if m.shared, err = parseShared(v); err != nil {
					return bad("%v", err)
				}
			default:
				return bad("volume-opt=%s is not supported; runwhp knows volume-opt=size=SIZE and volume-opt=shared=true", val)
			}
		case "volume-driver":
			only[key] = "volume"
			if val != "local" {
				return bad("volume-driver=%s is not supported", val)
			}
		case "tmpfs-size":
			only[key] = "tmpfs"
			n, err := parseSize(val)
			if err != nil {
				return bad("tmpfs-size: %v", err)
			}
			tmpfsOpts = append(tmpfsOpts, fmt.Sprintf("size=%d", n))
		case "tmpfs-mode":
			only[key] = "tmpfs"
			if !octalMode.MatchString(val) {
				return bad("tmpfs-mode=%s is not an octal mode", val)
			}
			tmpfsOpts = append(tmpfsOpts, "mode="+val)
		default:
			return bad("unknown option %q", key)
		}
	}
	for key, want := range only {
		if typ != want {
			return bad("%s applies only to type=%s", key, want)
		}
	}
	if dst == "" {
		return bad("target is required")
	}
	if m.target, err = containerPath(dst); err != nil {
		return bad("%v", err)
	}
	switch typ {
	case "bind", "npipe":
		if src == "" {
			return bad("source is required")
		}
		if pipe, ok := socketSource(src); ok {
			m.kind, m.source, m.pipe = guestspec.MountSocket, src, pipe
			break
		}
		if typ == "npipe" {
			return bad("source %s is not a named pipe, such as //./pipe/docker_engine", src)
		}
		m.kind = guestspec.MountBind
		if m.source, err = hostPath(src, cwd); err != nil {
			return bad("%v", err)
		}
	case "volume":
		m.kind = guestspec.MountVolume
		if src != "" {
			if err := volume.CheckName(src); err != nil {
				return bad("%v", err)
			}
			m.source = src
		}
	case "tmpfs":
		if src != "" {
			return bad("a tmpfs has no source")
		}
		m.kind = guestspec.MountTmpfs
		if m.options, err = tmpfsOptions(tmpfsOpts, m.readOnly); err != nil {
			return bad("%v", err)
		}
	case "":
		return bad("type is required: bind, volume or tmpfs")
	default:
		return bad("type=%s is not supported; use bind, volume, tmpfs or npipe", typ)
	}
	return m, nil
}

// parseTmpfs reads --tmpfs CONTAINERPATH[:OPTIONS].
func parseTmpfs(s string) (mountSpec, error) {
	dst, opts, _ := strings.Cut(s, ":")
	target, err := containerPath(dst)
	if err != nil {
		return mountSpec{}, fmt.Errorf("--tmpfs %s: %w", s, err)
	}
	var list []string
	if opts != "" {
		list = strings.Split(opts, ",")
	}
	options, err := tmpfsOptions(list, false)
	if err != nil {
		return mountSpec{}, fmt.Errorf("--tmpfs %s: %w", s, err)
	}
	return mountSpec{kind: guestspec.MountTmpfs, target: target, options: options, readOnly: strings.Contains(","+options+",", ",ro,")}, nil
}

var (
	octalMode = regexp.MustCompile(`^[0-7]{3,4}$`)
	tmpfsSize = regexp.MustCompile(`^[0-9]+[kmgKMG%]?$`)
	tmpfsNum  = regexp.MustCompile(`^[0-9]+[kmgKMG]?$`)
)

// tmpfsOptions checks a tmpfs's options and adds Docker's defaults: noexec,
// nosuid and nodev unless the options say otherwise, and mode 1777.
func tmpfsOptions(opts []string, readOnly bool) (string, error) {
	flags := map[string]bool{"exec": false, "suid": false, "dev": false}
	var data []string
	mode := "1777"
	for _, o := range opts {
		k, v, _ := strings.Cut(strings.TrimSpace(o), "=")
		switch k {
		case "exec", "suid", "dev":
			flags[k] = true
		case "noexec", "nosuid", "nodev":
			flags[k[2:]] = false
		case "ro":
			readOnly = true
		case "rw":
			readOnly = false
		case "size":
			if !tmpfsSize.MatchString(v) {
				return "", fmt.Errorf("size=%s: give bytes, with k, m or g, or a percentage of memory", v)
			}
			data = append(data, o)
		case "nr_inodes":
			if !tmpfsNum.MatchString(v) {
				return "", fmt.Errorf("nr_inodes=%s is not a number", v)
			}
			data = append(data, o)
		case "uid", "gid":
			if _, err := strconv.ParseUint(v, 10, 32); err != nil {
				return "", fmt.Errorf("%s=%s is not a number", k, v)
			}
			data = append(data, o)
		case "mode":
			if !octalMode.MatchString(v) {
				return "", fmt.Errorf("mode=%s is not an octal mode", v)
			}
			mode = v
		default:
			return "", fmt.Errorf("tmpfs option %q is not supported", o)
		}
	}
	var out []string
	for _, f := range []string{"exec", "suid", "dev"} {
		if !flags[f] {
			out = append(out, "no"+f)
		}
	}
	if readOnly {
		out = append(out, "ro")
	}
	out = append(out, "mode="+mode)
	return strings.Join(append(out, data...), ","), nil
}

func isLetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// hostPath makes a host path absolute. It takes Windows paths, absolute or
// starting with a dot, and Git Bash's /c/... form.
func hostPath(p, cwd string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("the host path is empty")
	case len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/'),
		strings.HasPrefix(p, `\\`), strings.HasPrefix(p, "//"):
		return filepath.Clean(p), nil
	case p == "." || p == ".." || strings.HasPrefix(p, `.\`) || strings.HasPrefix(p, "./") ||
		strings.HasPrefix(p, `..\`) || strings.HasPrefix(p, "../"):
		if cwd == "" {
			// The Engine API's, which has no working directory of the
			// client's to resolve it against.
			return "", fmt.Errorf("host path %s is relative: give an absolute Windows path, such as C:\\src", p)
		}
		return filepath.Join(cwd, p), nil
	case len(p) >= 2 && p[0] == '/' && isLetter(p[1]) && (len(p) == 2 || p[2] == '/'):
		return filepath.Clean(strings.ToUpper(p[1:2]) + `:\` + p[min(3, len(p)):]), nil
	}
	return "", fmt.Errorf("host path %s is not an absolute Windows path, such as C:\\src", p)
}

// socketSource reads a bind's source as a socket's: a named pipe, as in
// //./pipe/docker_engine or \\.\pipe\docker_engine, which it gives as
// \\.\pipe\NAME, or Docker's own socket, as Testcontainers' Ryuk and
// docker-outside-of-docker mount it, and as Docker Desktop's Windows
// spelling //var/run/docker.sock has it, which means runwhp's Engine API
// and gives an empty pipe. ok is false for any other source.
func socketSource(p string) (pipe string, ok bool) {
	slashed := strings.ReplaceAll(p, `\`, "/")
	switch low := strings.ToLower(slashed); {
	case low == "/var/run/docker.sock", low == "//var/run/docker.sock", low == "/run/docker.sock", low == "//run/docker.sock":
		return "", true
	case strings.HasPrefix(low, "//./pipe/"), strings.HasPrefix(low, "//?/pipe/"):
		name := strings.Trim(slashed[len("//./pipe/"):], "/")
		if name == "" {
			return "", false
		}
		return `\\.\pipe\` + strings.ReplaceAll(name, "/", `\`), true
	}
	return "", false
}

// pipeExists reports whether something serves pipe now. Unlike
// pipeServed, it does not connect: Stat tells a pipe that exists, busy or
// not, from one that does not without taking an instance.
func pipeExists(pipe string) bool {
	_, err := os.Stat(pipe)
	return !os.IsNotExist(err)
}

// warnUnservedPipes warns of sockets whose pipe nothing serves yet: the
// container is made all the same, and its connections fail until it is.
func warnUnservedPipes(w io.Writer, mounts []guestspec.Mount) {
	for _, m := range mounts {
		if m.Kind != guestspec.MountSocket || pipeExists(m.Pipe) {
			continue
		}
		hint := ""
		if strings.HasPrefix(m.Pipe, `\\.\pipe\runwhp_engine`) {
			hint = ": start runwhp's Engine API with runwhp docker-api start"
		}
		fmt.Fprintf(w, "runwhp: warning: %s is not served yet, so connections to %s in the container fail until it is%s\n", m.Pipe, m.Target, hint)
	}
}

// containerPath checks a mount point inside the container.
func containerPath(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("the container path %q is not absolute", p)
	}
	p = path.Clean(p)
	for _, reserved := range []string{"/", "/proc", "/sys"} {
		if p == reserved || (reserved != "/" && strings.HasPrefix(p, reserved+"/")) {
			return "", fmt.Errorf("%s cannot be mounted over", p)
		}
	}
	return p, nil
}

// volumeUse is a named volume a run attaches: one disk, however many
// targets it is mounted at.
type volumeUse struct {
	name   string
	size   string            // if it has to be created; empty means the default
	labels map[string]string // if it has to be created
	shared bool              // make it shared, if it is not
}

// mountPlan is what -v, --mount and --tmpfs ask for: the host directories to
// share, the volumes to attach, and the mounts init makes.
type mountPlan struct {
	shares  []container.Share
	volumes []volumeUse
	mounts  []guestspec.Mount
}

// parseMounts turns -v, --mount and --tmpfs into a mountPlan, with parents
// mounted before what is mounted inside them, Docker's socket reaching
// apiPipe.
func parseMounts(volumes, mounts, tmpfs []string, apiPipe string) (mountPlan, error) {
	specs, err := parseMountFlags(volumes, mounts, tmpfs)
	if err != nil {
		return mountPlan{}, err
	}
	return planMounts(specs, apiPipe)
}

// parseMountFlags reads -v, --mount and --tmpfs, resolving relative host
// paths against the working directory.
func parseMountFlags(volumes, mounts, tmpfs []string) ([]mountSpec, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	var specs []mountSpec
	for _, s := range volumes {
		m, err := parseVolume(s, cwd)
		if err != nil {
			return nil, err
		}
		specs = append(specs, m)
	}
	for _, s := range mounts {
		m, err := parseMount(s, cwd)
		if err != nil {
			return nil, err
		}
		specs = append(specs, m)
	}
	for _, s := range tmpfs {
		m, err := parseTmpfs(s)
		if err != nil {
			return nil, err
		}
		specs = append(specs, m)
	}
	return specs, nil
}

// planMounts turns mounts into a mountPlan, with parents mounted before
// what is mounted inside them, making missing bind sources it may make.
// Docker's socket reaches apiPipe, the Engine API's.
func planMounts(specs []mountSpec, apiPipe string) (mountPlan, error) {
	var plan mountPlan
	seen := map[string]bool{}
	disks := map[string]int{} // volume name, lower case -> its disk
	for _, m := range specs {
		if seen[m.target] {
			return plan, fmt.Errorf("%s is mounted more than once", m.target)
		}
		seen[m.target] = true
		gm := guestspec.Mount{Kind: m.kind, Target: m.target, ReadOnly: m.readOnly, NoCopy: m.noCopy, Options: m.options}
		switch {
		case m.kind == guestspec.MountSocket:
			// A socket is connected to, never written: read-only changes
			// nothing but what inspect says.
			gm.File, gm.Pipe, gm.Source = true, m.pipe, m.source
			if gm.Pipe == "" {
				gm.Pipe = apiPipe
			}
		case m.kind == guestspec.MountBind:
			st, err := os.Stat(m.source)
			if os.IsNotExist(err) && m.create {
				if err = os.MkdirAll(m.source, 0o755); err == nil {
					st, err = os.Stat(m.source)
				}
			}
			if os.IsNotExist(err) {
				return plan, fmt.Errorf("bind source path does not exist: %s", m.source)
			}
			if err != nil {
				return plan, fmt.Errorf("bind source %s: %w", m.source, err)
			}
			// A read-only mount writes nothing for the guest to cache.
			delegated := m.delegated && !m.readOnly
			// Delegated shares go to a file system of their own, and each
			// file system numbers its shares from 0.
			gm.Share, gm.File, gm.Delegated = 0, !st.IsDir(), delegated
			for _, s := range plan.shares {
				if s.Delegated == delegated {
					gm.Share++
				}
			}
			plan.shares = append(plan.shares, container.Share{Path: m.source, File: gm.File, ReadOnly: m.readOnly, Target: m.target,
				Create: m.create, Delegated: delegated})
		case m.kind == guestspec.MountVolume && m.source == "":
			gm.Kind = guestspec.MountAnon
		case m.kind == guestspec.MountVolume:
			key := strings.ToLower(m.source)
			i, ok := disks[key]
			if !ok {
				i = len(plan.volumes)
				disks[key] = i
				plan.volumes = append(plan.volumes, volumeUse{name: m.source})
			}
			if plan.volumes[i].size == "" {
				plan.volumes[i].size = m.size
			}
			if plan.volumes[i].labels == nil {
				plan.volumes[i].labels = m.labels
			}
			plan.volumes[i].shared = plan.volumes[i].shared || m.shared
			gm.Volume, gm.Name = i, m.source
		}
		plan.mounts = append(plan.mounts, gm)
	}
	sort.SliceStable(plan.mounts, func(i, j int) bool {
		return strings.Count(plan.mounts[i].Target, "/") < strings.Count(plan.mounts[j].Target, "/")
	})
	return plan, nil
}
