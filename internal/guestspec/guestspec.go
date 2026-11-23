// Package guestspec is the contract between the runwhp host process and the
// init program inside the guest.
//
// The host writes a Spec into the initramfs at SpecPath. The guest gets a
// multiport virtio console with the ports named below; then a virtio socket
// device, which gives the guest the context ID GuestCID; then a virtio
// memory balloon, which only takes free page reporting; then, when
// Spec.Network is set, a virtio network device with Network.MAC; then, when
// any Mount binds a share, a virtio-fs device tagged FSTag, and when any
// binds a delegated share, one tagged FSTagDelegated; then one
// read-only virtio block device per image layer, bottom layer first, each
// with the serial LayerSerial(i); then, when Spec.Scratch is set, the
// writable scratch disk, with the serial ScratchSerial; then one writable
// disk per named volume, with the serial VolumeSerial(i). Init finds disks
// by serial, not by the order they appear in.
//
// The scratch disk holds the container's writable layer: an ext4 file
// system on which init puts the overlay's upper and work directories.
// Without it, they go on a tmpfs. With Spec.KeepLayer, the disk is kept
// after the run and booted again: it has a journal, and init freezes its
// file system before resetting the machine, as it does each volume's.
// Otherwise it was made for this run and is thrown away after it.
//
// A virtio-fs device's root holds its shares as entries named "0", "1", and
// so on. Init mounts it, sets OwnerXattr on its root, and binds each share
// onto its Mount.Target. The FSTagDelegated device's FUSE connection takes
// the kernel's writeback cache, so writes reach the host when files are
// closed or synced, or the kernel writes back, rather than as they are made.
//
// A volume's disk holds a journaled ext4 file system, whose directory _data
// is what the container sees. Init fills an empty _data from the image
// before mounting anything, as Docker does, and freezes each volume's file
// system before resetting the machine, which leaves it clean.
//
// Init makes /etc/hostname, /etc/hosts and, with a network,
// /etc/resolv.conf, and binds them over the image's after the other mounts,
// unless one of those mounts is at that path or binds host files above it.
//
// The control port carries JSON lines both ways. The host sends Control
// messages: signals for the container process, terminal sizes, requests
// for statistics and, with Spec.WaitStart, when to start the process. Init
// sends Events: when the container is ready to start, when the process has
// started, the statistics asked for, and when the process has exited,
// after which init resets the machine.
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

// ScratchSerial is the virtio-blk serial of the scratch disk.
const ScratchSerial = "scratch"

// VolumeSerial is the virtio-blk serial of volume disk i.
func VolumeSerial(i int) string { return fmt.Sprintf("vol%d", i) }

// VolumeData is the directory of a volume's file system that is mounted
// into the container.
const VolumeData = "_data"

// GuestCID is the guest's vsock context ID.
const GuestCID = 3

// FSTag is the mount tag of the virtio-fs device that carries the shares,
// and FSTagDelegated that of the one carrying delegated shares, whose writes
// the guest caches.
const (
	FSTag          = "runwhp"
	FSTagDelegated = "runwhp-delegated"
)

// OwnerXattr is the extended attribute init sets on the root of the shares,
// to "UID:GID" of the container's user, before anything else reads them.
// Host files without an owner of their own then belong to that user.
const OwnerXattr = "trusted.runwhp.owner"

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
	// Scratch says the writable layer goes on the scratch disk.
	Scratch bool `json:"scratch,omitempty"`
	// KeepLayer says the scratch disk outlives the run: what is written to
	// it must reach the host before the guest resets.
	KeepLayer bool `json:"keepLayer,omitempty"`
	// WaitStart has init prepare the container, send an Event with Ready,
	// and wait for a Control with Start before it starts the process. A
	// signal that arrives first ends the container without starting it, as
	// if the process had been killed by it.
	WaitStart bool `json:"waitStart,omitempty"`
	// Volumes is how many volume disks there are.
	Volumes int `json:"volumes,omitempty"`
	// Network configures the network interface. nil means the guest has
	// none beyond loopback.
	Network *Network `json:"network,omitempty"`
	// Mounts are the bind mounts, volumes and tmpfs mounts, in the order to
	// mount them: parents before what is mounted inside them.
	Mounts []Mount `json:"mounts,omitempty"`
	// Stdin connects the process's standard input to the stdin port, which
	// reaches end of file when the host's input does. Without it the process
	// reads /dev/null.
	Stdin bool `json:"stdin,omitempty"`
	// TTY puts the process on a pseudo-terminal of Rows by Cols, with all of
	// its output on the stdout port. Zero sizes leave the kernel's default.
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Reclaim says when init gives the guest's page cache back.
	Reclaim Reclaim `json:"reclaim,omitzero"`
	// Debug makes init describe what it does on the kernel console.
	Debug bool `json:"debug,omitempty"`
}

