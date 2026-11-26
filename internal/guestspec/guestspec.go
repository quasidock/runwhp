// Package guestspec is the contract between the runwhp host process and the
// init program inside the guest.
//
// The host writes a Spec into the initramfs at SpecPath. The guest gets a
// multiport virtio console with the ports named below; then a virtio socket
// device, which gives the guest the context ID GuestCID; then a virtio
// memory balloon, which only takes free page reporting; then one virtio
// network device for each of Spec.Networks, with its MAC, in order; then,
// when any Mount binds a share, a virtio-fs device tagged FSTag, and when any
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
// Init makes /etc/hostname, /etc/hosts and, with Spec.Resolv,
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

import (
	"fmt"
	"time"
)

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
	// HideMountPoints puts the mount points of Mounts in the init layer
	// too, not only init's own, so they are no part of the writable
	// layer's changes: a build step's, as BuildKit leaves a RUN step's
	// mounts out of its layer.
	HideMountPoints bool `json:"hideMountPoints,omitempty"`
	// ClockSync says the host sends its time as init opens the control
	// port: init sets the clock from it before it starts the process, which
	// would otherwise see the CMOS stub's whole seconds.
	ClockSync bool `json:"clockSync,omitempty"`
	// Volumes is how many volume disks there are.
	Volumes int `json:"volumes,omitempty"`
	// Networks configures the network interfaces, one for each network
	// device, in the devices' order, which is the order the kernel names
	// them eth0, eth1 and so on. None means the guest has only loopback.
	Networks []Network `json:"networks,omitempty"`
	// Resolv is the container's /etc/resolv.conf. nil leaves the image's.
	Resolv *Resolv `json:"resolv,omitempty"`
	// HostnameAddrs, if set, are the container's addresses, which
	// /etc/hosts gives its hostname after Hosts, as Docker's does on a
	// user-defined network. Without them the hostname is 127.0.0.1's.
	HostnameAddrs []string `json:"hostnameAddrs,omitempty"`
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
	// SocketToken, made anew for each boot, opens each of init's
	// connections to PipePort, so that the host takes them from init alone
	// and not from the container's own processes, which cannot read the
	// spec.
	SocketToken string `json:"socketToken,omitempty"`
	// Serve, when not empty, boots the guest for runwhp itself rather than
	// for a container: with ServeLayers, init mounts the Layers disks
	// read-only, each on its own, serves their files on ArchivePort, sends
	// an Event with Ready, and waits for the host to stop the machine; with
	// ServeRoot, init prepares a stopped container's root and mounts as
	// for a run and serves them on RootPort. Either way it runs no process.
	Serve string `json:"serve,omitempty"`
}

// ServeLayers is Spec.Serve for a guest that serves image layers' files,
// for runwhp save.
const ServeLayers = "layers"

