//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// cgroupDir is where init mounts the cgroup hierarchy for itself, in the
// initramfs, which the container's root later covers.
const cgroupDir = "/runwhp/cgroup"

// memFiles are the files init reads the guest's memory and processor time
// from, and the one it reclaims page cache through. They are opened before
// the container's root replaces init's, so nothing the container mounts
// over /proc or /sys can take them away; init reads them again from the
// start each time.
type memFiles struct {
	stat, meminfo *os.File
	proc          *os.File // the /proc directory, read under procMu
	procMu        sync.Mutex
	reclaim       *os.File // the root cgroup's memory.reclaim, or nil
}

// openMemFiles opens the files, logging what it cannot open: without
// them, init neither reclaims nor reports memory.
func openMemFiles() *memFiles {
	m := &memFiles{}
	var err error
	if m.stat, err = os.Open("/proc/stat"); err != nil {
		logf("opening /proc/stat: %v", err)
	}
	if m.meminfo, err = os.Open("/proc/meminfo"); err != nil {
		logf("opening /proc/meminfo: %v", err)
	}
	if m.proc, err = os.Open("/proc"); err != nil {
		logf("opening /proc: %v", err)
	}
	if err := os.MkdirAll(cgroupDir, 0o755); err != nil {
		logf("making %s: %v", cgroupDir, err)
		return m
	}
	// Mounting the hierarchy waits for an RCU grace period, 15 ms in a
	// guest this idle; expedited, it takes well under one.
	openRCUExpedited()
	if storage.rcuExpedited >= 0 {
		syscall.Write(storage.rcuExpedited, []byte("1"))
	}
	err = syscall.Mount("cgroup2", cgroupDir, "cgroup2", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
	if storage.rcuExpedited >= 0 {
		syscall.Write(storage.rcuExpedited, []byte("0"))
	}
	if err != nil {
		logf("mounting cgroup2: %v", err)
		return m
	}
	fd, err := syscall.Open(cgroupDir+"/memory.reclaim", syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		logf("opening memory.reclaim: %v", err)
		return m
	}
	m.reclaim = os.NewFile(uintptr(fd), "memory.reclaim")
	return m
}

// read reads a whole file from its start.
func read(f *os.File) ([]byte, error) {
	if f == nil {
		return nil, os.ErrNotExist
	}
	buf := make([]byte, 8192)
	for {
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n < len(buf) {
			return buf[:n], nil
		}
		buf = make([]byte, 2*len(buf))
	}
}

// parseMeminfo parses /proc/meminfo into bytes by field name.
func parseMeminfo(data []byte) map[string]uint64 {
	fields := map[string]uint64{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		name, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		f := bytes.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(string(f[0]), 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && string(f[1]) == "kB" {
			n <<= 10
		}
		fields[string(name)] = n
	}
	return fields
}

// cpuTimes parses /proc/stat's first line: the time all processors have
// spent busy and in total, in clock ticks, and how many processors there
// are.
func cpuTimes(data []byte) (busy, total uint64, cpus int) {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("cpu")) && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			cpus++
		}
	}
	if len(lines) == 0 || !bytes.HasPrefix(lines[0], []byte("cpu ")) {
		return 0, 0, cpus
	}
	// user nice system idle iowait irq softirq steal
	for i, f := range bytes.Fields(lines[0])[1:] {
		if i >= 8 {
			break
		}
		n, _ := strconv.ParseUint(string(f), 10, 64)
		total += n
		if i != 3 && i != 4 {
			busy += n
		}
	}
	return busy, total, cpus
}

// memSample is what the reclaimer looks at each interval.
type memSample struct {
	busy, total uint64 // clock ticks, all processors together
	cpus        int
	file        uint64 // bytes of page cache: Active(file) + Inactive(file)
	memTotal    uint64
}

func (m *memFiles) sample() (memSample, error) {
	stat, err := read(m.stat)
	if err != nil {
		return memSample{}, err
	}
	info, err := read(m.meminfo)
	if err != nil {
		return memSample{}, err
	}
	var s memSample
	s.busy, s.total, s.cpus = cpuTimes(stat)
	f := parseMeminfo(info)
	s.file = f["Active(file)"] + f["Inactive(file)"]
	s.memTotal = f["MemTotal"]
	return s, nil
}

// reclaimer decides how much page cache to give back. The guest is idle
// over an interval when it kept less than 5% of one processor busy; once
// it has been idle for long enough, it gives back a tenth of its page
// cache, and at least 32 MiB, each interval, down to a floor of 2% of its
// RAM, and at least 64 MiB. Becoming busy again, or failing to reclaim
// all it asked for, ends the round.
type reclaimer struct {
	idle      time.Duration // how long the guest must be idle first
	idleFor   time.Duration
	exhausted bool // the kernel could not reclaim what it was asked to
	prev      memSample
	primed    bool
}

const (
	reclaimMinStep  = 32 << 20
	reclaimMinFloor = 64 << 20
)

// next takes the sample at the end of an interval and returns how many
// bytes to reclaim now.
func (r *reclaimer) next(s memSample, interval time.Duration) uint64 {
	prev, primed := r.prev, r.primed
	r.prev, r.primed = s, true
	if !primed || s.total <= prev.total || s.cpus == 0 {
		return 0
	}
	busy, total := s.busy-prev.busy, s.total-prev.total
	// busy / (total / cpus) < 5%
	if busy*20*uint64(s.cpus) >= total {
		r.idleFor, r.exhausted = 0, false
		return 0
	}
	r.idleFor += interval
	if r.idleFor < r.idle || r.exhausted {
		return 0
	}
	floor := max(uint64(reclaimMinFloor), s.memTotal/50)
	if s.file <= floor {
		return 0
	}
	return min(max(uint64(reclaimMinStep), s.file/10), s.file-floor)
}

// reclaimLoop reclaims page cache while the guest is idle, until stop is
// closed.
func (m *memFiles) reclaimLoop(cfg guestspec.Reclaim, stop <-chan struct{}) {
	if m.reclaim == nil || cfg.IdleSeconds <= 0 {
		return
	}
	interval := time.Duration(max(cfg.IntervalSeconds, 1)) * time.Second
	r := &reclaimer{idle: time.Duration(cfg.IdleSeconds) * time.Second}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		s, err := m.sample()
		if err != nil {
			logf("reading memory statistics: %v; no longer reclaiming page cache", err)
			return
		}
		n := r.next(s, interval)
		if n == 0 {
			continue
		}
		start := time.Now()
		_, err = m.reclaim.WriteString(strconv.FormatUint(n, 10))
		switch {
		case errors.Is(err, syscall.EAGAIN):
			r.exhausted = true
			debugf("reclaimed less than %d MiB of %d MiB of page cache", n>>20, s.file>>20)
		case err != nil:
			logf("reclaiming page cache: %v; no longer reclaiming", err)
			return
		default:
			debugf("reclaimed %d MiB of %d MiB of page cache in %d ms", n>>20, s.file>>20, time.Since(start).Milliseconds())
		}
	}
}
