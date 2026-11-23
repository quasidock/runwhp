//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/volume"
)

// systemStore is imageStore's store with more in it: a layer nothing lists,
// a volume a stopped container uses and one nothing uses, and the kernel
// this runwhp boots beside an old one.
func systemStore(t *testing.T) (root, dangling string) {
	root, _, _, _ = imageStore(t)
	dangling = sha("layer D")
	p := filepath.Join(root, "layers", "sha256", strings.TrimPrefix(dangling, "sha256:")+".ext4")
	os.WriteFile(p, make([]byte, 64<<10), 0o644)

	vs := volume.Open(root)
	for _, name := range []string{"used", "free"} {
		if _, err := vs.Create(name, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	c := containerIn(t, root, "volumes", container.Stopped, 64<<10)
	cfg := filepath.Join(c.Dir(), "config.json")
	c.Config.Volumes = []container.Volume{{Name: "used", Size: 64 << 20}}
	data, _ := json.Marshal(c.Config)
	os.WriteFile(cfg, data, 0o644)

	ks := kernel.Open(root)
	for _, d := range []string{kernel.Default.Digest, sha("old kernel")} {
		dir, _ := ks.Dir(d)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "bzImage"), make([]byte, 64<<10), 0o644)
	}
	return root, dangling
}

func systemCmd(stdin string, args ...string) (string, int) {
	var out bytes.Buffer
	code := cmdSystem(args, strings.NewReader(stdin), &out)
	return out.String(), code
}

func TestSystemDf(t *testing.T) {
	systemStore(t)
	out, code := systemCmd("", "df")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 5 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	fields := func(i int) string { return strings.Join(strings.Fields(lines[i]), " ") }
	// Layers A, B, C and D; C and D go with an image prune -a.
	if got := fields(1); got != "Images 3 1 256KiB 128KiB (50%)" {
		t.Errorf("images: %q", got)
	}
	// Two stopped containers; only one has a writable layer.
	if got := fields(2); got != "Containers 2 0 64KiB 64KiB (100%)" {
		t.Errorf("containers: %q", got)
	}
	if got := fields(3); !strings.HasPrefix(got, "Local Volumes 2 1 ") || !strings.HasSuffix(got, "(50%)") {
		t.Errorf("volumes: %q", got)
	}
	if got := fields(4); got != "Kernels 2 1 128KiB 64KiB (50%)" {
		t.Errorf("kernels: %q", got)
	}

	out, _ = systemCmd("", "df", "-v")
	for _, want := range []string{"Images space usage:", "SHARED SIZE", "Layers no image lists: 0 kept by containers, 0B; 1 dangling, 64KiB",
		"Containers space usage:", "Local Volumes space usage:", "Kernels space usage:"} {
		if !strings.Contains(out, want) {
			t.Errorf("df -v lacks %q:\n%s", want, out)
		}
	}

	var js struct {
		Summary []dfRow
		Images  []struct{ Containers int }
		Volumes []struct {
			Name  string
			Links int
		}
	}
	out, _ = systemCmd("", "df", "--format", "json")
	if err := json.Unmarshal([]byte(out), &js); err != nil || len(js.Summary) != 4 || js.Summary[0].Reclaimable != 128<<10 || len(js.Images) != 3 || len(js.Volumes) != 2 {
		t.Errorf("json: %v\n%s", err, out)
	}

	for _, args := range [][]string{{}, {"frobnicate"}, {"df", "x"}, {"df", "--format", "yaml"}, {"prune", "x"}} {
		if _, code := systemCmd("", args...); code != 2 {
			t.Errorf("system %q: exit %d, want 2", args, code)
		}
	}
}

func TestSystemPrune(t *testing.T) {
	root, dangling := systemStore(t)
	if out, code := systemCmd("n\n", "prune"); code != 0 || out != "" {
		t.Errorf("declined: exit %d, %q", code, out)
	}

	// The containers go, then the layer nothing lists and the old kernel;
	// images stay, and volumes.
	out, code := systemCmd("y\n", "prune")
	if code != 0 || strings.Count(out, "\n") != 11 || !strings.Contains(out, "Deleted Images:\ndeleted: "+dangling+"\n\n") ||
		!strings.Contains(out, "Deleted Kernels:\n"+sha("old kernel")+"\n") || strings.Contains(out, "Volumes") ||
		!strings.HasSuffix(out, "Total reclaimed space: 192KiB\n") {
		t.Errorf("prune: exit %d\n%s", code, out)
	}
	if cs, _ := container.Open(root).List(); len(cs) != 0 {
		t.Errorf("containers left: %d", len(cs))
	}

	// With -a and --volumes, every image and volume goes; the kernel
	// this runwhp boots stays.
	out, code = systemCmd("", "prune", "-a", "--volumes", "-f")
	if code != 0 || !strings.Contains(out, "untagged: app:1\n") || !strings.Contains(out, "untagged: other:1\n") ||
		!strings.Contains(out, "Deleted Volumes:\n") || strings.Contains(out, "Kernels") {
		t.Errorf("prune -a --volumes: exit %d\n%s", code, out)
	}
	_, u, _ := loadStore()
	if len(u.Images) != 0 || len(u.Layers) != 0 || len(u.Volumes) != 0 || len(u.Kernels) != 1 || u.Kernels[0].Digest != kernel.Default.Digest {
		t.Errorf("left: %d images, %d layers, %d volumes, %d kernels", len(u.Images), len(u.Layers), len(u.Volumes), len(u.Kernels))
	}
}
