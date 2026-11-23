//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/image"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// imageStore makes a store with layer files A, B and C, the images app:1
// and app:latest (one image, layers A and B) and other:1 (layers A and C),
// and a stopped container made from app:1, and points RUNWHP_ROOT at it.
func imageStore(t *testing.T) (root string, a, b, c string) {
	root = t.TempDir()
	t.Setenv("RUNWHP_ROOT", root)
	layer := func(n string) string {
		d := sha("layer " + n)
		p := filepath.Join(root, "layers", "sha256", strings.TrimPrefix(d, "sha256:")+".ext4")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, 64<<10), 0o644)
		return d
	}
	a, b, c = layer("A"), layer("B"), layer("C")
	put := func(ref, id string, created time.Time, layers ...string) *image.Image {
		cfg, _ := json.Marshal(map[string]any{
			"architecture": "amd64", "os": "linux", "created": created,
			"config": map[string]any{"Cmd": []string{"sh"}},
			"rootfs": map[string]any{"type": "layers", "diff_ids": layers},
		})
		img := &image.Image{Ref: "index.docker.io/library/" + ref, Digest: sha("manifest " + id), ConfigDigest: sha(id), Config: cfg}
		if err := image.Open(root).Put(ref, img); err != nil {
			t.Fatal(err)
		}
		return img
	}
	built := time.Now().Add(-48 * time.Hour)
	app := put("app:1", "one", built, a, b)
	put("app:latest", "one", built, a, b)
	put("other:1", "two", time.Now().Add(-time.Hour), a, c)

	s := container.Open(root)
	cfg := &container.Config{Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(), Args: []string{"sh"},
		Image: "app:1", ImageName: app.Ref, ImageDigest: app.Digest, Layers: []string{a, b}}
	ctr, err := s.Create(cfg, "user")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Lock(ctr)
	if err != nil {
		t.Fatal(err)
	}
	l.WriteState(container.State{Status: container.Stopped})
	l.Close()
	return root, a, b, c
}

func imageCmd(stdin string, args ...string) (string, int) {
	var out bytes.Buffer
	code := cmdImage(args, strings.NewReader(stdin), &out)
	return out.String(), code
}

