//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/volume"
)

func TestParseLabels(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "labels")
	// As check 19 recorded Docker's client reading one: a BOM, comments and
	// blank lines skipped, keys' leading spaces dropped, values kept as they
	// are, a bare key from the environment or left out.
	os.WriteFile(file, []byte("\xef\xbb\xbf# comment\n\nlf.a=1\n  lf.b=spaced value  \nlf.c\nimg.b=file\nlf.d=\"quoted\"\nLF_ENV\n"), 0o644)
	t.Setenv("LF_ENV", "from-env")
	got, err := parseLabels([]string{"img.b=flag", "lf.a=flag", "bare"}, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"bare": "", "img.b": "flag", "lf.a": "flag", "lf.b": "spaced value  ", "lf.d": `"quoted"`, "LF_ENV": "from-env"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got, err := parseLabels(nil, nil); got != nil || err != nil {
		t.Errorf("no labels: %v, %v", got, err)
	}
	if merged := withLabels(nil, nil); merged != nil {
		t.Errorf("nil over nil: %v", merged)
	}
	if merged := withLabels(map[string]string{"img.a": "1", "img.b": "image"}, want); merged["img.a"] != "1" || merged["img.b"] != "flag" {
		t.Errorf("merged %v", merged)
	}

	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("ok=1\nlf.b =x\n"), 0o644)
	for _, c := range []struct {
		flags, files []string
		want         string
	}{
		{[]string{"=x"}, nil, "invalid label '=x': empty name"},
		{nil, []string{bad}, "--label-file: invalid env file (" + bad + "): variable 'lf.b ' contains whitespaces"},
		{nil, []string{filepath.Join(dir, "nonexistent")}, "--label-file: open " + filepath.Join(dir, "nonexistent") + ":"},
	} {
		if _, err := parseLabels(c.flags, c.files); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%q %q: %v, want %q", c.flags, c.files, err, c.want)
		}
	}
}

func TestContainerFilters(t *testing.T) {
	now := time.Now()
	mk := func(id, name string, labels map[string]string, st container.State) *container.Container {
		return &container.Container{ID: id, Name: name, State: st, Config: &container.Config{
			ID: id, Labels: labels, Image: "c19lbl", ImageName: "index.docker.io/library/c19lbl:latest", ImageID: "sha256:610c9fa89f77",
			Network: true, Volumes: []container.Volume{{Name: "data"}}, Mounts: []guestspec.Mount{{Kind: guestspec.MountAnon, Target: "/anon"}},
		}}
	}
	l1 := mk("aaaa1111", "c19l1", map[string]string{"bare": "", "lf.a": "flag", "img.a": "1"}, container.State{Status: container.Running, StartedAt: now})
	l2 := mk("bbbb2222", "c19l2", map[string]string{"tier": "back"}, container.State{Status: container.Stopped, FinishedAt: now, ExitCode: 137})
	l2.Config.NetworkMode, l2.Config.Networks = "front", []container.Attachment{{Network: "cccc3333dddd", Name: "front"}}
	paused := mk("eeee", "p", nil, container.State{Status: container.Running, Paused: true})
	created := mk("ffff", "c", nil, container.State{Status: container.Created})
	all := []*container.Container{l1, l2, paused, created}

	for _, c := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"c19l1", "c19l2", "p", "c"}},
		{[]string{"name=c19l1", "name=c19l2"}, []string{"c19l1", "c19l2"}}, // OR within a key
		{[]string{"name=c19l", "label=tier=back"}, []string{"c19l2"}},      // AND across keys
		{[]string{"label=bare"}, []string{"c19l1"}},                        // a key alone
		{[]string{"label=lf.a=flag", "label=img.a=1"}, []string{"c19l1"}},  // labels all match
		{[]string{"label=lf.a=flag", "label=tier=back"}, nil},              // or none
		{[]string{"name=^c19l1"}, []string{"c19l1"}},                       // a regular expression
		{[]string{"name=C19L1"}, nil},                                      // case-sensitive
		{[]string{"id=BBBB"}, []string{"c19l2"}},                           // a prefix
		{[]string{"status=running", "status=exited"}, []string{"c19l1", "c19l2"}},
		{[]string{"status=paused"}, []string{"p"}},
		{[]string{"status=created"}, []string{"c"}},
		{[]string{"exited=137"}, []string{"c19l2"}},
		{[]string{"network=front"}, []string{"c19l2"}},
		{[]string{"network=cccc3333"}, []string{"c19l2"}},
		{[]string{"network=bridge", "name=c19"}, []string{"c19l1"}},
		{[]string{"ancestor=c19lbl", "name=c19l1"}, []string{"c19l1"}},
		{[]string{"ancestor=c19lbl:latest", "name=c19l1"}, []string{"c19l1"}},
		{[]string{"ancestor=610c9f", "name=c19l1"}, []string{"c19l1"}},
		{[]string{"ancestor=alpine"}, nil},
		{[]string{"volume=data", "name=c19l1"}, []string{"c19l1"}},
		{[]string{"volume=/anon", "name=c19l1"}, []string{"c19l1"}},
	} {
		f, err := parseContainerFilters(c.args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		var got []string
		for _, ctr := range all {
			if f.matchContainer(ctr) {
				got = append(got, ctr.Name)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.args, got, c.want)
		}
	}

	for arg, want := range map[string]string{
		"status=bogus": "invalid filter 'status=bogus': invalid value for state (bogus): must be one of created, running, paused, restarting, removing, exited, dead",
		"bogus=1":      "invalid filter 'bogus'",
		"label!=x=y":   "invalid filter 'label!'",
		"novalue":      "bad format of filter (expected name=value)",
		"exited=x":     "invalid filter 'exited=x'",
	} {
		if _, err := parseContainerFilters([]string{arg}); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", arg, err, want)
		}
	}
}

func TestNetworkAndVolumeFilters(t *testing.T) {
	n := &network.Network{ID: "1234abcd", Name: "c19lnet", Driver: "bridge", Labels: map[string]string{"a": "1", "b": ""}}
	user := network.Builtin("user")
	for args, want := range map[string][2]bool{
		"label=a=1":     {true, false},
		"label=b":       {true, false},
		"label=a=2":     {false, false},
		"name=c19l":     {true, false},
		"name=user":     {false, true},
		"driver=bridge": {true, false},
		"type=custom":   {true, false},
		"type=builtin":  {false, true},
		"id=1234":       {true, false},
	} {
		f, err := parseFilters([]string{args}, "name", "id", "label", "driver", "type")
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]bool{f.matchNetwork(n), f.matchNetwork(user)}; got != want {
			t.Errorf("%s: %v, want %v", args, got, want)
		}
	}
	v := &volume.Volume{Name: "c19lvol", Labels: map[string]string{"a": "1"}}
	for args, want := range map[string][2]bool{ // unused, used
		"label=a":        {true, true},
		"label=b":        {false, false},
		"dangling=true":  {true, false},
		"dangling=false": {false, true},
		"name=lvol":      {true, true},
		"driver=local":   {true, true},
	} {
		f, _ := parseFilters([]string{args}, "name", "label", "driver", "dangling")
		if got := [2]bool{f.matchVolume(v, false), f.matchVolume(v, true)}; got != want {
			t.Errorf("%s: %v, want %v", args, got, want)
		}
	}
}
