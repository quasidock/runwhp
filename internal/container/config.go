// Package container keeps runwhp's containers in the store: each one's
// configuration, resolved once when it is created, its state, its writable
// layer and its log.
package container

import (
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/netstack"
)

// ConfigVersion is the version of Config this runwhp writes.
const ConfigVersion = 1

// Config is everything needed to boot a container, resolved when it is
// created, so that starting it again does not depend on who starts it: its
// working directory, environment or flags.
type Config struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`

	// Image is the reference as given; ImageName the normalized one, as in
	// index.docker.io/library/alpine:3.20; ImageDigest the manifest's.
	Image       string `json:"image"`
	ImageName   string `json:"imageName,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
	// Layers are the image's layers by diff ID, bottom first. LayerFiles,
	// for development, are ext4 files given with --layer instead.
	Layers     []string `json:"layers,omitempty"`
	LayerFiles []string `json:"layerFiles,omitempty"`

	// The process.
	Args       []string `json:"args"`
	Env        []string `json:"env,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	User       string   `json:"user,omitempty"`
	StopSignal string   `json:"stopSignal,omitempty"` // the image's STOPSIGNAL
	Stdin      bool     `json:"stdin,omitempty"`
	// StdinOnce, for a container in the background whose input is open to
	// attached runwhps, ends that input when the first of them ends its
	// own, as docker create -i does. Without it, the input stays open.
	StdinOnce bool `json:"stdinOnce,omitempty"`
	TTY       bool `json:"tty,omitempty"`

	Hostname string `json:"hostname"`
	// Hosts are extra /etc/hosts lines, "ADDRESS NAME".
	Hosts       []string        `json:"hosts,omitempty"`
	Network     bool            `json:"network"`
	Publish     []netstack.Port `json:"publish,omitempty"`
	HostGateway bool            `json:"hostGateway,omitempty"`

	// Shares are the host directories and files bound into the container,
	// numbered as Mounts number them; Volumes the named volumes, one disk
	// each; Mounts every mount init makes, parents first.
	Shares  []Share           `json:"shares,omitempty"`
	Volumes []Volume          `json:"volumes,omitempty"`
	Mounts  []guestspec.Mount `json:"mounts,omitempty"`

	Memory      uint64 `json:"memory"`
	CPUs        int    `json:"cpus"`
	StorageSize int64  `json:"storageSize"`

	// Kernel is the bzImage --kernel or RUNWHP_KERNEL named. Empty means
	// runwhp's own kernel, whichever runwhp boots it.
	Kernel string `json:"kernel,omitempty"`
	// Development flags: the guest init and kernel modules to use.
	Init    string   `json:"init,omitempty"`
	Modules []string `json:"modules,omitempty"`
	Debug   bool     `json:"debug,omitempty"`

	// AutoRemove removes the container when it stops, as --rm asks.
	AutoRemove bool `json:"autoRemove,omitempty"`
	// Restart is when the runwhp running the container starts it again.
	Restart RestartPolicy `json:"restart,omitzero"`

	// The log: LogDriver is LogDriverLocal, runwhp's own, or LogDriverNone;
	// empty means local. LogMaxSize and LogMaxFiles bound it, 0 meaning
	// the defaults and a negative LogMaxSize no bound.
	LogDriver   string `json:"logDriver,omitempty"`
	LogMaxSize  int64  `json:"logMaxSize,omitempty"`
	LogMaxFiles int    `json:"logMaxFiles,omitempty"`
}

// Log drivers.
const (
	LogDriverLocal = "local"
	LogDriverNone  = "none"
)

// KeepsLog reports whether the container keeps a log.
func (c *Config) KeepsLog() bool { return c.LogDriver != LogDriverNone }

// LogOptions are the bounds of the container's log.
func (c *Config) LogOptions() LogOptions {
	o := LogOptions{MaxSize: c.LogMaxSize, MaxFiles: c.LogMaxFiles}
	if o.MaxSize == 0 {
		o.MaxSize = DefaultLogMaxSize
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = DefaultLogMaxFiles
	}
	return o
}

// Share is a host directory or file bound into the container.
type Share struct {
	Path     string `json:"path"` // absolute
	File     bool   `json:"file,omitempty"`
	ReadOnly bool   `json:"readonly,omitempty"`
	Target   string `json:"target"`
	// Create makes a missing directory at Path again, as -v does.
	Create bool `json:"create,omitempty"`
	// Delegated lets the guest cache writes, which reach the host later:
	// such shares are served by a file system of their own.
	Delegated bool `json:"delegated,omitempty"`
}

// Volume is a named volume the container mounts.
type Volume struct {
	Name string `json:"name"`
	// Size is the size to make it with, should it be missing.
	Size int64 `json:"size"`
}
