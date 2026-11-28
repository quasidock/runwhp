//go:build windows && compose

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/compose"
	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestSpecOf(t *testing.T) {
	dir := t.TempDir()
	c := compose.Container{
		Image: "alpine:3.20", Command: []string{"sh"}, Entrypoint: []string{"/bin/sh", "-c"}, EntrypointSet: true,
		Env: []string{"A=1"}, WorkingDir: "/w", User: "1000", Hostname: "h", TTY: true, StdinOpen: true,
		Memory: 512 << 20, CPUs: 2,
		Networks:   []compose.Attachment{{Name: "p_back", Aliases: []string{"db"}, IPv4: "172.30.0.9"}},
		Ports:      []compose.Port{{HostPort: 8080, Target: 80}, {HostIP: "0.0.0.0", HostPort: 8443, Target: 443, Protocol: "tcp"}, {HostPort: 5353, Target: 53, Protocol: "udp"}},
		ExtraHosts: []string{"h:host-gateway"},
		Mounts: []compose.Mount{
			{Kind: compose.MountBind, Source: dir, Target: "/data/", ReadOnly: true, Create: true},
			{Kind: compose.MountVolume, Source: "p_data", Target: "/v", NoCopy: true},
			{Kind: compose.MountVolume, Target: "/anon"},
			{Kind: compose.MountTmpfs, Target: "/t", Options: "size=1048576,mode=1777"},
		},
		Restart: "on-failure:3", StopSignal: "SIGUSR1", StopTimeout: new(int), StorageSize: "8g",
		Health:    &compose.Health{Test: []string{"CMD", "true"}, Interval: time.Second, Retries: 2},
		Labels:    map[string]string{"tier": "front"},
		LogDriver: "local", LogOpts: []string{"max-size=10m"},
	}
	s, err := specOf(c, map[string]string{"com.docker.compose.project": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if s.image != "alpine:3.20" || !s.entrypointSet || !reflect.DeepEqual(s.entrypoint, []string{"/bin/sh", "-c"}) || !s.tty || !s.interactive ||
		s.memory != 512<<20 || s.cpus != 2 || s.restart.String() != "on-failure:3" || s.stopSignal != "SIGUSR1" || s.stopTimeout == nil ||
		s.storageSize != 8<<30 || s.logs.maxSize != 10<<20 || s.labels["tier"] != "front" || s.labels["com.docker.compose.project"] != "p" {
		t.Errorf("spec %+v", s)
	}
	if want := (healthOverrides{test: []string{"CMD", "true"}, interval: time.Second, retries: 2}); !reflect.DeepEqual(s.health, want) {
		t.Errorf("health %+v", s.health)
	}
	n := s.network
	if len(n.attach) != 1 || n.attach[0].name != "p_back" || n.attach[0].ip != "172.30.0.9" || !reflect.DeepEqual(n.attach[0].aliases, []string{"db"}) ||
		n.publish[0].String() != "127.0.0.1:8080->80/tcp" || n.publish[1].String() != "0.0.0.0:8443->443/tcp" || n.publish[2].String() != "127.0.0.1:5353->53/udp" || !n.hostGateway || len(n.hosts) != 1 {
		t.Errorf("network %+v", n)
	}
	want := []mountSpec{
		{kind: guestspec.MountBind, source: dir, target: "/data", readOnly: true, create: true},
		{kind: guestspec.MountVolume, source: "p_data", target: "/v", noCopy: true},
		{kind: guestspec.MountVolume, target: "/anon"},
	}
	if !reflect.DeepEqual(s.mounts[:3], want) || s.mounts[3].kind != guestspec.MountTmpfs || !strings.Contains(s.mounts[3].options, "size=1048576") {
		t.Errorf("mounts %+v", s.mounts)
	}
	if _, err := planMounts(s.mounts, ""); err != nil {
		t.Errorf("planning the mounts: %v", err)
	}

	// The default network, none, and run's defaults.
	s, err = specOf(compose.Container{Image: "a", DefaultNetwork: true, Restart: "no"}, nil)
	if err != nil || len(s.network.attach) != 0 || s.memory == 0 || s.cpus == 0 || s.storageSize == 0 {
		t.Errorf("defaults: %+v, %v", s, err)
	}
	if s, _ := specOf(compose.Container{Image: "a", NoNetwork: true, Restart: "no"}, nil); len(s.network.attach) != 1 || s.network.attach[0].name != "none" {
		t.Errorf("none: %+v", s.network)
	}
	for _, c := range []struct {
		c    compose.Container
		want string
	}{
		{compose.Container{Image: "a", Restart: "sometimes"}, "restart"},
		{compose.Container{Image: "a", Restart: "no", Health: &compose.Health{Test: []string{"CMD", "x"}, Interval: time.Microsecond}}, "Interval in Healthcheck cannot be less than 1ms"},
		{compose.Container{Image: "a", Restart: "no", Mounts: []compose.Mount{{Kind: compose.MountBind, Source: dir, Target: "relative"}}}, "not absolute"},
		{compose.Container{Image: "a", Restart: "no", StopSignal: "SIGNOPE"}, "Invalid signal"},
		{compose.Container{Image: "a", Restart: "no", Memory: 1 << 20}, "memory must be between"},
	} {
		if _, err := specOf(c.c, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.c, err, c.want)
		}
	}
}

func TestBuildRequestOf(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Other.dockerfile"), []byte("FROM alpine\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("secret\n"), 0o644)
	req, err := buildRequestOf(&compose.Build{Context: dir, Dockerfile: filepath.Join(dir, "Other.dockerfile"), Args: map[string]string{"V": "1"},
		Target: "t", Tags: []string{"p-web", "mine/web:2"}, Secrets: map[string]string{"pw": filepath.Join(dir, "pw")}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(req.opts.Dockerfile) != "FROM alpine\n" || req.opts.DockerfileName != "Other.dockerfile" || !req.opts.NoCache || req.opts.Target != "t" ||
		req.opts.BuildArgs["V"] != "1" || req.opts.Secrets["pw"].Path == "" || len(req.refs) != 2 || req.refs[0] != "index.docker.io/library/p-web:latest" {
		t.Errorf("request %+v", req)
	}
	req, err = buildRequestOf(&compose.Build{Context: dir, Inline: "FROM busybox\n", Tags: []string{"x"}}, false, true)
	if err != nil || string(req.opts.Dockerfile) != "FROM busybox\n" || !req.pull {
		t.Errorf("inline: %+v, %v", req, err)
	}
	if _, err := buildRequestOf(&compose.Build{Context: filepath.Join(dir, "nope"), Tags: []string{"x"}}, false, false); err == nil {
		t.Error("a missing context")
	}
}
