//go:build windows

package main

import (
	"errors"
	"io"
	"testing"
)

func TestDetachFilter(t *testing.T) {
	keys, _ := parseDetachKeys(defaultDetachKeys)
	for _, c := range []struct {
		in       []string
		want     string
		detached bool
	}{
		{[]string{"ab", "c"}, "abc", false},
		{[]string{"ab\x10\x11cd"}, "ab", true},
		{[]string{"ab\x10", "\x11"}, "ab", true},   // split across reads
		{[]string{"a\x10b"}, "a\x10b", false},      // Ctrl+P alone passes through
		{[]string{"a\x10\x10\x11"}, "a\x10", true}, // Ctrl+P twice, then Ctrl+Q
		{[]string{"a\x10"}, "a\x10", false},        // a held Ctrl+P at the end of input
	} {
		in := chunks(c.in)
		f := &detachFilter{r: &in, keys: keys}
		var got []byte
		var err error
		buf := make([]byte, 3)
		for {
			var n int
			n, err = f.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		if string(got) != c.want || errors.Is(err, errDetached) != c.detached {
			t.Errorf("%q: read %q, %v; want %q, detached %v", c.in, got, err, c.want, c.detached)
		}
	}
}

func TestParseDetachKeys(t *testing.T) {
	for in, want := range map[string]string{
		"ctrl-p,ctrl-q": "\x10\x11",
		"ctrl-a":        "\x01",
		"CTRL-Z,x":      "\x1ax",
		"ctrl-@,ctrl-_": "\x00\x1f",
		"ctrl-[":        "\x1b",
	} {
		if got, err := parseDetachKeys(in); err != nil || string(got) != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ctrl-", "ctrl-1", "ab", "ctrl-p,"} {
		if got, err := parseDetachKeys(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

// chunks is a reader returning one string per read.
type chunks []string

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}
