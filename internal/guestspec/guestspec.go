// Package guestspec is the contract between the runwhp host process and the
// init program inside the guest.
//
// The host writes a Spec into the initramfs at SpecPath. The guest gets a
// multiport virtio console with the ports named below; then, when
// Spec.Network is set, a virtio network device with Network.MAC; then one
// read-only virtio block device per image layer, bottom layer first, each
// with the serial LayerSerial(i).
//
// The control port carries JSON lines both ways. The host sends Control
// messages: signals for the container process and terminal sizes. When the
// container process has exited, init writes one Status and resets the
// machine.
package guestspec

import "fmt"

// SpecPath is where the Spec sits in the initramfs, without a leading slash.
const SpecPath = "runwhp/spec.json"

// Console port names. All four ports are always there; init opens stdin
// only when Spec.Stdin is set, and the host feeds it only once it is open.
const (
	PortStdout  = "stdout"
	PortStderr  = "stderr"
	PortControl = "control"
	PortStdin   = "stdin"
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
	// Hosts are extra /etc/hosts lines, "ADDRESS NAME".
	Hosts  []string `json:"hosts,omitempty"`
	Layers int      `json:"layers"`
	// Network configures the network interface. nil means the guest has
	// none beyond loopback.
	Network *Network `json:"network,omitempty"`
	// Stdin connects the process's standard input to the stdin port, which
	// reaches end of file when the host's input does. Without it the process
	// reads /dev/null.
	Stdin bool `json:"stdin,omitempty"`
	// TTY puts the process on a pseudo-terminal of Rows by Cols, with all of
	// its output on the stdout port. Zero sizes leave the kernel's default.
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Debug makes init describe what it does on the kernel console.
	Debug bool `json:"debug,omitempty"`
}

// Network is the static configuration of the guest's one network interface.
type Network struct {
	MAC     string   `json:"mac"`     // identifies the interface, as in 02:00:00:00:00:0f
	Address string   `json:"address"` // the guest's address and prefix, as in 10.0.2.15/24
	Gateway string   `json:"gateway"` // the default route
	DNS     []string `json:"dns,omitempty"`
	MTU     int      `json:"mtu,omitempty"`
}

// Control is a message from the host to init.
type Control struct {
	// Signal, when not zero, is sent to the container process, or with
	// Group to its whole process group, as a terminal's Ctrl+C would be.
	Signal int  `json:"signal,omitempty"`
	Group  bool `json:"group,omitempty"`
	// Rows and Cols, when both are set, are the terminal's new size.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
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
