package rootfs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// fakeRoot is init's file server for Put: it answers the request, reads
// the tar to its end marker, as init does, never to the connection's end,
// and answers again with what it read.
func fakeRoot(t *testing.T) (*Client, chan []string) {
	got := make(chan []string, 1)
	dial := func(context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
		host, guest := net.Pipe()
		go func() {
			defer guest.Close()
			r := bufio.NewReader(guest)
			if _, err := r.ReadBytes('\n'); err != nil {
				return
			}
			enc := json.NewEncoder(guest)
			enc.Encode(guestspec.RootReply{})
			tr := tar.NewReader(r)
			var names []string
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					enc.Encode(guestspec.RootReply{Error: err.Error()})
					return
				}
				body, err := io.ReadAll(tr)
				if err != nil {
					enc.Encode(guestspec.RootReply{Error: err.Error()})
					return
				}
				names = append(names, h.Name+"="+string(body))
			}
			got <- names
			enc.Encode(guestspec.RootReply{})
		}()
		return host, bufio.NewReader(host), nil
	}
	return &Client{Dial: dial, Name: "test"}, got
}

func tarOf(t *testing.T, end bool, files ...string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		name, body, _ := strings.Cut(f, "=")
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	if end {
		tw.Close()
	} else {
		tw.Flush()
	}
	return buf.Bytes()
}

// TestPut checks that a tar reaches the file server, one without its end
// marker too, and that what is not a tar, or ends early, is refused
// rather than leaving the server waiting for the rest.
func TestPut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, end := range []bool{true, false} {
		c, got := fakeRoot(t)
		if err := c.Put(ctx, "/", bytes.NewReader(tarOf(t, end, "a=1", "b=two")), PutOptions{}); err != nil {
			t.Fatalf("end marker %v: %v", end, err)
		}
		if names := <-got; strings.Join(names, ",") != "a=1,b=two" {
			t.Errorf("end marker %v: the server read %q", end, names)
		}
	}
	full := tarOf(t, true, "a="+strings.Repeat("x", 1000))
	for name, content := range map[string][]byte{
		"not a tar":       []byte("abc"),
		"ends in a file":  full[:700],
		"ends in a block": full[:300],
	} {
		c, _ := fakeRoot(t)
		done := make(chan error, 1)
		go func() { done <- c.Put(ctx, "/", bytes.NewReader(content), PutOptions{}) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a tar, or ends early") {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Put waits", name)
		}
	}
}
