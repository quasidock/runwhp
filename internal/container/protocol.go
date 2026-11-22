package container

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
)

// Request is a request to a container's runwhp.
type Request struct {
	V       int     `json:"v"`
	Op      string  `json:"op"`
	Signal  int     `json:"signal,omitempty"`
	All     bool    `json:"all,omitempty"`
	Timeout float64 `json:"timeout,omitempty"` // seconds
	Port    uint32  `json:"port,omitempty"`
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
}
