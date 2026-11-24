package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLogOptions(t *testing.T) {
	for _, c := range []struct {
		driver string
		opts   []string
		want   logConfig
		err    string
	}{
		{"", nil, logConfig{}, ""},
		{"local", []string{"max-size=10m", "max-file=3"}, logConfig{maxSize: 10 << 20, maxFiles: 3}, ""},
		{"", []string{"max-size=-1"}, logConfig{maxSize: -1}, ""},
		{"", []string{"MAX-SIZE=1.5k"}, logConfig{}, "at least 4k"},
		{"", []string{"compress=false"}, logConfig{}, ""},
		{"", []string{"compress=true"}, logConfig{}, "does not compress"},
		{"", []string{"max-file=0"}, logConfig{}, "1 to 1000"},
		{"", []string{"max-size"}, logConfig{}, "KEY=VALUE"},
		{"", []string{"labels=a"}, logConfig{}, "unknown option"},
		{"none", nil, logConfig{driver: "none"}, ""},
		{"none", []string{"max-file=2"}, logConfig{}, "takes no options"},
		{"json-file", nil, logConfig{}, "own log format"},
		{"bogus", nil, logConfig{}, "unknown log driver"},
	} {
		got, err := parseLogOptions(c.driver, c.opts)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q %q: error %v, want %q", c.driver, c.opts, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q %q = %+v, %v; want %+v", c.driver, c.opts, got, err, c.want)
		}
	}
}

func TestDiagLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runwhp.log")
	d, err := openDiagLog(path)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 1000)
	line[len(line)-1] = '\n'
	for range 2*diagLogMax/len(line) + 10 {
		if _, err := d.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > diagLogMax || fi.Size() == 0 {
			t.Errorf("%s: %v, %v", p, fi.Size(), err)
		}
	}
	if _, err := os.Stat(path + ".2"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a third file: %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 2, errors.New("broken pipe") }

func TestTeeWriter(t *testing.T) {
	var log bytes.Buffer
	n, err := teeWriter{failWriter{}, &log}.Write([]byte("hello"))
	if n != 2 || err == nil || log.String() != "he" {
		t.Errorf("tee: %d, %v, log %q", n, err, log.String())
	}
}

func TestParseLogTime(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"", time.Time{}, true},
		{"10m", now.Add(-10 * time.Minute), true},
		{"1h30m", now.Add(-90 * time.Minute), true},
		{"-5m", time.Time{}, false},
		{"1764000000", time.Unix(1764000000, 0), true},
		{"1764000000.5", time.Unix(1764000000, 5e8), true},
		{"1764000000.000000001", time.Unix(1764000000, 1), true},
		{"2026-11-24T05:55:57.718492400Z", time.Date(2026, 11, 24, 5, 55, 57, 718492400, time.UTC), true},
		{"2026-11-24T12:00:00+07:00", time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC), true},
		{"2026-11-24T12:00:00", time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local), true},
		{"2026-11-24", time.Date(2026, 11, 24, 0, 0, 0, 0, time.Local), true},
		{"yesterday", time.Time{}, false},
		{"12.x", time.Time{}, false},
	} {
		got, err := parseLogTime(c.in, now)
		if (err == nil) != c.ok || !got.Equal(c.want) {
			t.Errorf("parseLogTime(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for in, want := range map[string]int{"all": -1, "": -1, "ALL": -1, "0": 0, "25": 25} {
		if got, err := parseTail(in); got != want || err != nil {
			t.Errorf("parseTail(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "x", "1.5"} {
		if _, err := parseTail(in); err == nil {
			t.Errorf("parseTail(%q) took it", in)
		}
	}
}
