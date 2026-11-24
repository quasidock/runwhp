//go:build windows

package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCommit makes images of containers, as docker commit does.
func TestCommit(t *testing.T) {
	s := newOwnStore(t)
	ref := mirrored(t, alpine)
	s.ok("run", "-d", "--name", "src", "-e", "FOO=bar", "-v", "/anon", ref, "sh", "-c",
		"echo x > /x && rm /etc/issue && echo a > /anon/f && touch /ready && sleep 300")
	for deadline := time.Now().Add(15 * time.Second); s.cli("", "exec", "src", "test", "-e", "/ready").code != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the container never got ready")
		}
	}
	id := strings.TrimSpace(s.ok("commit", "-c", `CMD ["cat","/x"]`, "-m", "msg", "-a", "me", "src", "committed:1").stdout)
	if !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("commit printed %q", id)
	}
	var st stateJSON
	json.Unmarshal([]byte(s.ok("state", "src").stdout), &st)
	if st.Status != "running" {
		t.Errorf("after commit, the container is %s", st.Status)
	}
	if r := s.ok("run", "--rm", "committed:1"); r.stdout != "x\n" {
		t.Errorf("run of the committed image: %q", r.stdout)
	}
	if r := s.ok("run", "--rm", "committed:1", "sh", "-c", "test -e /etc/issue || echo gone; echo $FOO; ls /anon"); r.stdout != "gone\nbar\n" {
		t.Errorf("the committed image's files and environment: %q", r.stdout)
	}
	if r := s.ok("history", "committed:1"); !strings.Contains(r.stdout, "msg") {
		t.Errorf("history:\n%s", r.stdout)
	}

	// Saved and loaded with its ID.
	archive := filepath.Join(t.TempDir(), "c.tar")
	s.ok("save", "-o", archive, "committed:1")
	s.ok("rmi", "committed:1")
	if r := s.ok("load", "-i", archive); !strings.Contains(r.stdout, "committed:1") {
		t.Errorf("load: %q", r.stdout)
	}
	var loaded []struct{ Id string }
	if err := json.Unmarshal([]byte(s.ok("image", "inspect", "committed:1").stdout), &loaded); err != nil || len(loaded) != 1 || loaded[0].Id != id {
		t.Errorf("loaded as %+v (%v), want %s", loaded, err, id)
	}

	// Without pausing, and untagged.
	untagged := strings.TrimSpace(s.ok("commit", "--pause=false", "src").stdout)
	if r := s.ok("images"); !strings.Contains(r.stdout, "<none>") || !strings.HasPrefix(untagged, "sha256:") {
		t.Errorf("an untagged commit: %q\n%s", untagged, r.stdout)
	}

	// Stopped, and unchanged: no layer added.
	s.ok("stop", "-t", "1", "src")
	s.ok("commit", "src", "committed:2")
	if r := s.ok("run", "--rm", "committed:2", "cat", "/x"); r.stdout != "x\n" {
		t.Errorf("the stopped container's commit: %q", r.stdout)
	}
	s.ok("run", "--name", "same", ref, "true")
	s.ok("commit", "same", "same:1")
	layers := func(ref string) int {
		var out []struct{ RootFS struct{ Layers []string } }
		if err := json.Unmarshal([]byte(s.ok("image", "inspect", ref).stdout), &out); err != nil || len(out) != 1 {
			t.Fatalf("image inspect %s: %v", ref, err)
		}
		return len(out[0].RootFS.Layers)
	}
	if layers("same:1") != layers(ref) {
		t.Errorf("an unchanged container's commit has %d layers, its image %d", layers("same:1"), layers(ref))
	}
}
