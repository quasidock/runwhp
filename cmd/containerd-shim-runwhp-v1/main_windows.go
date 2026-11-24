//go:build windows

// containerd-shim-runwhp-v1 is runwhp's containerd shim. containerd runs it
// for each task of the runtime io.containerd.runwhp.v1, as
// containerd-shim-runwhp-v1.exe on its PATH, and the shim runs the task's
// container in a VM of its own, as runwhp runs one, in its own process.
// Run as `containerd-shim-runwhp-v1 snapshotter`, it serves containerd
// runwhp's snapshotter instead, which runwhp containerd starts it for.
//
// containerd talks to a shim as its shim v2 protocol says:
//
//   - `start`, with the bundle as the working directory and protobuf
//     BootstrapParams on stdin, starts the shim proper in the background
//     and prints a protobuf BootstrapResult with its address, a named pipe
//     it serves the task API (containerd.task.v3) on over ttrpc. containerd
//     reads start's stdout and stderr together until both close, so the
//     shim proper inherits neither.
//   - The shim proper serves the task API, publishes the task's events to
//     containerd's ttrpc address, and serves runwhp's messages on the log
//     pipe containerd reads, containerd-shim-NAMESPACE-ID-log.
//   - `delete`, for a shim that died, cleans up after it and prints a
//     protobuf DeleteResponse.
//   - `-info` prints a protobuf RuntimeInfo.
package main

import (
	"flag"
	"fmt"
	"os"
)

// runtimeName is the containerd runtime the shim is.
const runtimeName = "io.containerd.runwhp.v1"

// options are the flags containerd runs a shim with.
type options struct {
	namespace     string
	id            string
	address       string // containerd's gRPC address
	publishBinary string
	bundle        string // delete's
	socket        string // the shim proper's address, from start
	debug         bool
}

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	if len(args) > 0 && args[0] == "snapshotter" {
		return cmdSnapshotter(args[1:])
	}
	var o options
	fs := flag.NewFlagSet("containerd-shim-runwhp-v1", flag.ContinueOnError)
	fs.StringVar(&o.namespace, "namespace", "", "namespace that owns the shim")
	fs.StringVar(&o.id, "id", "", "id of the task")
	fs.StringVar(&o.address, "address", "", "grpc address back to main containerd")
	fs.StringVar(&o.publishBinary, "publish-binary", "", "path to publish binary (used for publishing events)")
	fs.StringVar(&o.bundle, "bundle", "", "path to the bundle if not workdir")
	fs.StringVar(&o.socket, "socket", "", "socket path to serve")
	fs.BoolVar(&o.debug, "debug", false, "enable debug output in logs")
	info := fs.Bool("info", false, "get the option protobuf from stdin, print the shim info protobuf to stdout, and exit")
	version := fs.Bool("v", false, "show the shim version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case *version:
		fmt.Printf("containerd-shim-runwhp-v1\n  runtime: %s\n", runtimeName)
		return 0
	case *info:
		return cmdInfo()
	}
	switch fs.Arg(0) {
	case "start":
		return cmdStart(o)
	case "delete":
		return cmdDelete(o)
	case "":
		return cmdServe(o)
	default:
		fmt.Fprintf(os.Stderr, "containerd-shim-runwhp-v1: unknown action %q\n", fs.Arg(0))
		return 2
	}
}