// Reclaim has init reclaim the guest's page cache once the guest has kept
// its processors idle for IdleSeconds, a step every IntervalSeconds, so
// that the memory goes back to the host through free page reporting. Zero
// IdleSeconds turns it off.
type Reclaim struct {
	IdleSeconds     int `json:"idleSeconds,omitempty"`
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// Network is the static configuration of one of the guest's network
// interfaces.
type Network struct {
	MAC     string `json:"mac"`     // identifies the interface, as in 02:00:00:00:00:0f
	Address string `json:"address"` // the guest's address and prefix, as in 10.0.2.15/24
	// Gateway, if set, is the default route's; one interface has it.
	Gateway string `json:"gateway,omitempty"`
	MTU     int    `json:"mtu,omitempty"`
}

// Resolv is what /etc/resolv.conf says.
type Resolv struct {
	Nameservers []string `json:"nameservers,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Mount kinds.
const (
	MountBind   = ""       // share Share of the virtio-fs device
	MountVolume = "volume" // the _data directory of volume disk Volume
	MountAnon   = "anon"   // an anonymous volume: a directory of this run's own
	MountTmpfs  = "tmpfs"  // a new tmpfs, mounted with Options
	// MountSocket is a Unix socket, at a File mount point, whose
	// connections reach the host's named pipe Pipe: init listens on it and
	// carries each connection to the host over vsock; see PipePort.
	MountSocket = "socket"
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
	// CopyUp fills an empty bind mount's share with what the image has at
	// Target, as a new volume is filled: containerd's volumes are host
	// directories, which nerdctl cannot fill, since runwhp's snapshotter
	// shows it no image files.
	CopyUp bool `json:"copyUp,omitempty"`
	// Options are a tmpfs's mount options: noexec, nosuid, nodev, ro, and
	// size, mode, uid, gid and nr_inodes as tmpfs takes them.
	Options string `json:"options,omitempty"`
	// Init, if set, gives a volume's _data directory this owner and mode
	// when init makes it, as a new RUN --mount=type=cache volume's.
	Init *MountInit `json:"init,omitempty"`
	// Pipe is a socket's named pipe on the host, as in
	// \\.\pipe\runwhp_engine, for the host's side; Source is what the
	// mount was asked for with, as in /var/run/docker.sock, for inspect.
	Pipe   string `json:"pipe,omitempty"`
	Source string `json:"source,omitempty"`
}

// MountInit is the owner and mode of a new volume's _data directory.
type MountInit struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Mode uint32 `json:"mode"`
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
	// Top, when not zero, asks init for the container's processes, which
	// it sends back in an Event with Processes.Seq set to Top.
	Top uint64 `json:"top,omitempty"`
	// Time, when not zero, is the host's wall clock as the message left
	// it, in nanoseconds since the Unix epoch. Init brings the guest's
	// clock to it: gradually when it is close, at once when not.
	Time int64 `json:"time,omitempty"`
	// Shutdown ends a guest booted with Spec.Serve set to ServeRoot: init
	// freezes its file systems and resets the machine, as after a
	// container's process.
	Shutdown bool `json:"shutdown,omitempty"`
	// Freeze, when set, freezes the container's processes, or with Frozen
	// false thaws them, as docker pause and unpause do. Init answers with
	// an Event whose Freeze has the same Seq, once they are, or with Error.
	Freeze *Freeze `json:"freeze,omitempty"`
}

// Freeze asks init to freeze or thaw the container's processes, and is
// init's answer. Init itself, and so the clock, statistics and the file
// server, are not frozen.
type Freeze struct {
	Seq    uint64 `json:"seq"`
	Frozen bool   `json:"frozen"`
	Error  string `json:"error,omitempty"`
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
	// Top answers Control.Top.
	Top *Processes `json:"top,omitempty"`
	// Freeze answers Control.Freeze.
	Freeze *Freeze `json:"freeze,omitempty"`
}

// Processes is init's list of the container's processes: every process
// in the guest but init itself and the kernel's own, as ps would show
// them in the container's PID namespace.
type Processes struct {
	Seq   uint64    `json:"seq"`
	Procs []Process `json:"procs"`
	// MemTotal is the guest's RAM, in bytes, for each process's share.
	MemTotal uint64 `json:"memTotal"`
	// Now is the guest's clock as init listed the processes.
	Now   time.Time `json:"now"`
	Error string    `json:"error,omitempty"`
}

// Process is one process, from /proc/PID/stat, status and cmdline.
type Process struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	PGID    int    `json:"pgid"`
	SID     int    `json:"sid"`
	TPGID   int    `json:"tpgid"` // the terminal's foreground process group
	UID     uint32 `json:"uid"`
	User    string `json:"user,omitempty"` // in the container's /etc/passwd
	State   string `json:"state"`          // R, S, D, Z, T...
	TTY     uint32 `json:"tty,omitempty"`  // the terminal's device number
	Nice    int    `json:"nice,omitempty"`
	Threads int    `json:"threads"`
	// Start is when it started; CPU the processor time it has used.
	Start time.Time     `json:"start"`
	CPU   time.Duration `json:"cpu"`
	// VSZ and RSS are its virtual and resident memory, in bytes.
	VSZ  uint64   `json:"vsz"`
	RSS  uint64   `json:"rss"`
	Comm string   `json:"comm"`
	Args []string `json:"args,omitempty"`
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
// /dev/null and no streams, and goes on if the host closes the session
// once it has started; init reports its end to a host that did not.
//
// When the container's process exits, init kills every process left,
// those of runwhp exec included.
const ExecPort = 1024

// PipePort is the host's vsock port for MountSocket mounts. For each
// connection made to a socket, init connects to it and sends a
// SocketHello line; the host answers with one byte, SocketOK once it has
// connected to the mount's pipe, anything else if it could not, and then
// carries bytes both ways, each side's half-close included.
const PipePort = 1027

// SocketHello opens a connection to PipePort.
type SocketHello struct {
	Token string `json:"token"`
	Mount int    `json:"mount"` // the socket's index in Spec.Mounts
}

// SocketOK is the host's answer to a SocketHello it has connected.
const SocketOK = 0

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
