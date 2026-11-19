// Package guestspec is the contract between the runwhp host process and the
// init program inside the guest.
//
// The host writes a Spec into the initramfs at SpecPath. The guest attaches
// one read-only virtio block device per image layer, bottom layer first, each
// with the serial LayerSerial(i), and a multiport virtio console with the
// ports named below. When the container process has exited, init writes one
// Status as a JSON line to the control port and resets the machine.
package guestspec

import "fmt"

// SpecPath is where the Spec sits in the initramfs, without a leading slash.
const SpecPath = "runwhp/spec.json"

// Console port names.
const (
	PortStdout  = "stdout"
	PortStderr  = "stderr"
	PortControl = "control"
)

// LayerSerial is the virtio-blk serial of layer i (0 is the bottom layer).
func LayerSerial(i int) string { return fmt.Sprintf("layer%d", i) }

// Spec describes the container process.
type Spec struct {
	Args     []string `json:"args"`
	Env      []string `json:"env,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	User     string   `json:"user,omitempty"` // name, uid, name:group or uid:gid; empty means root
	Hostname string   `json:"hostname,omitempty"`
	Layers   int      `json:"layers"`
	// Debug makes init describe what it does on the kernel console.
	Debug bool `json:"debug,omitempty"`
}

// Status is how the container ended.
type Status struct {
	Exit  int    `json:"exit"`
	Error string `json:"error,omitempty"` // set when the process could not be started
}

// Exit codes, as Docker uses them.
const (
	ExitRuntime      = 125 // runwhp itself failed
	ExitCannotInvoke = 126 // the command exists but cannot be executed
	ExitNotFound     = 127 // the command does not exist
	ExitSignalBase   = 128 // plus the signal number that killed the process
)
