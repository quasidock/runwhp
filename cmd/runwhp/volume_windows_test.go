//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVolume(t *testing.T) {
	cwd := `C:\work\proj`
	for _, tc := range []struct {
		in   string
		want bind
	}{
		{`C:\src:/app`, bind{source: `C:\src`, target: "/app", create: true}},
		{`c:/src/:/app/:ro`, bind{source: `c:\src`, target: "/app", readOnly: true, create: true}},
		{`.:/app`, bind{source: cwd, target: "/app", create: true}},
		{`.\sub:/app:rw,z`, bind{source: cwd + `\sub`, target: "/app", create: true}},
		{`..\other:/o:ro,cached`, bind{source: `C:\work\other`, target: "/o", readOnly: true, create: true}},
		{`./a/b:/x/../y`, bind{source: cwd + `\a\b`, target: "/y", create: true}},
		{`\\server\share\dir:/net`, bind{source: `\\server\share\dir`, target: "/net", create: true}},
		{`/c/Users/me:/home/me`, bind{source: `C:\Users\me`, target: "/home/me", create: true}},
		{`/d:/d`, bind{source: `D:\`, target: "/d", create: true}},
	} {
		got, err := parseVolume(tc.in, cwd)
		if err != nil || got != tc.want {
			t.Errorf("parseVolume(%s) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for in, want := range map[string]string{
		`C:\src`:           "want HOSTPATH:CONTAINERPATH",
		`data:/x`:          "named volumes",
		`C:\src:app`:       "not absolute",
		`C:\src:/`:         "cannot be mounted over",
		`C:\src:/proc`:     "cannot be mounted over",
		`C:\src:/sys/x`:    "cannot be mounted over",
		`C:\src:/x:shared`: "not supported",
		`C:src:/x`:         "not an absolute Windows path",
		`/etc:/x`:          "not an absolute Windows path",
		`:/x`:              "empty",
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
		want bind
	}{
		{`type=bind,source=C:\src,target=/app`, bind{source: `C:\src`, target: "/app"}},
		{`type=bind,src=.\d,dst=/d,readonly`, bind{source: cwd + `\d`, target: "/d", readOnly: true}},
		{`type=bind,source=C:\s,destination=/s,ro=false,bind-propagation=rprivate`, bind{source: `C:\s`, target: "/s"}},
		{`"type=bind","source=C:\with,comma",target=/c,readonly=1`, bind{source: `C:\with,comma`, target: "/c", readOnly: true}},
	} {
		got, err := parseMount(tc.in, cwd)
		if err != nil || got != tc.want {
			t.Errorf("parseMount(%s) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for in, want := range map[string]string{
		`source=C:\s,target=/s`:                                   "only type=bind",
		`type=volume,source=v,target=/s`:                          "not supported yet",
		`type=tmpfs,target=/s`:                                    "not supported yet",
		`type=bind,target=/s`:                                     "source is required",
		`type=bind,source=C:\s`:                                   "target is required",
		`type=bind,source=C:\s,target=/s,ro=what`:                 "not a boolean",
		`type=bind,source=C:\s,target=/s,size=1`:                  "unknown option",
		`type=bind,source=C:\s,target=/s,bind-propagation=shared`: "not supported",
	} {
		if _, err := parseMount(in, cwd); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseMount(%s) = %v, want an error about %q", in, err, want)
		}
	}
}

func TestParseBinds(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.toml")
	os.WriteFile(file, []byte("x"), 0o644)
	missing := filepath.Join(dir, "made", "by", "v")

	shares, mounts, err := parseBinds(
		[]string{dir + ":/a/b/c", missing + ":/a:ro"},
		[]string{"type=bind,source=" + file + ",target=/etc/app.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 3 || !shares[2].File || shares[0].File || !shares[1].ReadOnly || shares[0].Target != "/a/b/c" {
		t.Errorf("shares %+v", shares)
	}
	if st, err := os.Stat(missing); err != nil || !st.IsDir() {
		t.Errorf("-v did not create its missing source: %v", err)
	}
	// Parents are mounted before what is mounted inside them.
	var order []string
	for _, m := range mounts {
		order = append(order, m.Target)
		if shares[m.Share].Target != m.Target || shares[m.Share].File != m.File {
			t.Errorf("mount %+v does not match its share %+v", m, shares[m.Share])
		}
	}
	if strings.Join(order, " ") != "/a /etc/app.toml /a/b/c" {
		t.Errorf("mount order %v", order)
	}

	if _, _, err := parseBinds(nil, []string{"type=bind,source=" + filepath.Join(dir, "nope") + ",target=/x"}); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("--mount with a missing source: %v", err)
	}
	if _, _, err := parseBinds([]string{dir + ":/x", dir + ":/x/"}, nil); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Errorf("a target mounted twice: %v", err)
	}
}
