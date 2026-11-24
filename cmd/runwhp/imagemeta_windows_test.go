//go:build windows

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/image"
)

func TestTag(t *testing.T) {
	root, _, _, _ := imageStore(t)
	is := image.Open(root)
	if _, code := imageCmd("", "tag", "app:1", "mine/app:v2"); code != 0 {
		t.Fatal("tag failed")
	}
	rec, err := findImage(is, "mine/app:v2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID() != sha("one") || rec.Digest != "" || rec.Ref != "index.docker.io/mine/app:v2" {
		t.Errorf("tagged record: %+v", rec.Image)
	}
	// Tagging by ID, over another image's tag, moves the tag.
	if _, code := imageCmd("", "tag", strings.TrimPrefix(sha("two"), "sha256:")[:8], "app:1"); code != 0 {
		t.Fatal("tag by ID failed")
	}
	if rec, _ := findImage(is, "app:1"); rec.ID() != sha("two") {
		t.Errorf("app:1 is %s after the tag moved", rec.ID())
	}
	out, _ := imageCmd("", "ls", "--digests")
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 2 && f[0] == "mine/app" && f[2] != "<none>" {
			t.Errorf("a tag shows a registry digest: %q", l)
		}
	}
	for _, args := range [][]string{
		{"tag", "nothere:1", "x:1"},
		{"tag", "app:latest", "x@sha256:" + strings.Repeat("0", 64)},
		{"tag", "app:latest", "Upper:1"},
	} {
		if _, code := imageCmd("", args...); code != 1 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if _, code := imageCmd("", "tag", "app:latest"); code != 2 {
		t.Error("tag with one argument")
	}
}

func TestHistory(t *testing.T) {
	root, a, b, _ := imageStore(t)
	built := time.Now().Add(-49 * time.Hour)
	cfg, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux", "created": built,
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{a, b}},
		"history": []map[string]any{
			{"created": built, "created_by": "ADD alpine-minirootfs-3.20.10-x86_64.tar.gz / # buildkit", "comment": "buildkit.dockerfile.v0"},
			{"created": built, "created_by": "ENV\tA=b", "empty_layer": true},
			{"created": built, "created_by": "RUN apk add curl", "comment": ""},
		},
	})
	img := &image.Image{Ref: "index.docker.io/library/hist:1", ConfigDigest: sha("hist"), Config: cfg}
	image.Open(root).Put("hist:1", img)

	out, code := imageCmd("", "history", "hist:1")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 4 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	id := strings.TrimPrefix(sha("hist"), "sha256:")[:12]
	for i, want := range []string{
		"IMAGE CREATED CREATED BY SIZE COMMENT",
		id + " 2 days ago RUN apk add curl 64KiB",
		"<missing> 2 days ago ENV A=b 0B",
		"<missing> 2 days ago ADD alpine-minirootfs-3.20.10-x86_64.tar.gz … 64KiB buildkit.dockerfile.v0",
	} {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != want {
			t.Errorf("line %d: %q, want %q", i, got, want)
		}
	}
	out, _ = imageCmd("", "history", "--no-trunc", "-H=false", "hist:1")
	if !strings.Contains(out, "CREATED AT") || !strings.Contains(out, "/ # buildkit") || !strings.Contains(out, sha("hist")) || !strings.Contains(out, " 65536 ") {
		t.Errorf("--no-trunc -H=false:\n%s", out)
	}
	if out, _ := imageCmd("", "history", "-q", "hist:1"); out != id+"\n<missing>\n<missing>\n" {
		t.Errorf("-q: %q", out)
	}
	out, _ = imageCmd("", "history", "--format", "json", "hist:1")
	var first map[string]string
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 3 || json.Unmarshal([]byte(lines[0]), &first) != nil ||
		first["ID"] != id || first["CreatedBy"] != "RUN apk add curl" || first["Size"] != "64KiB" || first["CreatedSince"] != "2 days ago" {
		t.Errorf("--format json:\n%s", out)
	}
	// An image without history: a step per layer.
	if out, code := imageCmd("", "history", "app:1"); code != 0 || strings.Count(out, "\n") != 3 {
		t.Errorf("history of an image without history: exit %d:\n%s", code, out)
	}
	if _, code := imageCmd("", "history", "nothere:1"); code != 1 {
		t.Error("history of a missing image")
	}
}
