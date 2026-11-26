//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/passwd"
)

// userHZ is the unit of /proc's times, which Linux fixes at 100 on x86
// whatever the kernel's own tick.
const userHZ = 100

// sendTop answers a request for the container's processes.
func (c *controller) sendTop(seq uint64) {
	top := &guestspec.Processes{Seq: seq, Now: time.Now()}
	mem := c.mem.Load()
	if mem == nil {
		top.Error = "init cannot read /proc"
		c.send(guestspec.Event{Top: top})
		return
	}
	if data, err := read(mem.meminfo); err == nil {
		top.MemTotal = parseMeminfo(data)["MemTotal"]
	}
	var err error
	if top.Procs, err = mem.processes(); err != nil {
		top.Error = err.Error()
	}
	c.send(guestspec.Event{Top: top})
}

// processes lists every process in the guest but init and the kernel's
// own, which are the container's, from the /proc init opened for itself,
// with users named by the container's /etc/passwd.
func (m *memFiles) processes() ([]guestspec.Process, error) {
	if m.proc == nil {
		return nil, errors.New("init cannot read /proc")
	}
	boot, err := bootTime(m.stat)
	if err != nil {
		return nil, err
	}
	m.procMu.Lock()
	defer m.procMu.Unlock()
	if _, err := m.proc.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	names, err := m.proc.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	pwData, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	users := passwd.Names(pwData)
	dir := int(m.proc.Fd())
	var ps []guestspec.Process
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 2 {
			continue
		}
		stat, err := readAt(dir, name+"/stat")
		if err != nil {
			continue // gone already
		}
		p, ok := parseProcStat(stat, boot)
		if !ok || p.PPID == 2 {
			continue // the kernel's
		}
		p.PID = pid
		if status, err := readAt(dir, name+"/status"); err == nil {
			p.UID = statusUID(status)
		}
		p.User = users[p.UID]
		if cmdline, err := readAt(dir, name+"/cmdline"); err == nil {
			p.Args = splitCmdline(cmdline)
		}
		ps = append(ps, p)
	}
	return ps, nil
}

// readAt reads a file below the directory dir.
func readAt(dir int, name string) ([]byte, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(f)
}

// bootTime is when the guest booted, by /proc/stat's btime.
func bootTime(stat *os.File) (time.Time, error) {
	data, err := read(stat)
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				break
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

// parseProcStat reads a process from /proc/PID/stat, whose second field,
// the command name in parentheses, may hold spaces and parentheses itself.
func parseProcStat(data []byte, boot time.Time) (guestspec.Process, bool) {
	var p guestspec.Process
	open, end := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')')
	if open < 0 || end < open {
		return p, false
	}
	p.Comm = string(data[open+1 : end])
	// state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt
	// cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime vsize rss
	f := bytes.Fields(data[end+1:])
	if len(f) < 22 {
		return p, false
	}
	num := func(i int) int64 {
		n, _ := strconv.ParseInt(string(f[i]), 10, 64)
		return n
	}
	p.State = string(f[0])
	p.PPID, p.PGID, p.SID = int(num(1)), int(num(2)), int(num(3))
	p.TTY, p.TPGID = uint32(num(4)), int(num(5))
	p.CPU = time.Duration(num(11)+num(12)) * time.Second / userHZ
	p.Nice, p.Threads = int(num(16)), int(num(17))
	p.Start = boot.Add(time.Duration(num(19)) * time.Second / userHZ)
	p.VSZ = uint64(num(20))
	p.RSS = uint64(num(21)) * uint64(os.Getpagesize())
	return p, true
}

// statusUID is the real user ID on /proc/PID/status's Uid line.
func statusUID(data []byte) uint32 {
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 32)
				return uint32(n)
			}
		}
	}
	return 0
}

// splitCmdline splits /proc/PID/cmdline's NUL-separated arguments.
func splitCmdline(data []byte) []string {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil
	}
	return strings.Split(string(data), "\x00")
}
