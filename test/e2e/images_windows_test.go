//go:build windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTag names an image anew, runs it by that name and by its ID, and
// removes one name of two.
func TestTag(t *testing.T) {
	s := newOwnStore(t)
	alpine := mirrored(t, alpine)
	s.ok("pull", alpine)
	id := configDigest(t, alpine)

	s.ok("tag", alpine, "mine/app:1")
	if r := s.ok(runArgs("mine/app:1", "cat", "/etc/alpine-release")...); !strings.HasPrefix(r.stdout, "3.20") {
		t.Errorf("run by the new tag: %q", r.stdout)
	}
	if r := s.ok(runArgs(id[7:19], "cat", "/etc/alpine-release")...); !strings.HasPrefix(r.stdout, "3.20") {
		t.Errorf("run by ID: %q", r.stdout)
	}
	rows := map[string][]string{}
	for _, l := range strings.Split(s.ok("images", "--digests").stdout, "\n")[1:] {
		if f := strings.Fields(l); len(f) > 3 {
			rows[f[0]+":"+f[1]] = f
		}
	}
	mine, pulled := rows["mine/app:1"], rows[repoOf(alpine)+":3.20"]
	if mine == nil || pulled == nil || mine[3] != id[7:19] || pulled[3] != id[7:19] {
		t.Fatalf("images --digests: %v", rows)
	}
	if mine[2] != "<none>" || !strings.HasPrefix(pulled[2], "sha256:") {
		t.Errorf("digests: the tag's %s, the pull's %s", mine[2], pulled[2])
	}
	layers := s.layers()
	if r := s.ok("rmi", "mine/app:1"); !strings.Contains(r.stdout, "Untagged: mine/app:1") || strings.Contains(r.stdout, "Deleted") {
		t.Errorf("rmi of one tag of two:\n%s", r.stdout)
	}
	if got := s.layers(); len(got) != len(layers) {
		t.Errorf("rmi of one tag removed layers: %v, then %v", layers, got)
	}
	s.ok(runArgs(alpine, "true")...)
	if r := s.cli("", "tag", "nothere:1", "x:1"); r.code != 1 || !strings.Contains(r.stderr, "no such image") {
		t.Errorf("tag of a missing image: %+v", r)
	}
}

// TestHistory shows how alpine was built, with its layer's size as Docker
// gives it: the size of its files.
func TestHistory(t *testing.T) {
	s := newOwnStore(t)
	alpine := mirrored(t, alpine)
	s.ok("pull", alpine)
	id := configDigest(t, alpine)

	lines := strings.Split(strings.TrimSpace(s.ok("history", "-H=false", alpine).stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "IMAGE") || !strings.HasPrefix(lines[1], id[7:19]) || !strings.HasPrefix(lines[2], "<missing>") {
		t.Fatalf("history:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], `CMD ["/bin/sh"]`) || !strings.Contains(lines[2], "ADD alpine-minirootfs") {
		t.Errorf("history:\n%s", strings.Join(lines, "\n"))
	}
	// The size Docker 29 gave alpine:3.20's layer.
	if f := strings.Fields(lines[2]); !contains(f, "7807582") {
		t.Errorf("the layer's size: %q", lines[2])
	}
	// A layer pulled before runwhp kept its tar's metadata shows its ext4
	// image's size instead.
	splits, _ := filepath.Glob(filepath.Join(s.root, "layers", "sha256", "*.tar-split.gz"))
	for _, p := range splits {
		os.Remove(p)
	}
	lines = strings.Split(strings.TrimSpace(s.ok("history", "-H=false", alpine).stdout), "\n")
	if f := strings.Fields(lines[2]); contains(f, "7807582") {
		t.Errorf("the size without metadata: %q", lines[2])
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
