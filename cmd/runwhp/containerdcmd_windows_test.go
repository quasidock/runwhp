//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestContainerdConfig(t *testing.T) {
	c, err := containerdConfig(containerdSetup{dir: `C:\Users\me\AppData\Local\runwhp\containerd`, address: `\\.\pipe\containerd-runwhp`,
		snapshotter: `\\.\pipe\runwhp-snapshotter-1234`, kernel: `C:\k\bzImage`})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"version = 4",
		`root = 'C:\Users\me\AppData\Local\runwhp\containerd\root'`,
		`state = 'C:\Users\me\AppData\Local\runwhp\containerd\state'`,
		`address = '\\.\pipe\containerd-runwhp'`,
		`address = '\\.\pipe\containerd-runwhp.ttrpc'`,
		`'io.containerd.snapshotter.v1.cimfs'`,
		`'io.containerd.grpc.v1.cri'`,
		`env = ['RUNWHP_ROOT=C:\Users\me\AppData\Local\runwhp\containerd\store', 'RUNWHP_KERNEL=C:\k\bzImage']`,
		"snapshotter = 'runwhp'",
		"differ = 'windows-lcow'",
		"platform = 'linux/amd64'",
		"[proxy_plugins.runwhp]",
		`address = '\\.\pipe\runwhp-snapshotter-1234'`,
		"level = 'info'",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %s:\n%s", want, c)
		}
	}
	if _, err := containerdConfig(containerdSetup{dir: `C:\it's`, address: `\\.\pipe\x`}); err == nil {
		t.Error("a quote in a path was taken")
	}
	c, _ = containerdConfig(containerdSetup{dir: `C:\d`, address: `\\.\pipe\x`, snapshotter: `\\.\pipe\s`, debug: true})
	if !strings.Contains(c, "level = 'debug'") || strings.Contains(c, "RUNWHP_KERNEL") {
		t.Errorf("debug, no kernel:\n%s", c)
	}
}
