//go:build windows

// runwhp runs Linux containers on the Windows Hypervisor Platform, one small
// virtual machine per container, from an ordinary unelevated process.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usage = `Usage:
  runwhp run [flags] IMAGE [COMMAND [ARG...]]   run a command in a new container
  runwhp pull IMAGE                             pull an image into the local store

Run 'runwhp run -h' for the run flags.

The local store is in %LOCALAPPDATA%\runwhp unless RUNWHP_ROOT is set.
`

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "pull":
		return cmdPull(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// storeRoot is where images, layers and kernel modules are kept.
func storeRoot() (string, error) {
	if root := os.Getenv("RUNWHP_ROOT"); root != "" {
		return root, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "runwhp"), nil
}

func errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "runwhp: "+format+"\n", a...)
}
