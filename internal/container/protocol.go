package container

import (
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// The runwhp that runs a container serves requests from other runwhps on a
// named pipe of the container's own: JSON lines, one request and one reply
// per connection, except that a connect request is followed by the bytes
// of a vsock connection to the guest.

// ProtocolVersion is the version of Request this runwhp sends. A running
// container may have been started by an older or newer runwhp.
const ProtocolVersion = 1

// Request operations.
const (
	// OpStart starts the process of a created container, and replies once
	// it runs or failed to.
	OpStart = "start"
	// OpKill sends Signal to the container's process, or to every process
	// in the container with All.
	OpKill = "kill"
	// OpStop sends the container's stop signal, then SIGKILL after Timeout
	// seconds, and replies once the container has stopped.
	OpStop = "stop"
	// OpWait replies once the container has stopped.
	OpWait = "wait"
	// OpConnect connects to vsock port Port in the guest, and after the
	// reply carries that connection's bytes both ways.
	OpConnect = "connect"
	// OpStats replies with what the running container uses, in Stats.
	OpStats = "stats"
	// OpAttach attaches to the container's standard streams; after the
	// reply, frames go both ways, as Attach* describes.
	OpAttach = "attach"
	// OpPause freezes the running container's processes, and OpUnpause
	// thaws them; each replies once that is done.
	OpPause   = "pause"
	OpUnpause = "unpause"
)

// After the reply to OpAttach, both ends send frames in the log's framing
// (see AppendFrame): the container's runwhp sends output as it comes, as
// AttachStdout and AttachStderr, and AttachExit once the container has
// stopped, last; the attached runwhp sends AttachStdin, which goes to the
// container's input if the reply's Stdin says it may, and AttachControl.
// An attached runwhp that closes its connection detaches.
const (
	// AttachStdin carries input. An empty one ends it, for a container
	// that takes input once (Config.StdinOnce); others ignore that.
	AttachStdin = 0
	// AttachStdout and AttachStderr carry output. With a terminal, all of
	// it comes as AttachStdout.
	AttachStdout = Stdout
	AttachStderr = Stderr
	// AttachExit holds a Reply, as JSON: how the container ended.
	AttachExit = 3
	// AttachControl holds a guestspec.Control, as JSON, of which only a
	// terminal size and a signal are taken.
	AttachControl = 4
)

// Request is a request to a container's runwhp.
type Request struct {
	V       int     `json:"v"`
	Op      string  `json:"op"`
	Signal  int     `json:"signal,omitempty"`
	All     bool    `json:"all,omitempty"`
	Timeout float64 `json:"timeout,omitempty"` // seconds
	Port    uint32  `json:"port,omitempty"`
	// Stdin asks OpAttach to pass on the attached runwhp's input.
	Stdin bool `json:"stdin,omitempty"`
}

// Reply is a container's runwhp's answer to a Request.
type Reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Status and ExitCode are the container's state when the reply was
	// made: after a start, stop or wait, how it stands or ended.
	Status   Status `json:"status,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	// Console holds the last kernel console lines, when a start failed.
	Console []string `json:"console,omitempty"`
	// Stats answers OpStats.
	Stats *Stats `json:"stats,omitempty"`
	// Stdin, in answer to OpAttach, says that the container's input is
	// open to attached runwhps: it was run in the background with -i and
	// neither a file nor a pipe for input.
	Stdin bool `json:"stdin,omitempty"`
}

// Stats is what a running container uses, as its runwhp sees it.
type Stats struct {
	Time time.Time `json:"time"`
	CPUs int       `json:"cpus"`
	// CPUTime is how long the container's vCPUs have run, all together.
	CPUTime time.Duration `json:"cpuTime"`
	// MemLimit is the guest's RAM, and Committed how much of it Windows
	// has committed to it now.
	MemLimit  uint64 `json:"memLimit"`
	Committed uint64 `json:"committed"`
	// HostCommit and HostWorkingSet are what Windows holds for the
	// container's runwhp process, guest RAM included: its commit and its
	// private working set.
	HostCommit     uint64 `json:"hostCommit"`
	HostWorkingSet uint64 `json:"hostWorkingSet"`
	// Network bytes the container received and sent, and disk bytes it
	// read and wrote.
	NetRx      uint64 `json:"netRx"`
	NetTx      uint64 `json:"netTx"`
	BlockRead  uint64 `json:"blockRead"`
	BlockWrite uint64 `json:"blockWrite"`
	// Guest is what init reported of the guest, or nil if it did not
	// answer in time.
	Guest *guestspec.GuestStats `json:"guest,omitempty"`
}
