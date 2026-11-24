package main

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func testTar(t *testing.T) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: 3000, Typeflag: tar.TypeReg})
	tw.Write(bytes.Repeat([]byte("x"), 3000))
	tw.Close()
	return buf.Bytes()
}

// TestCutTar fails a tar the file server ended early, as export and
// commit read it.
func TestCutTar(t *testing.T) {
	whole := testTar(t)
	var out bytes.Buffer
	if err := copyTar(&out, bytes.NewReader(whole)); err != nil || !bytes.Equal(out.Bytes(), whole) {
		t.Errorf("copyTar of a whole tar: %v, %d bytes of %d", err, out.Len(), len(whole))
	}
	for _, cut := range []int{100, 512, 2000, len(whole) - 1024} {
		if err := copyTar(io.Discard, bytes.NewReader(whole[:cut])); err == nil || !strings.Contains(err.Error(), "early") {
			t.Errorf("copyTar of %d bytes of %d: %v", cut, len(whole), err)
		}
		if _, err := io.ReadAll(&tarChecker{r: bytes.NewReader(whole[:cut])}); err == nil {
			t.Errorf("tarChecker of %d bytes of %d passed", cut, len(whole))
		}
	}
	if _, err := io.ReadAll(&tarChecker{r: bytes.NewReader(whole)}); err != nil {
		t.Errorf("tarChecker of a whole tar: %v", err)
	}
}
