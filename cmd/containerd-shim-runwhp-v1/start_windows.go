//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	bootstrap "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	apitypes "github.com/containerd/containerd/api/types"
	"golang.org/x/sys/windows"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/quasidock/runwhp/internal/container"
)

const (
	detachedProcess  = 0x00000008 // DETACHED_PROCESS
	breakawayFromJob = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB

	// serveWait is how long start waits for the shim proper to listen.
	serveWait = 10 * time.Second
)

// shimAddress is the named pipe the shim of task id in namespace ns
// serves the task API on.
func shimAddress(ns, id string) string {
	return `\\.\pipe\containerd-shim-runwhp-` + ns + "-" + id
}

// logAddress is the named pipe containerd reads the shim's messages from.
func logAddress(ns, id string) string {
	return `\\.\pipe\containerd-shim-` + ns + "-" + id + "-log"
}

// cmdStart starts the shim proper in the background, detached from
// containerd's console and, where the job allows, from its job, so that
// the task outlives containerd; waits until it listens; and prints where.
func cmdStart(o options) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "containerd-shim-runwhp-v1: %v\n", err)
		return 1
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(err)
	}
	var params bootstrap.BootstrapParams
	if len(data) > 0 {
		if err := proto.Unmarshal(data, &params); err != nil {
			return fail(fmt.Errorf("reading containerd's bootstrap parameters: %w", err))
		}
	}
	if o.namespace == "" {
		o.namespace = params.Namespace
	}
	if o.id == "" {
		o.id = params.InstanceID
	}
	if o.namespace == "" || o.id == "" {
		return fail(errors.New("start needs -namespace and -id"))
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	bundle, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	addr := shimAddress(o.namespace, o.id)
	args := []string{"-namespace", o.namespace, "-id", o.id, "-address", o.address, "-socket", addr}
	if o.debug || params.LogLevel < bootstrap.LogLevel_LOG_LEVEL_INFO {
		args = append(args, "-debug")
	}
	cmd := exec.Command(exe, append(args, "-bundle", bundle)...)
	// Not the bundle, which containerd renames and removes as the task
	// goes, while this process may still be ending: Windows keeps a
	// process's working directory from being renamed.
	root, err := storeRoot()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fail(err)
	}
	cmd.Dir = root
	cmd.Env = os.Environ()
	if os.Getenv("TTRPC_ADDRESS") == "" && params.ContainerdTtrpcAddress != "" {
		cmd.Env = append(cmd.Env, "TTRPC_ADDRESS="+params.ContainerdTtrpcAddress)
	}
	// No stdin, stdout or stderr: containerd waits for this process's
	// output to close.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | windows.CREATE_NEW_PROCESS_GROUP | breakawayFromJob}
	err = cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// containerd's job does not let its children leave it; the task
		// then ends with containerd.
		cmd.SysProcAttr.CreationFlags &^= breakawayFromJob
		err = cmd.Start()
	}
	if err != nil {
		return fail(fmt.Errorf("starting the shim: %w", err))
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	timeout := 200 * time.Millisecond
	for deadline := time.Now().Add(serveWait); ; {
		c, err := winio.DialPipe(addr, &timeout)
		if err == nil {
			c.Close()
			break
		}
		select {
		case err := <-exited:
			return fail(fmt.Errorf("the shim exited before it served the task: %v; containerd logs its messages with --log-level debug", err))
		default:
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			return fail(fmt.Errorf("the shim did not serve the task within %v", serveWait))
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := proto.Marshal(&bootstrap.BootstrapResult{Version: 3, Address: addr, Protocol: "ttrpc"})
	if err != nil {
		return fail(err)
	}
	os.Stdout.Write(out)
	return 0
}

// bundleRecord is what the shim keeps in the task's bundle for delete: the
// store, and the runwhp container it made there.
type bundleRecord struct {
	Root      string `json:"root"`
	Container string `json:"container"`
}

const bundleRecordName = "runwhp.json"

// cmdDelete cleans up after a shim that died: the VM went with it, so it
// removes the container's record, and the bundle's empty root file
// system directory, which containerd would otherwise ask Windows to
// unmount, and could not, unelevated.
func cmdDelete(o options) int {
	bundle := o.bundle
	if bundle == "" {
		bundle, _ = os.Getwd()
	}
	exit := uint32(255)
	if data, err := os.ReadFile(filepath.Join(bundle, bundleRecordName)); err == nil {
		var r bundleRecord
		if json.Unmarshal(data, &r) == nil && r.Root != "" && r.Container != "" {
			store := container.Open(r.Root)
			if c, err := store.Get(r.Container); err == nil {
				if c.State.Status == container.Stopped {
					exit = uint32(c.State.ExitCode)
				}
				store.Remove(c)
			}
		}
	}
	os.Remove(filepath.Join(bundle, "rootfs"))
	out, err := proto.Marshal(&taskv3.DeleteResponse{ExitStatus: exit, ExitedAt: timestamppb.Now()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Stdout.Write(out)
	return 0
}

// cmdInfo prints the shim's RuntimeInfo, which containerd asks for to see
// what mounts the shim makes itself; it names none.
func cmdInfo() int {
	io.Copy(io.Discard, os.Stdin)
	out, err := proto.Marshal(&apitypes.RuntimeInfo{Name: runtimeName, Version: &apitypes.RuntimeVersion{Version: "1"}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Stdout.Write(out)
	return 0
}
