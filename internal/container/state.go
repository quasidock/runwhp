package container

import (
	"time"

	"github.com/quasidock/runwhp/internal/netstack"
)

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
	// Restarting: the process has ended and the VM is gone, and the runwhp
	// running the container waits to start it again, as its restart policy
	// says. Not one of the OCI spec's, which lets runtimes add their own.
	Restarting Status = "restarting"
	// Paused is not a status a container's state holds: a paused container
	// is Running, with Paused set, as Docker's is. runwhp state shows it,
	// as docker inspect's State.Status does.
	Paused Status = "paused"
)

// Output is where a container's output goes when no terminal is attached.
const (
	OutputTerminal = "terminal" // the console or pipes of a foreground run
	OutputFile     = "file"     // files the caller redirected to
	OutputLog      = "log"      // the container's log
	OutputNone     = "none"     // nowhere: --log-driver none
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
	// RestartCount counts the times the runwhp running the container has
	// started it again, as its restart policy says.
	RestartCount int `json:"restartCount,omitempty"`
	// Paused says the running container's processes are frozen, by runwhp
	// pause, until runwhp unpause.
	Paused bool `json:"paused,omitempty"`
	// Health is how the container's healthcheck has gone, since it last
	// started; a stopped container keeps its last, as Docker's does.
	Health *Health `json:"health,omitempty"`
	// Ports are the ports published on the host while the container has a
	// VM, as they were bound: a random host port has the one the system
	// chose, which the next boot may not get again.
	Ports []netstack.Port `json:"ports,omitempty"`
}

// Published is a container's published ports: as bound while it has a VM,
// and as asked for otherwise, random host ports as 0.
func Published(cfg *Config, st State) []netstack.Port {
	if st.Active() && len(st.Ports) == len(cfg.Publish) {
		return st.Ports
	}
	return cfg.Publish
}

// HealthStatus is the container's health as list shows and filters it:
// none without a healthcheck.
func (s State) HealthStatus() string {
	if s.Health == nil || s.Health.Status == "" {
		return HealthNone
	}
	return s.Health.Status
}

// Active reports whether the state says a runwhp runs the container: it
// has a VM, or is waiting to start it again.
func (s State) Active() bool {
	return s.Status == Creating || s.Status == Created || s.Status == Running || s.Status == Restarting
}
