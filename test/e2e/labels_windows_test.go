//go:build windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLabels covers labels on containers, from the image, --label-file and
// --label, list's filters, and labels on networks, volumes and commits.
func TestLabels(t *testing.T) {
	s := newOwnStore(t)
	base := mirrored(t, alpine)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\nLABEL img.a=1 img.b=image\n"), 0o644)
	s.ok("build", "-q", "-t", "lbl:1", dir)
	file := filepath.Join(dir, "labels")
	os.WriteFile(filepath.Join(dir, "labels"), []byte("# comment\nlf.a=1\nimg.b=file\n"), 0o644)

	run := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(s.ok(append([]string{"run", "-d"}, withFlags(args)...)...).stdout)
	}
	l1 := run("--name", "l1", "--label-file", file, "--label", "img.b=flag", "-l", "lf.a=flag", "--label", "bare", "lbl:1", "sleep", "300")
	run("--name", "l2", "--label", "tier=back", alpine, "sleep", "300")
	var st struct{ Labels map[string]string }
	json.Unmarshal([]byte(s.ok("state", "l1").stdout), &st)
	want := map[string]string{"img.a": "1", "img.b": "flag", "lf.a": "flag", "bare": ""}
	if !reflect.DeepEqual(st.Labels, want) {
		t.Errorf("l1's labels: %v, want %v", st.Labels, want)
	}

	names := func(args ...string) string {
		t.Helper()
		r := s.ok(append([]string{"list", "--format", "json"}, args...)...)
		var list []struct{ Name string }
		json.Unmarshal([]byte(r.stdout), &list)
		var out []string
		for _, c := range list {
			out = append(out, c.Name)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-f", "label=tier=back"}, "l2"},
		{[]string{"--filter", "label=bare"}, "l1"},
		{[]string{"-f", "label=lf.a=flag", "-f", "label=img.a=1"}, "l1"},
		{[]string{"-f", "label=lf.a=flag", "-f", "label=tier=back"}, ""},
		{[]string{"-f", "name=^l1$", "-f", "name=^l2$"}, "l2 l1"},
		{[]string{"-f", "status=running", "-f", "label=img.a"}, "l1"},
		{[]string{"-f", "ancestor=lbl:1"}, "l1"},
		{[]string{"-f", "network=user", "-f", "name=l2"}, "l2"},
	} {
		if got := names(c.args...); got != c.want {
			t.Errorf("list %q: %q, want %q", c.args, got, c.want)
		}
	}
	if got := s.ok("list", "-q", "-f", "id="+l1[:8]).stdout; strings.TrimSpace(got) != l1 {
		t.Errorf("list -q -f id=: %q", got)
	}
	if r := s.cli("", "list", "-f", "status=bogus"); r.code != 2 || !strings.Contains(r.stderr, "invalid value for state (bogus)") {
		t.Errorf("a bad status: %+v", r)
	}
	if r := s.cli("", "run", "--rm", "--label", "=x", alpine, "true"); r.code == 0 || !strings.Contains(r.stderr, "invalid label '=x': empty name") {
		t.Errorf("an empty label name: %+v", r)
	}

	// Committed, a container's labels are its image's.
	s.ok("commit", "l1", "lbl:committed")
	var img []struct {
		Config struct{ Labels map[string]string }
	}
	json.Unmarshal([]byte(s.ok("image", "inspect", "lbl:committed").stdout), &img)
	if len(img) != 1 || !reflect.DeepEqual(img[0].Config.Labels, want) {
		t.Errorf("the committed image's labels: %+v", img)
	}

	// Networks and volumes.
	s.ok("network", "create", "--label", "a=1", "--label", "b", "lnet")
	var nets []struct{ Labels map[string]string }
	json.Unmarshal([]byte(s.ok("network", "inspect", "lnet").stdout), &nets)
	if len(nets) != 1 || !reflect.DeepEqual(nets[0].Labels, map[string]string{"a": "1", "b": ""}) {
		t.Errorf("network labels: %+v", nets)
	}
	if got := s.ok("network", "ls", "-q", "--no-trunc", "-f", "label=a=1").stdout; strings.Count(got, "\n") != 1 {
		t.Errorf("network ls -f label=a=1: %q", got)
	}
	if got := s.ok("network", "ls", "-q", "-f", "type=builtin").stdout; strings.Count(got, "\n") != 2 {
		t.Errorf("network ls -f type=builtin: %q, want user and none", got)
	}
	s.ok("volume", "create", "--label", "a=1", "lvol")
	s.ok("volume", "create", "plain")
	var vols []struct{ Labels map[string]string }
	json.Unmarshal([]byte(s.ok("volume", "inspect", "lvol").stdout), &vols)
	if len(vols) != 1 || vols[0].Labels["a"] != "1" {
		t.Errorf("volume labels: %+v", vols)
	}
	if got := s.ok("volume", "ls", "-q", "-f", "label=a").stdout; got != "lvol\n" {
		t.Errorf("volume ls -f label=a: %q", got)
	}
	run("--name", "l3", "-v", "plain:/p", alpine, "sleep", "300")
	if got := s.ok("volume", "ls", "-q", "-f", "dangling=true").stdout; got != "lvol\n" {
		t.Errorf("volume ls -f dangling=true: %q", got)
	}
}
