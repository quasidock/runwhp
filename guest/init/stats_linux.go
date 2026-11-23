//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// tasks counts the threads of every process in the guest but init and the
// kernel's own: kthreadd, pid 2, and its children. It reads the /proc init
// opened for itself, so a container that mounts over /proc cannot hide
// its processes.
func (m *memFiles) tasks() int {
	if m.proc == nil {
		return 0
	}
	m.procMu.Lock()
	defer m.procMu.Unlock()
	if _, err := m.proc.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	names, err := m.proc.Readdirnames(-1)
	if err != nil {
		return 0
	}
	dir := int(m.proc.Fd())
	n := 0
	for _, name := range names {
		if pid, err := strconv.Atoi(name); err != nil || pid <= 2 {
			continue
		}
		fd, err := unix.Openat(dir, name+"/stat", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // gone already
		}
		f := os.NewFile(uintptr(fd), name)
		data, err := read(f)
		f.Close()
		if err != nil {
			continue
		}
		if ppid, threads, ok := parseStat(data); ok && ppid != 2 {
			n += threads
		}
	}
	return n
}

// parseStat reads a process's parent and thread count from
// /proc/PID/stat, whose second field, the command name in parentheses, may
// hold spaces and parentheses itself.
func parseStat(data []byte) (ppid, threads int, ok bool) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, 0, false
	}
	// state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt
	// cmajflt utime stime cutime cstime priority nice num_threads
	f := bytes.Fields(data[i+1:])
	if len(f) < 18 {
		return 0, 0, false
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	threads, err2 := strconv.Atoi(string(f[17]))
	return ppid, threads, err1 == nil && err2 == nil
}
