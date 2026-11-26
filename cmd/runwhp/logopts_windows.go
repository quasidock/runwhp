package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

// logConfig is what --log-driver and --log-opt set.
type logConfig struct {
	driver   string
	maxSize  int64
	maxFiles int
}

// parseLogOptions reads --log-driver and --log-opt as Docker's local
// driver takes them: max-size (a size, or -1 for no bound), max-file, and
// compress=false, since runwhp does not compress its logs.
func parseLogOptions(driver string, opts []string) (logConfig, error) {
	var lc logConfig
	switch strings.ToLower(driver) {
	case "", container.LogDriverLocal:
	case container.LogDriverJSONFile:
		// runwhp's own log, read back as json-file's is, under the name
		// Docker's clients look for: docker-py reads the output of a run
		// only from a json-file or journald log.
		lc.driver = container.LogDriverJSONFile
	case container.LogDriverNone:
		lc.driver = container.LogDriverNone
		if len(opts) > 0 {
			return lc, fmt.Errorf("--log-opt %s: the none log driver takes no options", opts[0])
		}
		return lc, nil
	case "journald", "syslog", "fluentd", "gelf", "awslogs", "splunk", "etwlogs", "gcplogs":
		return lc, fmt.Errorf("--log-driver %s: runwhp keeps its own log format; use local (the default) or none", driver)
	default:
		return lc, fmt.Errorf("--log-driver %s: unknown log driver; use local (the default) or none", driver)
	}
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			return lc, fmt.Errorf("--log-opt %s: give KEY=VALUE", o)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "max-size":
			if strings.TrimSpace(v) == "-1" {
				lc.maxSize = -1
				continue
			}
			n, err := parseSize(v)
			if err != nil || n < 4<<10 {
				return lc, fmt.Errorf("--log-opt %s: give a size of at least 4k, or -1 for no limit", o)
			}
			lc.maxSize = int64(n)
		case "max-file":
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 || n > 1000 {
				return lc, fmt.Errorf("--log-opt %s: give a number of files from 1 to 1000", o)
			}
			lc.maxFiles = n
		case "compress":
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err != nil || b {
				return lc, fmt.Errorf("--log-opt %s: runwhp does not compress logs", o)
			}
		default:
			return lc, fmt.Errorf("--log-opt %s: unknown option; runwhp knows max-size, max-file and compress=false", o)
		}
	}
	return lc, nil
}

// diagLogMax is how large runwhp.log grows before it is moved aside to
// runwhp.log.1, replacing the one before.
const diagLogMax = 1 << 20

// diagLog is runwhp.log, which a runwhp running a container in the
// background writes its own messages to, kept to two files of diagLogMax.
type diagLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openDiagLog(path string) (*diagLog, error) {
	d := &diagLog{path: path}
	if err := d.open(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *diagLog) open() error {
	f, err := os.OpenFile(d.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	d.f, d.size = f, fi.Size()
	return nil
}

func (d *diagLog) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil && d.size > 0 && d.size+int64(len(p)) > diagLogMax {
		d.f.Close()
		d.f = nil
		os.Remove(d.path + ".1")
		os.Rename(d.path, d.path+".1")
		d.open()
	}
	if d.f == nil {
		return len(p), nil
	}
	n, err := d.f.Write(p)
	d.size += int64(n)
	return n, err
}

func (d *diagLog) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// teeWriter writes to w, then to log, which never fails: what it returns
// is w's.
type teeWriter struct {
	w, log io.Writer
}

func (t teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.log.Write(p[:n])
	return n, err
}

// parseLogTime reads a time for logs --since and --until as Docker does:
// an RFC 3339 date and time, with or without a zone (without, it is local
// time), or a date alone; Unix seconds, with a fraction if need be; or a
// duration such as 10m or 1h30m, that long before now.
func parseLogTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("invalid time %q: a duration before now cannot be negative", s)
		}
		return now.Add(-d), nil
	}
	if sec, frac, ok := strings.Cut(s, "."); isDigits(sec) && (!ok || isDigits(frac)) {
		n, err := strconv.ParseInt(sec, 10, 64)
		if err == nil {
			var ns int64
			if ok {
				frac = (frac + "000000000")[:9]
				ns, _ = strconv.ParseInt(frac, 10, 64)
			}
			return time.Unix(n, ns), nil
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q: give a date and time such as 2026-11-24T12:00:00Z, Unix seconds, or a duration such as 10m", s)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseTail reads logs --tail: a number of lines, or all.
func parseTail(s string) (int, error) {
	if strings.EqualFold(s, "all") || s == "" {
		return -1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --tail %q: give a number of lines, or all", s)
	}
	return n, nil
}