func TestImagesList(t *testing.T) {
	imageStore(t)
	oneID, twoID := strings.TrimPrefix(sha("one"), "sha256:")[:12], strings.TrimPrefix(sha("two"), "sha256:")[:12]

	out, code := imageCmd("", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 || !strings.HasPrefix(lines[0], "REPOSITORY") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// The newest image first.
	for i, want := range []string{"other 1 " + twoID + " About an hour ago", "app 1 " + oneID + " 2 days ago", "app latest " + oneID + " 2 days ago"} {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); !strings.HasPrefix(got, want) {
			t.Errorf("line %d: %q, want %q...", i+1, got, want)
		}
	}
	if !strings.Contains(lines[1], "128KiB") {
		t.Errorf("other:1's size: %q", lines[1])
	}

	if out, _ := imageCmd("", "ls", "-q"); out != twoID+"\n"+oneID+"\n" {
		t.Errorf("-q: %q", out)
	}
	for arg, want := range map[string]int{"app": 2, "library/app": 2, "docker.io/library/app:latest": 1, "app:1": 1, "other": 1, "nothere": 0} {
		if out, _ := imageCmd("", "ls", "-q", arg); strings.Count(out, "\n") != min(want, 1) {
			t.Errorf("ls -q %s: %q", arg, out)
		}
		out, _ := imageCmd("", "ls", arg)
		if n := strings.Count(out, "\n") - 1; n != want {
			t.Errorf("ls %s: %d images, want %d:\n%s", arg, n, want, out)
		}
	}
	if out, _ := imageCmd("", "ls", "--digests"); !strings.Contains(out, "DIGEST") || !strings.Contains(out, sha("manifest one")) {
		t.Errorf("--digests:\n%s", out)
	}

	var rows []imageRow
	out, _ = imageCmd("", "ls", "--format", "json")
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 3 || rows[1].Containers != 1 || rows[2].Containers != 0 ||
		rows[0].SharedSize != 64<<10 || rows[1].ID != sha("one") || !rows[0].Complete || rows[0].Reference != "index.docker.io/library/other:1" {
		t.Errorf("json: %v\n%s", err, out)
	}
	if out, _ := imageCmd("", "ls", "--format", "json", "nothere"); strings.TrimSpace(out) != "[]" {
		t.Errorf("json of nothing: %q", out)
	}

	var infos []imageInfo
	out, code = imageCmd("", "inspect", "app:latest", oneID)
	if err := json.Unmarshal([]byte(out), &infos); err != nil || code != 0 || len(infos) != 2 {
		t.Fatalf("inspect: exit %d, %v\n%s", code, err, out)
	}
	if i := infos[1]; i.Id != sha("one") || strings.Join(i.RepoTags, " ") != "app:1 app:latest" && strings.Join(i.RepoTags, " ") != "app:latest app:1" ||
		len(i.RootFS.Layers) != 2 || i.Os != "linux" || len(i.Containers) != 1 || i.Size != 128<<10 {
		t.Errorf("inspect by ID: %+v", i)
	}
	if i := infos[0]; len(i.RepoTags) != 1 || len(i.Containers) != 0 || !strings.HasPrefix(i.RepoDigests[0], "app@sha256:") {
		t.Errorf("inspect app:latest: %+v", i)
	}
	if _, code := imageCmd("", "inspect", "nothere"); code != 1 {
		t.Errorf("inspect of a missing image: exit %d", code)
	}

	for _, args := range [][]string{{}, {"frobnicate"}, {"ls", "--format", "yaml"}, {"ls", "a", "b"}, {"rm"}, {"inspect"}, {"prune", "x"}} {
		if _, code := imageCmd("", args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

func TestRmiAndPrune(t *testing.T) {
	root, a, b, c := imageStore(t)
	has := func(d string) bool { return image.Open(root).Layers.Has(d) }

	// Refused for an image a container was made from, and for an ID two
	// references share; the rest go.
	out, code := imageCmd("", "rm", "app:1", strings.TrimPrefix(sha("one"), "sha256:")[:8], "other:1")
	if code != 1 || out != "Untagged: other:1\nDeleted: "+c+"\n" || has(c) || !has(a) {
		t.Errorf("rm: exit %d, %q", code, out)
	}
	// With -f, the record goes and the container keeps its layers.
	if out, code := imageCmd("", "rm", "-f", "app:1"); code != 0 || out != "Untagged: app:1\n" || !has(a) || !has(b) {
		t.Errorf("rm -f app:1: exit %d, %q", code, out)
	}

	// Declined, prune removes nothing.
	if out, code := imageCmd("n\n", "prune", "-a"); code != 0 || out != "" {
		t.Errorf("prune declined: exit %d, %q", code, out)
	}
	if out, code := imageCmd("", "prune", "-f"); code != 0 || out != "Total reclaimed space: 0B\n" {
		t.Errorf("prune with nothing dangling: exit %d, %q", code, out)
	}
	out, code = imageCmd("y\n", "prune", "-a")
	if code != 0 || out != "Deleted Images:\nuntagged: app:latest\n\nTotal reclaimed space: 0B\n" || !has(a) || !has(b) {
		t.Errorf("prune -a: exit %d, %q", code, out)
	}

	// Once the container is gone, its layers dangle.
	s := container.Open(root)
	cs, _ := s.List()
	if err := s.Remove(cs[0]); err != nil {
		t.Fatal(err)
	}
	out, code = imageCmd("", "prune", "-f")
	if code != 0 || !strings.Contains(out, "deleted: "+a) || !strings.Contains(out, "deleted: "+b) || !strings.HasSuffix(out, "Total reclaimed space: 128KiB\n") || has(a) || has(b) {
		t.Errorf("prune once the container is gone: exit %d, %q", code, out)
	}
	if out, _ := imageCmd("", "ls", "-q"); out != "" {
		t.Errorf("images left: %q", out)
	}
}