// Reclaim has init reclaim the guest's page cache once the guest has kept
// its processors idle for IdleSeconds, a step every IntervalSeconds, so
// that the memory goes back to the host through free page reporting. Zero
// IdleSeconds turns it off.
type Reclaim struct {
	IdleSeconds     int `json:"idleSeconds,omitempty"`
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// Network is the static configuration of the guest's one network interface.
type Network struct {
	MAC     string   `json:"mac"`     // identifies the interface, as in 02:00:00:00:00:0f
	Address string   `json:"address"` // the guest's address and prefix, as in 10.0.2.15/24
	Gateway string   `json:"gateway"` // the default route
	DNS     []string `json:"dns,omitempty"`
	MTU     int      `json:"mtu,omitempty"`
}

// Mount kinds.
const (
	MountBind   = ""       // share Share of the virtio-fs device
	MountVolume = "volume" // the _data directory of volume disk Volume
	MountAnon   = "anon"   // an anonymous volume: a directory of this run's own
	MountTmpfs  = "tmpfs"  // a new tmpfs, mounted with Options
)

// Mount is one mount in the container, at Target.
type Mount struct {
	Kind     string `json:"kind,omitempty"`
	Target   string `json:"target"`
	Share    int    `json:"share"`
	Volume   int    `json:"volume,omitempty"`
	Name     string `json:"name,omitempty"` // the volume's name, for messages
	File     bool   `json:"file,omitempty"` // the share is a file, not a directory
	ReadOnly bool   `json:"readonly,omitempty"`
	// Delegated says the share is on the virtio-fs device tagged
	// FSTagDelegated, whose FUSE connection caches writes, instead of the
	// one tagged FSTag. Each device numbers its shares from 0.
	Delegated bool `json:"delegated,omitempty"`
	// NoCopy leaves a new volume empty, instead of filling it with what the
	// image has at Target.
	NoCopy bool `json:"nocopy,omitempty"`
	// Options are a tmpfs's mount options: noexec, nosuid, nodev, ro, and
	// size, mode, uid, gid and nr_inodes as tmpfs takes them.
	Options string `json:"options,omitempty"`
}

// Control is a message from the host to init.
type Control struct {
	// Signal, when not zero, is sent to the container process, or with
	// Group to its whole process group, as a terminal's Ctrl+C would be, or
	// with All to every process in the container.
	Signal int  `json:"signal,omitempty"`
	Group  bool `json:"group,omitempty"`
	All    bool `json:"all,omitempty"`
	// Start starts the process of a container booted with Spec.WaitStart.
	Start bool `json:"start,omitempty"`
	// Rows and Cols, when both are set, are the terminal's new size.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Stats, when not zero, asks init for the guest's statistics, which
	// it sends back in an Event with GuestStats.Seq set to Stats.
	Stats uint64 `json:"stats,omitempty"`
	// Time, when not zero, is the host's wall clock as the message left
	// it, in nanoseconds since the Unix epoch. Init brings the guest's
	// clock to it: gradually when it is close, at once when not.
	Time int64 `json:"time,omitempty"`
}

// Event is a message from init to the host.
type Event struct {
	// Ready: with Spec.WaitStart, the container is prepared, and init waits
	// for Control.Start.
	Ready bool `json:"ready,omitempty"`
	// Started: the container process runs, as Pid.
	Started bool `json:"started,omitempty"`
	Pid     int  `json:"pid,omitempty"`
	// Exited: the process has ended with Exit, or could not be started, as
	// Error says. Init sends it once, last, just before it resets the
	// machine.
	Exited bool   `json:"exited,omitempty"`
	Exit   int    `json:"exit,omitempty"`
	Error  string `json:"error,omitempty"`
	// Stats answers Control.Stats.
	Stats *GuestStats `json:"stats,omitempty"`
}

// GuestStats is what init reports of the guest when the host asks.
type GuestStats struct {
	Seq uint64 `json:"seq"`
	// MemTotal, MemAvailable and Cached are /proc/meminfo's, in bytes.
	MemTotal     uint64 `json:"memTotal"`
	MemAvailable uint64 `json:"memAvailable"`
	Cached       uint64 `json:"cached"`
	// Tasks counts the threads of every process in the guest but init and
	// the kernel's own, as a container's pids.current would.
	Tasks int `json:"tasks"`
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

// ExecPort is the vsock port on which init serves runwhp exec, once the
// container's process has started.
//
// runwhp exec opens a control connection and sends an ExecHello with Exec.
// Init answers with an ExecEvent naming the session and the streams it
// wants, or saying why it cannot run the command. The host opens one more
// connection per stream, each starting with an ExecHello that attaches it,
// followed by the stream's bytes: stdin from the host, stdout and stderr
// to it, or with a terminal stdout alone. Once every stream is attached,
// init starts the process and sends an ExecEvent with its pid. On the
// control connection the host then sends Control messages, signals and
// terminal sizes, and init sends a last ExecEvent when the process has
// exited and its output has been sent. A process started with Detach gets
// /dev/null and no streams, and its session ends once it has started.
//
// When the container's process exits, init kills every process left,
// those of runwhp exec included.
const ExecPort = 1024

// Exec describes a process for runwhp exec.
type Exec struct {
	Args []string `json:"args"`
	// Env is added to the container's environment.
	Env []string `json:"env,omitempty"`
	// Cwd defaults to the container's working directory, and must exist.
	Cwd string `json:"cwd,omitempty"`
	// User defaults to the container's user.
	User   string `json:"user,omitempty"`
	Stdin  bool   `json:"stdin,omitempty"`
	TTY    bool   `json:"tty,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
	Cols   uint16 `json:"cols,omitempty"`
	Detach bool   `json:"detach,omitempty"`
}

// ExecHello is the first line on each runwhp exec connection.
type ExecHello struct {
	Exec *Exec `json:"exec,omitempty"`
	// Attach and Stream attach a connection to a session's stream.
	Attach string `json:"attach,omitempty"`
	Stream string `json:"stream,omitempty"`
}

// ExecEvent is a message from init on a runwhp exec control connection.
type ExecEvent struct {
	ID      string   `json:"id,omitempty"`
	Streams []string `json:"streams,omitempty"`
	Started bool     `json:"started,omitempty"`
	Pid     int      `json:"pid,omitempty"`
	Exited  bool     `json:"exited,omitempty"`
	Exit    int      `json:"exit,omitempty"`
	Error   string   `json:"error,omitempty"`
}
