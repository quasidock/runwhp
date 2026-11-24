//go:build windows

package main

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
)

func TestBinaryURI(t *testing.T) {
	// As nerdctl run -d gives it.
	u, ok := binaryURI("binary:///C:/Users/me/containerd/bin/nerdctl.exe?_NERDCTL_INTERNAL_LOGGING=C%3A%5Cdata%5C305c82c4")
	if !ok {
		t.Fatal("not taken as a binary URI")
	}
	p, err := uriPath(u)
	if err != nil || p != `C:\Users\me\containerd\bin\nerdctl.exe` {
		t.Errorf("path %q, %v", p, err)
	}
	if got := loggerArgs(u); !reflect.DeepEqual(got, []string{"_NERDCTL_INTERNAL_LOGGING", `C:\data\305c82c4`}) {
		t.Errorf("args %q", got)
	}
	u, _ = url.Parse("binary-v2:///C:/log.exe?a=1&flag")
	args := loggerArgs(u)
	sort.Strings(args)
	if !reflect.DeepEqual(args, []string{"1", "a", "flag"}) {
		t.Errorf("args %q", args)
	}
	for _, s := range []string{`\\.\pipe\ctr-x-stdout`, "file:///C:/x.log", ""} {
		if _, ok := binaryURI(s); ok {
			t.Errorf("%q taken as a binary URI", s)
		}
	}
	for _, s := range []string{"binary://server/share/x.exe", "binary:///relative.exe"} {
		u, _ := url.Parse(s)
		if p, err := uriPath(u); err == nil {
			t.Errorf("%s gave path %q", s, p)
		}
	}
}

func TestFileStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "out.log")
	u := "file:///" + filepath.ToSlash(path)
	for _, line := range []string{"one\n", "two\n"} {
		pio, err := openIO("ns", "id", "", u, "", false)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(pio.stdout, line)
		pio.close()
	}
	if data, _ := os.ReadFile(path); string(data) != "one\ntwo\n" {
		t.Errorf("the file holds %q", data)
	}
	if _, err := openIO("ns", "id", "", "fifo:///x", "", false); err == nil {
		t.Error("an unknown stream was taken")
	}
}

func TestStdinReaderClose(t *testing.T) {
	r, w := io.Pipe()
	in := &stdinReader{r: r}
	go w.Write([]byte("abc"))
	b := make([]byte, 8)
	if n, err := in.Read(b); n != 3 || err != nil {
		t.Fatalf("read %d, %v", n, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := in.Read(b)
		done <- err
	}()
	in.close() // as CloseIO does, with a read under way
	if err := <-done; err != io.EOF {
		t.Errorf("a read under way returned %v, want EOF", err)
	}
	if _, err := in.Read(b); err != io.EOF {
		t.Errorf("a read after close returned %v", err)
	}
}

func TestViewless(t *testing.T) {
	sn, err := newSnapshotter(t.TempDir())
	if err != nil {
		if strings.Contains(err.Error(), "NTFS") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	defer sn.Close()
	ctx := context.Background()
	// An unpacked layer: prepared, then committed.
	ms, err := sn.Prepare(ctx, "extract-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Type != "lcow-layer" {
		t.Fatalf("prepare gave %+v", ms)
	}
	if err := sn.Commit(ctx, "layer-1", "extract-1"); err != nil {
		t.Fatal(err)
	}
	// nerdctl's view of the image has no mounts.
	if ms, err := sn.View(ctx, "view-1", "layer-1"); err != nil || len(ms) != 0 {
		t.Errorf("view gave %+v, %v", ms, err)
	}
	if ms, err := sn.Mounts(ctx, "view-1"); err != nil || len(ms) != 0 {
		t.Errorf("the view's mounts are %+v, %v", ms, err)
	}
	// A container's snapshot has the layer's mount, with the layer as
	// its parent.
	ms, err = sn.Prepare(ctx, "container-1", "layer-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Type != "lcow-layer" || !strings.Contains(strings.Join(ms[0].Options, " "), "parentLayerPaths=") {
		t.Errorf("the container's mounts are %+v", ms)
	}
	if _, err := os.Stat(filepath.Join(ms[0].Source, "sandbox.vhdx")); err != nil {
		t.Errorf("no sandbox.vhdx copied from the empty scratch.vhdx: %v", err)
	}
	info, err := sn.Stat(ctx, "container-1")
	if err != nil || info.Kind != snapshots.KindActive {
		t.Errorf("stat %+v, %v", info, err)
	}
}
