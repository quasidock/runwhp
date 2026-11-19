//go:build windows

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]uint64{
		"512m": 512 << 20, "1g": 1 << 30, "1.5g": 1536 << 20, "300M": 300 << 20,
		"200000k":   196 << 20, // rounded up to 2 MiB
		"134217728": 128 << 20,
	} {
		got, err := parseMemory(in)
		if err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1g", "64m", "4g", "512"} {
		if _, err := parseMemory(in); err == nil {
			t.Errorf("parseMemory(%q) accepted", in)
		}
	}
}

func TestStatusWriter(t *testing.T) {
	var w statusWriter
	if _, ok := w.get(); ok {
		t.Fatal("status before any write")
	}
	w.Write([]byte(`{"exit":12`))
	if _, ok := w.get(); ok {
		t.Fatal("status from a partial line")
	}
	w.Write([]byte("7,\"error\":\"boom\"}\nnot json\n"))
	st, ok := w.get()
	if !ok || st.Exit != 127 || st.Error != "boom" {
		t.Errorf("status = %+v, %v", st, ok)
	}
}

func TestRing(t *testing.T) {
	r := &ring{max: 3}
	for _, l := range []string{"a", "b", "c", "d"} {
		r.add(l)
	}
	var buf bytes.Buffer
	r.print(&buf)
	if got := buf.String(); !strings.HasSuffix(got, "  b\n  c\n  d\n") || strings.Contains(got, "  a\n") {
		t.Errorf("ring printed %q", got)
	}
}
