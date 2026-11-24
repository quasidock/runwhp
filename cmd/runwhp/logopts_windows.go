package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

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
	case container.LogDriverNone:
		lc.driver = container.LogDriverNone
		if len(opts) > 0 {
			return lc, fmt.Errorf("--log-opt %s: the none log driver takes no options", opts[0])
		}
		return lc, nil
	case "json-file", "journald", "syslog", "fluentd", "gelf", "awslogs", "splunk", "etwlogs", "gcplogs":
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
