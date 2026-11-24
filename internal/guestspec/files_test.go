package guestspec

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestFilesRequest(t *testing.T) {
	names := []string{"etc/os-release", "usr/lib/\xff\xfeodd", strings.Repeat("d/", 2000) + "f", ""}
	for range 5000 {
		names = append(names, "many")
	}
	var buf bytes.Buffer
	if err := WriteFilesRequest(&buf, 7, names); err != nil {
		t.Fatal(err)
	}
	layer, got, err := ReadFilesRequest(&buf)
	if err != nil || layer != 7 || !reflect.DeepEqual(got, names) {
		t.Errorf("ReadFilesRequest = %d, %d names, %v", layer, len(got), err)
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes left", buf.Len())
	}
	if _, _, err := ReadFilesRequest(bytes.NewReader([]byte{0, 0, 0, 1, 0xff, 0, 0, 0})); err == nil {
		t.Error("read a request for 4 billion files")
	}
	if _, _, err := ReadFilesRequest(bytes.NewReader([]byte{0, 0, 0, 1, 0, 0, 0, 1, 0xff, 0, 0, 0})); err == nil {
		t.Error("read a name of 4 GB")
	}
}

func TestFileHeaders(t *testing.T) {
	var buf bytes.Buffer
	WriteFileHeader(&buf, 5)
	buf.WriteString("hello")
	WriteFileError(&buf, "etc/gone: no such file")
	WriteFileHeader(&buf, 0)
	if n, err := ReadFileHeader(&buf); err != nil || n != 5 {
		t.Fatalf("first: %d, %v", n, err)
	}
	if b, _ := io.ReadAll(io.LimitReader(&buf, 5)); string(b) != "hello" {
		t.Fatalf("contents %q", b)
	}
	var fe *FileError
	if _, err := ReadFileHeader(&buf); !errors.As(err, &fe) || fe.Msg != "etc/gone: no such file" {
		t.Errorf("error: %v", err)
	}
	if n, err := ReadFileHeader(&buf); err != nil || n != 0 {
		t.Errorf("empty: %d, %v", n, err)
	}
	if _, err := ReadFileHeader(&buf); err == nil {
		t.Error("read past the end")
	}
}
