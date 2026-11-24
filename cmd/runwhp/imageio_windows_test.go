//go:build windows

package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/image"
)

func rootfsTar(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "bin/hello", Mode: 0o755, Size: 5})
	tw.Write([]byte("hello"))
	tw.Close()
	p := filepath.Join(t.TempDir(), "rootfs.tar")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

// writeArchive writes files into a tar at path.
func writeArchive(t *testing.T, path string, files [][2]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1]))})
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestImportAndLoad imports a root file system, saves the image by hand
// in Docker's manifest.json layout, removes it, and loads it back.
func TestImportAndLoad(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RUNWHP_ROOT", root)
	out, code := imageCmd("", "import", "-c", `CMD ["/bin/hello"]`, "--change", "ENV A=b", "-m", "made here", rootfsTar(t), "made:1")
	id := strings.TrimSpace(out)
	if code != 0 || !image.Untagged(id) {
		t.Fatalf("import: exit %d, %q", code, out)
	}
	is := image.Open(root)
	img, err := is.Get("made:1")
	if err != nil || img.ID() != id {
		t.Fatalf("Get = %v, %v", img, err)
	}
	if hist, _ := imageCmd("", "history", "made:1"); !strings.Contains(hist, "made here") {
		t.Errorf("history:\n%s", hist)
	}

	raw, _ := is.RawConfig(img)
	layer, _ := os.ReadFile(rootfsTar(t))
	archive := filepath.Join(t.TempDir(), "made.tar")
	writeArchive(t, archive, [][2]string{
		{"manifest.json", `[{"Config":"` + id[7:] + `.json","RepoTags":["made:1"],"Layers":["layer.tar"]}]`},
		{id[7:] + ".json", string(raw)},
		{"layer.tar", string(layer)},
	})
	if _, code := imageCmd("", "rm", "made:1"); code != 0 {
		t.Fatal("rmi failed")
	}
	out, code = imageCmd("", "load", "-q", "-i", archive)
	if code != 0 || out != "Loaded image: made:1\n" {
		t.Fatalf("load: exit %d, %q", code, out)
	}
	if img, err := is.Get("made:1"); err != nil || img.ID() != id {
		t.Errorf("loaded: %v, %v", img, err)
	}
	// From stdin, spooled to the store's tmp, which is left empty.
	data, _ := os.ReadFile(archive)
	if out, code := imageCmd(string(data), "load"); code != 0 || out != "Loaded image: made:1\n" {
		t.Errorf("load from stdin: exit %d, %q", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "tmp")); len(entries) != 0 {
		t.Errorf("load left %d files in tmp", len(entries))
	}
	// An untagged import prints its ID and lists as <none>.
	out, code = imageCmd("", "import", rootfsTar(t))
	if code != 0 || !image.Untagged(strings.TrimSpace(out)) {
		t.Fatalf("untagged import: exit %d, %q", code, out)
	}
	if ls, _ := imageCmd("", "ls"); !strings.Contains(ls, "<none>") {
		t.Errorf("images:\n%s", ls)
	}
}

func TestImportRefused(t *testing.T) {
	t.Setenv("RUNWHP_ROOT", t.TempDir())
	xz := filepath.Join(t.TempDir(), "rootfs.tar.xz")
	os.WriteFile(xz, []byte{0xfd, '7', 'z', 'X', 'Z', 0, 0, 0}, 0o644)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"import"}, 2},
		{[]string{"import", "a", "b", "c"}, 2},
		{[]string{"import", "--platform", "linux/arm64", rootfsTar(t)}, 1},
		{[]string{"import", "https://example.com/rootfs.tar"}, 1},
		{[]string{"import", xz}, 1},
		{[]string{"import", "-c", "RUN true", rootfsTar(t)}, 1},
		{[]string{"import", filepath.Join(t.TempDir(), "missing.tar")}, 1},
		{[]string{"load", "extra"}, 2},
		{[]string{"load", "-i", rootfsTar(t)}, 1},
	} {
		if _, code := imageCmd("", c.args...); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
	}
}
