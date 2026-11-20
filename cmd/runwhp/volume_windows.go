//go:build windows

package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/hostfs"
)

// bind is one host path bound into the container, from -v or --mount.
type bind struct {
	source   string // absolute host path
	target   string // clean absolute container path
	readOnly bool
	create   bool // create a missing source directory, as -v does
}

// parseVolume reads -v HOSTPATH:CONTAINERPATH[:OPTIONS]. The colon of a
// drive letter belongs to HOSTPATH.
func parseVolume(s, cwd string) (bind, error) {
	usage := fmt.Errorf("-v %s: want HOSTPATH:CONTAINERPATH[:ro]", s)
	skip := 0
	if len(s) >= 2 && s[1] == ':' && isLetter(s[0]) {
		skip = 2
	}
	i := strings.IndexByte(s[skip:], ':')
	if i < 0 {
		return bind{}, usage
	}
	src, rest := s[:skip+i], s[skip+i+1:]
	dst, opts, _ := strings.Cut(rest, ":")
	b := bind{create: true}
	var err error
	if b.source, err = hostPath(src, cwd); err != nil {
		return bind{}, fmt.Errorf("-v %s: %w", s, err)
	}
	if b.target, err = containerPath(dst); err != nil {
		return bind{}, fmt.Errorf("-v %s: %w", s, err)
	}
	if opts == "" {
		return b, nil
	}
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "ro", "readonly":
			b.readOnly = true
		case "rw":
			b.readOnly = false
		case "z", "Z", "consistent", "cached", "delegated", "private", "rprivate":
			// SELinux labels, Docker Desktop's old consistency hints, and
			// the propagation every mount here has.
		default:
			return bind{}, fmt.Errorf("-v %s: option %q is not supported", s, o)
		}
	}
	return b, nil
}

// parseMount reads --mount type=bind,source=...,target=...[,readonly].
func parseMount(s, cwd string) (bind, error) {
	fields, err := csv.NewReader(strings.NewReader(s)).Read()
	if err != nil {
		return bind{}, fmt.Errorf("--mount %s: %w", s, err)
	}
	var b bind
	var typ, src, dst string
	for _, f := range fields {
		key, val, hasVal := strings.Cut(f, "=")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "type":
			typ = val
		case "source", "src":
			src = val
		case "target", "destination", "dst":
			dst = val
		case "readonly", "ro":
			b.readOnly = true
			if hasVal {
				if b.readOnly, err = strconv.ParseBool(val); err != nil {
					return bind{}, fmt.Errorf("--mount %s: readonly=%q is not a boolean", s, val)
				}
			}
		case "bind-propagation":
			if val != "private" && val != "rprivate" {
				return bind{}, fmt.Errorf("--mount %s: bind-propagation=%s is not supported", s, val)
			}
		case "consistency":
		default:
			return bind{}, fmt.Errorf("--mount %s: unknown option %q", s, key)
		}
	}
	switch typ {
	case "bind":
	case "":
		return bind{}, fmt.Errorf("--mount %s: only type=bind is supported", s)
	default:
		return bind{}, fmt.Errorf("--mount %s: type=%s is not supported yet; use type=bind", s, typ)
	}
	if src == "" {
		return bind{}, fmt.Errorf("--mount %s: source is required", s)
	}
	if dst == "" {
		return bind{}, fmt.Errorf("--mount %s: target is required", s)
	}
	if b.source, err = hostPath(src, cwd); err != nil {
		return bind{}, fmt.Errorf("--mount %s: %w", s, err)
	}
	if b.target, err = containerPath(dst); err != nil {
		return bind{}, fmt.Errorf("--mount %s: %w", s, err)
	}
	return b, nil
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
		return filepath.Join(cwd, p), nil
	case len(p) >= 2 && p[0] == '/' && isLetter(p[1]) && (len(p) == 2 || p[2] == '/'):
		return filepath.Clean(strings.ToUpper(p[1:2]) + `:\` + p[min(3, len(p)):]), nil
	case p[0] == '/' || p[0] == '\\' || (len(p) >= 2 && p[1] == ':'):
		return "", fmt.Errorf("host path %s is not an absolute Windows path, such as C:\\src", p)
	}
	return "", fmt.Errorf("named volumes are not supported yet; give a host path, such as C:\\data or .\\data, instead of %q", p)
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

// parseBinds turns -v and --mount into the shares the host serves and the
// mounts init makes, parents before what is mounted inside them.
func parseBinds(volumes, mounts []string) ([]hostfs.Share, []guestspec.Mount, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	var binds []bind
	for _, s := range volumes {
		b, err := parseVolume(s, cwd)
		if err != nil {
			return nil, nil, err
		}
		binds = append(binds, b)
	}
	for _, s := range mounts {
		b, err := parseMount(s, cwd)
		if err != nil {
			return nil, nil, err
		}
		binds = append(binds, b)
	}
	var shares []hostfs.Share
	var gm []guestspec.Mount
	seen := map[string]bool{}
	for i, b := range binds {
		if seen[b.target] {
			return nil, nil, fmt.Errorf("%s is mounted more than once", b.target)
		}
		seen[b.target] = true
		st, err := os.Stat(b.source)
		if os.IsNotExist(err) && b.create {
			if err = os.MkdirAll(b.source, 0o755); err == nil {
				st, err = os.Stat(b.source)
			}
		}
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("bind source path does not exist: %s", b.source)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("bind source %s: %w", b.source, err)
		}
		shares = append(shares, hostfs.Share{Path: b.source, File: !st.IsDir(), ReadOnly: b.readOnly, Target: b.target})
		gm = append(gm, guestspec.Mount{Target: b.target, Share: i, File: !st.IsDir(), ReadOnly: b.readOnly})
	}
	sort.SliceStable(gm, func(i, j int) bool {
		return strings.Count(gm[i].Target, "/") < strings.Count(gm[j].Target, "/")
	})
	return shares, gm, nil
}
