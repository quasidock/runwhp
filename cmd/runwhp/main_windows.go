//go:build windows

// runwhp runs Linux containers on the Windows Hypervisor Platform, one small
// virtual machine per container, from an ordinary unelevated process.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const usage = `Usage:
  runwhp run [flags] IMAGE [COMMAND [ARG...]]   run a command in a new container
  runwhp create [flags] IMAGE [COMMAND...]      create a container, ready to start
  runwhp start [-a [-i]] CONTAINER...           start created or stopped containers
  runwhp attach CONTAINER                       attach this console to a running container
  runwhp wait CONTAINER...                      wait for containers to stop, and print their exit codes
  runwhp exec [flags] CONTAINER COMMAND...      run a command in a running container
  runwhp kill [-a] CONTAINER [SIGNAL]           send a signal to a container
  runwhp stop [-t SECONDS] CONTAINER...         stop containers
  runwhp restart [-t SECONDS] CONTAINER...      stop containers and start them again
  runwhp logs [-f] CONTAINER                    print the output of a container in the background
  runwhp list [-q] [--format table|json]        list containers
  runwhp state CONTAINER                        describe a container's state as JSON
  runwhp stats [--no-stream] [CONTAINER...]     show what running containers use
  runwhp delete [-f] CONTAINER...               remove containers
  runwhp rename CONTAINER NAME                  rename a container
  runwhp container prune [-f]                   remove stopped and created containers
  runwhp pull IMAGE                             pull an image, and runwhp's kernel, into the local store
  runwhp images [-q] [REPOSITORY[:TAG]]         list images
  runwhp rmi [-f] IMAGE...                      remove images
  runwhp image COMMAND                          list, inspect, remove and prune images
  runwhp volume COMMAND                         create, list, inspect and remove volumes
  runwhp system df [-v]                         show the space the store takes
  runwhp system prune [-a] [--volumes] [-f]     remove what nothing uses

Run 'runwhp run -h' for the run flags, and 'runwhp image' or 'runwhp
volume' for their commands. A container is named by its ID, a unique
prefix of it, or its name. It is kept after it stops, with its writable
layer, until deleted, unless it was run with --rm.

The local store is in %LOCALAPPDATA%\runwhp unless RUNWHP_ROOT is set. The
first run or pull fetches runwhp's kernel into it.
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
	case "volume":
		return cmdVolume(args[1:], os.Stdin, os.Stdout)
	case "image":
		return cmdImage(args[1:], os.Stdin, os.Stdout)
	case "container":
		return cmdContainer(args[1:], os.Stdin, os.Stdout)
	case "system":
		return cmdSystem(args[1:], os.Stdin, os.Stdout)
	case "images":
		return cmdImages(args[1:], os.Stdout)
	case "rmi":
		return cmdRmi(args[1:], os.Stdout)
	case "list":
		return cmdList(args[1:], os.Stdout)
	case "state":
		return cmdState(args[1:], os.Stdout)
	case "stats":
		return cmdStats(args[1:], os.Stdout)
	case "delete":
		return cmdDelete(args[1:], os.Stdout)
	case "rename":
		return cmdRename(args[1:])
	case "create":
		return cmdCreate(args[1:])
	case "start":
		return cmdStart(args[1:], os.Stdout)
	case "attach":
		return cmdAttach(args[1:])
	case "wait":
		return cmdWait(args[1:], os.Stdout)
	case "kill":
		return cmdKill(args[1:])
	case "stop":
		return cmdStop(args[1:], os.Stdout)
	case "restart":
		return cmdRestart(args[1:], os.Stdout)
	case "logs":
		return cmdLogs(args[1:], os.Stdout, os.Stderr)
	case "exec":
		return cmdExec(args[1:])
	case "__supervise":
		return cmdSupervise(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// storeRoot is where images, layers and kernels are kept.
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

// diagOut takes runwhp's own messages: stderr, except in a runwhp running
// a container in the background, whose stderr may be the container's.
var diagOut io.Writer = os.Stderr

func errorf(format string, a ...any) {
	fmt.Fprintf(diagOut, "runwhp: "+format+"\n", a...)
}
