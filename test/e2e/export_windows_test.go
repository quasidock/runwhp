//go:build windows

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestExport writes a container's root file system, running and stopped,
// with its changes, without its volumes or anonymous volumes' files.
func TestExport(t *testing.T) {
	parallel(t)
	vol := newVolume(t)
	name := newName(t)
	detached(t, "--name", name, "-v", vol+":/data", "-v", "/anon", alpine, "sh", "-c",
		"echo new > /newfile && rm /etc/issue && echo v > /data/v && echo a > /anon/f && touch /ready && sleep 300")
	waitExec(t, name, "test", "-e", "/ready")
	out := filepath.Join(t.TempDir(), "e.tar")
	expect(t, cli(t, "export", "-o", out, name), "", 0)
	data, _ := os.ReadFile(out)
	running := tarNames(t, data)
	for _, want := range []string{"newfile", "bin/busybox", "anon/", "data/", "etc/resolv.conf", "proc/"} {
		if !slices.Contains(running, want) {
			t.Errorf("export lacks %s", want)
		}
	}
	for _, not := range []string{"etc/issue", "data/v", "anon/f", "proc/self/"} {
		if slices.Contains(running, not) {
			t.Errorf("export has %s", not)
		}
	}
	expect(t, cli(t, "stop", "-t", "1", name), name+"\n", 0)
	r := cli(t, "export", name)
	if r.code != 0 {
		t.Fatalf("export of a stopped container: %+v", r)
	}
	if stopped := tarNames(t, []byte(r.stdout)); !slices.Equal(stopped, running) {
		t.Errorf("stopped and running exports differ:\n%q\n%q", stopped, running)
	}
}

// TestDiff lists a container's changes as docker diff does.
func TestDiff(t *testing.T) {
	parallel(t)
	vol := newVolume(t)
	name := newName(t)
	if r := keep(t, "--name", name, "-v", vol+":/data", "-v", "/anon", "-w", "/newdir", alpine, "sh", "-c",
		"echo new > /newfile && rm /etc/issue && rm -rf /home && mkdir /home && echo h > /home/h && echo a > /anon/f && touch /root && chmod 600 /etc/shells && mkdir -p /a/b && echo x > /a/b/c"); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	want := strings.Join([]string{
		"A /a", "A /a/b", "A /a/b/c", "A /anon", "A /data", "C /etc", "D /etc/issue", "C /etc/shells",
		"C /home", "A /home/h", "A /newdir", "A /newfile",
	}, "\n") + "\n"
	expect(t, cli(t, "diff", name), want, 0)

	// Running: the same server, through the container's runwhp.
	running := newName(t)
	detached(t, "--name", running, alpine, "sh", "-c", "echo x > /x && sleep 300")
	waitExec(t, running, "test", "-e", "/x")
	expect(t, cli(t, "diff", running), "A /x\n", 0)

	// An image without /proc, /sys, /dev or /etc: init's mount points are
	// not the container's changes.
	s := newOwnStore(t)
	ref := mirrored(t, busybox)
	s.ok("run", "--name", "bb", ref, "true")
	if r := s.ok("diff", "bb"); r.stdout != "" {
		t.Errorf("diff of busybox true: %q", r.stdout)
	}
}
