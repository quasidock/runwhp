//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/vm"
)

const (
	bindKind   = guestspec.MountBind
	volumeKind = guestspec.MountVolume
	tmpfsKind  = guestspec.MountTmpfs
)

func TestParseVolume(t *testing.T) {
	cwd := `C:\work\proj`
	for _, tc := range []struct {
		in   string
		want mountSpec
	}{
		{`C:\src:/app`, mountSpec{kind: bindKind, source: `C:\src`, target: "/app", create: true}},
		{`c:/src/:/app/:ro`, mountSpec{kind: bindKind, source: `c:\src`, target: "/app", readOnly: true, create: true}},
		{`.:/app`, mountSpec{kind: bindKind, source: cwd, target: "/app", create: true}},
		{`.\sub:/app:rw,z`, mountSpec{kind: bindKind, source: cwd + `\sub`, target: "/app", create: true}},
		{`..\other:/o:ro,cached`, mountSpec{kind: bindKind, source: `C:\work\other`, target: "/o", readOnly: true, create: true}},
		{`./a/b:/x/../y`, mountSpec{kind: bindKind, source: cwd + `\a\b`, target: "/y", create: true}},
		{`\\server\share\dir:/net`, mountSpec{kind: bindKind, source: `\\server\share\dir`, target: "/net", create: true}},
		{`/c/Users/me:/home/me`, mountSpec{kind: bindKind, source: `C:\Users\me`, target: "/home/me", create: true}},
		{`/d:/d`, mountSpec{kind: bindKind, source: `D:\`, target: "/d", create: true}},
		{`data:/x`, mountSpec{kind: volumeKind, source: "data", target: "/x"}},
		{`pg-data.v2:/var/lib/pg:ro,nocopy`, mountSpec{kind: volumeKind, source: "pg-data.v2", target: "/var/lib/pg", readOnly: true, noCopy: true}},
		{`/app/node_modules`, mountSpec{kind: volumeKind, target: "/app/node_modules"}},
		{`/cache/`, mountSpec{kind: volumeKind, target: "/cache"}},
		{`C:\out:/out:delegated`, mountSpec{kind: bindKind, source: `C:\out`, target: "/out", create: true, delegated: true}},
		{`C:\out:/out:ro,delegated`, mountSpec{kind: bindKind, source: `C:\out`, target: "/out", readOnly: true, create: true, delegated: true}},
		{`C:\out:/out:consistent`, mountSpec{kind: bindKind, source: `C:\out`, target: "/out", create: true}},
	} {
		got, err := parseVolume(tc.in, cwd)
		if err != nil || got != tc.want {
			t.Errorf("parseVolume(%s) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for in, want := range map[string]string{
		`C:\src`:              "want HOSTPATH:CONTAINERPATH",
		`data`:                "want HOSTPATH:CONTAINERPATH",
		`C:\src:app`:          "not absolute",
		`C:\src:/`:            "cannot be mounted over",
		`C:\src:/proc`:        "cannot be mounted over",
		`C:\src:/sys/x`:       "cannot be mounted over",
		`C:\src:/x:shared`:    "not supported",
		`C:\src:/x:nocopy`:    "not supported",
		`C:src:/x`:            "not an absolute Windows path",
		`/etc:/x`:             "not an absolute Windows path",
		`:/x`:                 "empty",
		`a:/x`:                "want HOSTPATH:CONTAINERPATH", // drive A:
		`_x:/x`:               "invalid volume name",
		`con:/x`:              "reserves",
		`bad name:/x`:         "invalid volume name",
		`/`:                   "cannot be mounted over",
		`/proc/x`:             "cannot be mounted over",
		`data:/x:ro,whatever`: "not supported",
	} {
		if _, err := parseVolume(in, cwd); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseVolume(%s) = %v, want an error about %q", in, err, want)
		}
	}
	// /procfs is not /proc.
	if _, err := parseVolume(`C:\x:/procfs`, cwd); err != nil {
		t.Errorf("/procfs refused: %v", err)
	}
}

func TestParseMount(t *testing.T) {
	cwd := `C:\work`
	for _, tc := range []struct {
		in   string
		want mountSpec
	}{
		{`type=bind,source=C:\src,target=/app`, mountSpec{kind: bindKind, source: `C:\src`, target: "/app"}},
		{`type=bind,src=.\d,dst=/d,readonly`, mountSpec{kind: bindKind, source: cwd + `\d`, target: "/d", readOnly: true}},
		{`type=bind,source=C:\s,destination=/s,ro=false,bind-propagation=rprivate`, mountSpec{kind: bindKind, source: `C:\s`, target: "/s"}},
		{`"type=bind","source=C:\with,comma",target=/c,readonly=1`, mountSpec{kind: bindKind, source: `C:\with,comma`, target: "/c", readOnly: true}},
		{`type=volume,source=v1,target=/s`, mountSpec{kind: volumeKind, source: "v1", target: "/s"}},
		{`type=volume,target=/s,volume-nocopy`, mountSpec{kind: volumeKind, target: "/s", noCopy: true}},
		{`type=volume,src=big,dst=/b,volume-opt=size=200g,volume-driver=local,ro`, mountSpec{kind: volumeKind, source: "big", target: "/b", size: "200g", readOnly: true}},
		{`type=bind,source=C:\o,target=/o,consistency=delegated`, mountSpec{kind: bindKind, source: `C:\o`, target: "/o", delegated: true}},
		{`type=bind,source=C:\o,target=/o,consistency=cached`, mountSpec{kind: bindKind, source: `C:\o`, target: "/o"}},
		{`type=bind,source=C:\o,target=/o,consistency=default`, mountSpec{kind: bindKind, source: `C:\o`, target: "/o"}},
		{`type=tmpfs,target=/t`, mountSpec{kind: tmpfsKind, target: "/t", options: "noexec,nosuid,nodev,mode=1777"}},
		{`type=tmpfs,dst=/t,tmpfs-size=1m,tmpfs-mode=700,readonly`, mountSpec{kind: tmpfsKind, target: "/t", readOnly: true, options: "noexec,nosuid,nodev,ro,mode=700,size=1048576"}},
	} {
		got, err := parseMount(tc.in, cwd)
		if err != nil || got != tc.want {
			t.Errorf("parseMount(%s) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for in, want := range map[string]string{
		`source=C:\s,target=/s`:                                   "type is required",
		`type=npipe,source=x,target=/s`:                           "not supported",
		`type=bind,target=/s`:                                     "source is required",
		`type=bind,source=C:\s`:                                   "target is required",
		`type=bind,source=data,target=/s`:                         "not an absolute Windows path",
		`type=bind,source=C:\s,target=/s,ro=what`:                 "not a boolean",
		`type=bind,source=C:\s,target=/s,size=1`:                  "unknown option",
		`type=bind,source=C:\s,target=/s,bind-propagation=shared`: "not supported",
		`type=bind,source=C:\s,target=/s,volume-nocopy`:           "only to type=volume",
		`type=volume,source=v1,target=/s,tmpfs-size=1m`:           "only to type=tmpfs",
		`type=volume,source=x,target=/s`:                          "invalid volume name",
		`type=volume,source=v1,target=/s,volume-opt=type=nfs`:     "volume-opt=type=nfs",
		`type=volume,source=v1,target=/s,volume-opt=size=1m`:      "out of range",
		`type=volume,source=v1,target=/s,volume-driver=nfs`:       "not supported",
		`type=tmpfs,source=x,target=/t`:                           "no source",
		`type=tmpfs,target=/t,tmpfs-mode=999`:                     "octal",
		`type=tmpfs,target=/t,tmpfs-size=lots`:                    "invalid size",
		`type=bind,source=C:\s,target=/s,consistency=eventual`:    "consistency=eventual is not supported",
	} {
		if _, err := parseMount(in, cwd); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseMount(%s) = %v, want an error about %q", in, err, want)
		}
	}
}

func TestParseTmpfs(t *testing.T) {
	for in, want := range map[string]mountSpec{
		"/run":                         {kind: tmpfsKind, target: "/run", options: "noexec,nosuid,nodev,mode=1777"},
		"/t:exec,size=64m":             {kind: tmpfsKind, target: "/t", options: "nosuid,nodev,mode=1777,size=64m"},
		"/t:rw,suid,dev,exec,mode=700": {kind: tmpfsKind, target: "/t", options: "mode=700"},
		"/t:ro,uid=1000,gid=1000":      {kind: tmpfsKind, target: "/t", readOnly: true, options: "noexec,nosuid,nodev,ro,mode=1777,uid=1000,gid=1000"},
		"/t:size=50%,nr_inodes=10k":    {kind: tmpfsKind, target: "/t", options: "noexec,nosuid,nodev,mode=1777,size=50%,nr_inodes=10k"},
	} {
		got, err := parseTmpfs(in)
		if err != nil || got != want {
			t.Errorf("parseTmpfs(%s) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"t":             "not absolute",
		"/":             "cannot be mounted over",
		"/t:size=lots":  "size=lots",
		"/t:mode=8":     "octal",
		"/t:uid=root":   "not a number",
		"/t:huge=1":     "not supported",
		"/t:remount,ro": "not supported",
	} {
		if _, err := parseTmpfs(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseTmpfs(%s) = %v, want an error about %q", in, err, want)
		}
	}
}

func TestParseMounts(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.toml")
	os.WriteFile(file, []byte("x"), 0o644)
	missing := filepath.Join(dir, "made", "by", "v")

	plan, err := parseMounts(
		[]string{dir + ":/a/b/c", "cache:/root/.cache", missing + ":/a:ro", "/a/b/c/node_modules", "Cache:/x/again:ro"},
		[]string{"type=bind,source=" + file + ",target=/etc/app.toml", "type=volume,source=db,target=/var/db,volume-opt=size=2g"},
		[]string{"/run:exec"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.shares) != 3 || !plan.shares[2].File || plan.shares[0].File || !plan.shares[1].ReadOnly || plan.shares[0].Target != "/a/b/c" {
		t.Errorf("shares %+v", plan.shares)
	}
	// One disk per volume, however many targets and whatever case.
	if len(plan.volumes) != 2 || plan.volumes[0] != (volumeUse{name: "cache"}) || plan.volumes[1] != (volumeUse{name: "db", size: "2g"}) {
		t.Errorf("volumes %+v", plan.volumes)
	}
	if st, err := os.Stat(missing); err != nil || !st.IsDir() {
		t.Errorf("-v did not create its missing source: %v", err)
	}
	// Parents are mounted before what is mounted inside them, whatever
	// their kind.
	var order []string
	for _, m := range plan.mounts {
		order = append(order, m.Kind+":"+m.Target)
		switch m.Kind {
		case guestspec.MountBind:
			if s := plan.shares[m.Share]; s.Target != m.Target || s.File != m.File {
				t.Errorf("mount %+v does not match its share %+v", m, s)
			}
		case guestspec.MountVolume:
			if !strings.EqualFold(plan.volumes[m.Volume].name, m.Name) {
				t.Errorf("mount %+v does not match its volume %+v", m, plan.volumes[m.Volume])
			}
		}
	}
	if got := strings.Join(order, " "); got != ":/a tmpfs:/run volume:/root/.cache volume:/x/again :/etc/app.toml volume:/var/db :/a/b/c anon:/a/b/c/node_modules" {
		t.Errorf("mount order %s", got)
	}

	if _, err := parseMounts(nil, []string{"type=bind,source=" + filepath.Join(dir, "nope") + ",target=/x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("--mount with a missing source: %v", err)
	}
	if _, err := parseMounts([]string{dir + ":/x", "v1:/x/"}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Errorf("a target mounted twice: %v", err)
	}
	if _, err := parseMounts(nil, nil, []string{"/t", "/t"}); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("a tmpfs mounted twice: %v", err)
	}
}

// Delegated bind mounts go to a file system of their own, which numbers its
// shares from 0 as the other does; a read-only one is not delegated.
func TestParseMountsDelegated(t *testing.T) {
	a, b, c, d := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	plan, err := parseMounts([]string{a + ":/a", b + ":/b:delegated", c + ":/c:ro,delegated"},
		[]string{"type=bind,source=" + d + ",target=/d,consistency=delegated"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		share     int
		delegated bool
	}{"/a": {0, false}, "/b": {0, true}, "/c": {1, false}, "/d": {1, true}}
	for _, m := range plan.mounts {
		if w := want[m.Target]; m.Share != w.share || m.Delegated != w.delegated {
			t.Errorf("mount %s: share %d delegated %v, want %d %v", m.Target, m.Share, m.Delegated, w.share, w.delegated)
		}
	}
	var delegated []string
	for _, s := range plan.shares {
		if s.Delegated {
			delegated = append(delegated, s.Target)
		}
	}
	if strings.Join(delegated, " ") != "/b /d" {
		t.Errorf("delegated shares %q, want /b and /d", delegated)
	}

	// The two file systems take an interrupt line each.
	cfg := &container.Config{Shares: plan.shares, LayerFiles: make([]string, vm.MaxDevices-6)}
	if err := checkDevices(cfg, 0); err != nil {
		t.Errorf("layers that fit refused: %v", err)
	}
	cfg.LayerFiles = append(cfg.LayerFiles, "one more")
	if err := checkDevices(cfg, 0); err == nil || !strings.Contains(err.Error(), "delegated bind mounts") {
		t.Errorf("one layer too many: %v", err)
	}
}
