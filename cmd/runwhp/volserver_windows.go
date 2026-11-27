//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/quasidock/runwhp/internal/vmm"
	"github.com/quasidock/runwhp/internal/volume"
)

// cmdVolumeServer serves a shared volume to the containers using it, as
// the first of them starts it: runwhp __volume --ready HANDLE NAME. It
// writes a volume.ServerReply line on the ready handle once it serves, or
// cannot, and its diagnostics to server.log in the volume's directory.
func cmdVolumeServer(args []string) int {
	fs := flag.NewFlagSet("__volume", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ready uint64
	var kernel, initPath string
	var modules listFlag
	var debug bool
	fs.Uint64Var(&ready, "ready", 0, "")
	fs.StringVar(&kernel, "kernel", "", "")
	fs.Var(&modules, "module", "")
	fs.StringVar(&initPath, "init", "", "")
	fs.BoolVar(&debug, "debug", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: runwhp __volume [--ready HANDLE] [--kernel PATH] [--module PATH]... [--init PATH] NAME")
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	var readyFile io.Writer
	if ready != 0 {
		f := os.NewFile(uintptr(ready), "ready")
		defer f.Close()
		readyFile = f
	}
	s := &vmm.VolumeServer{
		Root: root, Name: fs.Arg(0),
		Kernel: kernel, Modules: modules, Init: initPath, Debug: debug,
		Log: io.Discard,
	}
	if v, err := volume.Open(root).Get(s.Name); err == nil {
		if log, err := os.OpenFile(filepath.Join(v.Dir(), "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer log.Close()
			s.Log = log
		}
	}
	return s.Run(readyFile)
}
