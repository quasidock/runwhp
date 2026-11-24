package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// TestServeFiles answers a files request over a layer laid out by hand:
// names that are not UTF-8, ./ and .., a symlinked directory, a missing
// file and a directory, which get errors, and a large file.
func TestServeFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "os-release"), []byte("ID=test\n"), 0o644)
	os.WriteFile(filepath.Join(root, "\xff\xfeodd"), []byte("not utf-8"), 0o644)
	big := bytes.Repeat([]byte("0123456789"), 100000)
	os.WriteFile(filepath.Join(root, "big"), big, 0o644)
	os.WriteFile(filepath.Join(root, "empty"), nil, 0o644)
	os.Symlink("etc", filepath.Join(root, "link"))
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644)
	os.Symlink(outside, filepath.Join(root, "out"))

	names := []string{"etc/os-release", "./\xff\xfeodd", "big", "/etc/../etc/os-release", "missing", "etc", "link/os-release", "out/secret", "../../etc/passwd", "empty"}
	var req bytes.Buffer
	guestspec.WriteFilesRequest(&req, 1, names)
	var resp bytes.Buffer
	if err := serveFiles(&req, &resp, []string{"/nonexistent", root}); err != nil {
		t.Fatal(err)
	}
	want := []string{"ID=test\n", "not utf-8", string(big), "ID=test\n", "!", "!", "!", "!", "", ""}
	// ../../etc/passwd is cleaned to etc/passwd beneath the root, as
	// tar2ext4 cleans names, and is not there.
	want[8] = "!"
	for i, name := range names {
		size, err := guestspec.ReadFileHeader(&resp)
		var fe *guestspec.FileError
		if want[i] == "!" {
			if !errors.As(err, &fe) {
				t.Errorf("%q: size %d, %v; want an error", name, size, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		got, _ := io.ReadAll(io.LimitReader(&resp, size))
		if string(got) != want[i] {
			t.Errorf("%q: %.20q, want %.20q", name, got, want[i])
		}
	}
	if resp.Len() != 0 {
		t.Errorf("%d bytes left", resp.Len())
	}

	var bad bytes.Buffer
	guestspec.WriteFilesRequest(&bad, 2, names)
	if err := serveFiles(&bad, io.Discard, []string{root}); err == nil || !strings.Contains(err.Error(), "layer 2") {
		t.Errorf("request for a layer out of range: %v", err)
	}
}
