//go:build windows

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

// containerIn records a container in root with the given status and a
// writable layer of size bytes. A running one holds its lock until the
// test ends, as its runwhp would.
func containerIn(t *testing.T, root, name string, status container.Status, size int) *container.Container {
	t.Helper()
	s := container.Open(root)
	cfg := &container.Config{Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(), Args: []string{"true"}}
	c, err := s.Create(cfg, name)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.RW(), make([]byte, size), 0o644)
	l, err := s.Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	l.WriteState(container.State{Status: status, Pid: os.Getpid()})
	if status == container.Running {
		t.Cleanup(func() { l.Close() })
	} else {
		l.Close()
	}
	return c
}

func TestContainerPrune(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RUNWHP_ROOT", root)
	a := containerIn(t, root, "first", container.Stopped, 64<<10)
	b := containerIn(t, root, "second", container.Stopped, 128<<10)
	running := containerIn(t, root, "running", container.Running, 64<<10)

	prune := func(stdin string, args ...string) (string, int) {
		var out bytes.Buffer
		code := cmdContainer(append([]string{"prune"}, args...), strings.NewReader(stdin), &out)
		return out.String(), code
	}
	if out, code := prune("no\n"); code != 0 || out != "" {
		t.Errorf("declined: exit %d, %q", code, out)
	}
	out, code := prune("", "-f")
	if code != 0 || !strings.HasPrefix(out, "Deleted Containers:\n") || !strings.Contains(out, a.ID+"\n") || !strings.Contains(out, b.ID+"\n") ||
		strings.Contains(out, running.ID) || !strings.HasSuffix(out, "\nTotal reclaimed space: 192KiB\n") {
		t.Errorf("prune -f: exit %d\n%s", code, out)
	}
	cs, _ := container.Open(root).List()
	if len(cs) != 1 || cs[0].ID != running.ID {
		t.Errorf("left: %v", cs)
	}
	if out, _ := prune("y\n"); out != "Total reclaimed space: 0B\n" {
		t.Errorf("again: %q", out)
	}

	for _, args := range [][]string{{}, {"frobnicate"}, {"prune", "extra"}} {
		if code := cmdContainer(args, strings.NewReader(""), new(bytes.Buffer)); code != 2 {
			t.Errorf("container %q: exit %d, want 2", args, code)
		}
	}
	var ls bytes.Buffer
	if code := cmdContainer([]string{"ls", "-q"}, nil, &ls); code != 0 || ls.String() != running.ID+"\n" {
		t.Errorf("container ls -q: exit %d, %q", code, ls.String())
	}
}
