package container

import "time"

// Status is where a container is in its life, by the OCI runtime spec's
// names.
type Status string

const (
	// Creating: the container has a record, and its VM is being booted.
	Creating Status = "creating"
	// Created: the VM is up and init has prepared the container's root,
	// but the process waits for start.
	Created Status = "created"
	// Running: the process runs.
	Running Status = "running"
	// Stopped: the process has ended and the VM is gone. Its writable layer
	// is kept, and starting it again boots a new VM on it.
	Stopped Status = "stopped"
)

// Output is where a container's output goes when no terminal is attached.
const (
	OutputTerminal = "terminal" // the console or pipes of a foreground run
	OutputFile     = "file"     // files the caller redirected to
	OutputLog      = "log"      // the container's log
)

// State is what changes as a container runs. Only the process holding the
// container's lock writes it.
type State struct {
	Status Status `json:"status"`
	// Pid is the runwhp process that owns the container's VM, while it has
	// one.
	Pid int `json:"pid,omitempty"`
	// GuestPid is the container process's pid inside the VM.
	GuestPid   int       `json:"guestPid,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	ExitCode   int       `json:"exitCode"`
	// Error is why the container could not be started, or ended without
	// its process exiting.
	Error  string `json:"error,omitempty"`
	Output string `json:"output,omitempty"`
}

// Active reports whether the state says the container has a VM.
func (s State) Active() bool {
	return s.Status == Creating || s.Status == Created || s.Status == Running
}
