//go:build windows

package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// TestSessionHeaders checks that a /session request's headers that
// describe the session reach BuildKit, and no others: HTTP/1's hop-by-hop
// headers mean nothing over HTTP/2.
func TestSessionHeaders(t *testing.T) {
	h := http.Header{
		"X-Docker-Expose-Session-Uuid":        {"abc"},
		"X-Docker-Expose-Session-Name":        {"ctx"},
		"X-Docker-Expose-Session-Sharedkey":   {"k"},
		"X-Docker-Expose-Session-Grpc-Method": {"/moby.filesync.v1.FileSync/DiffCopy", "/moby.buildkit.secrets.v1.Secrets/GetSecret"},
		"X-Buildkit-Session-Foo":              {"bar"},
		"Connection":                          {"Upgrade"},
		"Upgrade":                             {"h2c"},
		"User-Agent":                          {"buildx"},
	}
	got := sessionHeaders(h)
	want := map[string][]string{
		"X-Docker-Expose-Session-Uuid":        {"abc"},
		"X-Docker-Expose-Session-Name":        {"ctx"},
		"X-Docker-Expose-Session-Sharedkey":   {"k"},
		"X-Docker-Expose-Session-Grpc-Method": {"/moby.filesync.v1.FileSync/DiffCopy", "/moby.buildkit.secrets.v1.Secrets/GetSecret"},
		"X-Buildkit-Session-Foo":              {"bar"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// TestUpgradeSplice checks /grpc's and /session's take-over: a 101 for
// h2c, as the moby client requires, then bytes both ways between the
// client and the builder, those the client sent with its request first.
func TestUpgradeSplice(t *testing.T) {
	builderEnd, ours := net.Pipe()
	// The builder: echoes what it reads, upper-cased.
	go func() {
		r := bufio.NewReader(builderEnd)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				builderEnd.Close()
				return
			}
			builderEnd.Write([]byte(strings.ToUpper(line)))
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := upgrade(w, "h2c")
		if err != nil {
			t.Error(err)
			return
		}
		spliceConns(context.Background(), conn, brw.Reader, ours)
	}))
	defer srv.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	// The first line is sent with the request, before the 101 is read.
	io.WriteString(c, "POST /grpc HTTP/1.1\r\nHost: runwhp\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\nfirst\n")
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "h2c" {
		t.Fatalf("answered %s, Upgrade %q", resp.Status, resp.Header.Get("Upgrade"))
	}
	io.WriteString(c, "second\n")
	for _, want := range []string{"FIRST\n", "SECOND\n"} {
		if got, err := r.ReadString('\n'); got != want || err != nil {
			t.Fatalf("read %q, %v; want %q", got, err, want)
		}
	}
}

// TestLogLines checks the builder's pull progress as the server logs it:
// a line at a time, carriage returns too.
func TestLogLines(t *testing.T) {
	var lines []string
	s := &apiServer{log: writerFunc(func(p []byte) (int, error) {
		lines = append(lines, string(p))
		return len(p), nil
	})}
	l := &logLines{s: s, prefix: "builder: "}
	io.WriteString(l, "pulling a")
	io.WriteString(l, "\rpulled a\nlast")
	if len(lines) != 2 || !strings.Contains(lines[0], "builder: pulling a") || !strings.Contains(lines[1], "builder: pulled a") {
		t.Errorf("logged %q", lines)
	}
}

// TestBuilderNotRemovable checks that no prune removes the builder, which
// is the server's, whatever its state.
func TestBuilderNotRemovable(t *testing.T) {
	c := &container.Container{Config: &container.Config{}}
	c.State.Status = container.Stopped
	if !gc.Removable(c) {
		t.Fatal("a stopped container is not removable")
	}
	c.Config.Labels = map[string]string{guestspec.BuilderLabel: "buildkit"}
	if gc.Removable(c) || !isBuilder(c) {
		t.Error("the builder is removable")
	}
}
