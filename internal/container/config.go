// Package container keeps runwhp's containers in the store: each one's
// configuration, resolved once when it is created, its state, its writable
// layer and its log.
package container

import (
	"errors"
	"fmt"
	"sort"
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
	// ImageID is the image's ID, its configuration's digest, which commit
	// builds on, and which keeps that configuration in the store.
	ImageID string `json:"imageID,omitempty"`
	// Layers are the image's layers by diff ID, bottom first. LayerFiles
	// are ext4 files instead: given with --layer, for development, or a
	// containerd snapshot's.
	Layers     []string `json:"layers,omitempty"`
	LayerFiles []string `json:"layerFiles,omitempty"`
	// RWPath is the writable layer, when it is not the container's own
	// rw.ext4: a containerd snapshot's, kept with it rather than with the
	// container.
	RWPath string `json:"rwPath,omitempty"`
	// Containerd names the containerd task the container runs, for one a
	// containerd shim made.
	Containerd *ContainerdTask `json:"containerd,omitempty"`

	// The process. Entrypoint and Cmd are Args, split as Docker keeps
	// them, for commit; containers made before iteration 15 lack them.
	Args       []string `json:"args"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	User       string   `json:"user,omitempty"`
	StopSignal string   `json:"stopSignal,omitempty"` // --stop-signal, or the image's STOPSIGNAL
	// StopTimeout is how many seconds stop waits after the stop signal
	// before SIGKILL, when it is not told: --stop-timeout's. Nil means 10.
	StopTimeout *int `json:"stopTimeout,omitempty"`
	// Healthcheck is the check the container's runwhp runs while it runs,
	// or nil.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Stdin       bool         `json:"stdin,omitempty"`
	// StdinOnce, for a container in the background whose input is open to
	// attached runwhps, ends that input when the first of them ends its
	// own, as docker create -i does. Without it, the input stays open.
	StdinOnce bool `json:"stdinOnce,omitempty"`
	TTY       bool `json:"tty,omitempty"`

	// Labels are the image's labels, with --label-file's and --label's over
	// them, as Docker keeps a container's.
	Labels map[string]string `json:"labels,omitempty"`

	Hostname string `json:"hostname"`
	// Hosts are extra /etc/hosts lines, "ADDRESS NAME".
	Hosts []string `json:"hosts,omitempty"`
	// Network says the container has a network device: on the default
	// network, or on each of Networks.
	Network bool `json:"network"`
	// NetworkMode is the network --network named first: user, none or a
	// network's name, as Docker's HostConfig keeps it. Empty, as in
	// containers made before iteration 18, means user or none, as Network
	// says.
	NetworkMode string `json:"networkMode,omitempty"`
	// Networks are the user-defined networks the container is on, by name:
	// the first is its primary, with the default route, as Docker routes
	// through the network whose name sorts first. With any, the container
	// is not on the default network.
	Networks []Attachment `json:"networks,omitempty"`
	// Publish are the ports published on the host, as asked for: a random
	// host port is 0, and State.Ports has what each boot bound.
	Publish     []netstack.Port `json:"publish,omitempty"`
	HostGateway bool            `json:"hostGateway,omitempty"`
	// DNS, DNSSearch and DNSOptions are --dns, --dns-search and
	// --dns-option's, as Docker's HostConfig keeps them: on the default
	// network resolv.conf's servers, search domains and options; on a
	// user-defined one, where the embedded server answers, the servers it
	// passes other names to. A search domain of "." means none.
	DNS        []string `json:"dns,omitempty"`
	DNSSearch  []string `json:"dnsSearch,omitempty"`
	DNSOptions []string `json:"dnsOptions,omitempty"`
	// Expose are the ports the container says it listens on, "80/tcp", as
	// Docker's ExposedPorts: its image's, --expose's and the published
	// ones. Containers made before iteration 21 have none.
	Expose []string `json:"expose,omitempty"`
	// PublishAll says -P published the exposed ports, among Publish.
	PublishAll bool `json:"publishAll,omitempty"`

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

	// HideMountPoints keeps the mount points of Mounts out of the writable
	// layer, as a build step's are.
	HideMountPoints bool `json:"hideMountPoints,omitempty"`
	// Privileged is --privileged: the container's cgroup read-write, with
	// controllers to delegate, /sys read-write, and more devices.
	Privileged bool `json:"privileged,omitempty"`

	// AutoRemove removes the container when it stops, as --rm asks.
	AutoRemove bool `json:"autoRemove,omitempty"`
	// Restart is when the runwhp running the container starts it again.
	Restart RestartPolicy `json:"restart,omitzero"`

	// The log: LogDriver is LogDriverLocal, runwhp's own, or LogDriverNone;
	// empty means local. LogDriverJSONFile is runwhp's own too, under the
	// name it was asked for by, which Docker's clients look for. LogMaxSize and LogMaxFiles bound it, 0 meaning
	// the defaults and a negative LogMaxSize no bound.
	LogDriver   string `json:"logDriver,omitempty"`
	LogMaxSize  int64  `json:"logMaxSize,omitempty"`
	LogMaxFiles int    `json:"logMaxFiles,omitempty"`
}

// Attachment is a container's place on a user-defined network.
type Attachment struct {
	// Network is the network's ID, and Name its name.
	Network string `json:"network"`
	Name    string `json:"name"`
	// Aliases are further names the container has on the network.
	Aliases []string `json:"aliases,omitempty"`
	// IPv4 is the address --ip asked for; empty means any.
	IPv4 string `json:"ipv4,omitempty"`
}

// Mode is the container's network mode, as NetworkMode keeps it.
func (c *Config) Mode() string {
	switch {
	case c.NetworkMode != "":
		return c.NetworkMode
	case len(c.Networks) > 0:
		return c.Networks[0].Name
	case c.Network:
		return "user"
	}
	return "none"
}

// ErrNetworksRunning refuses to connect or disconnect a running container.
var ErrNetworksRunning = errors.New("cannot connect a running container to a network, nor disconnect it: its VM cannot take or give up a network device while it runs; stop the container first")

// Connect is the configuration with the container on network a too, which
// it must not be on yet. A container on the default network, or none,
// cannot join another.
func (c *Config) Connect(a Attachment) (*Config, error) {
	switch c.Mode() {
	case "none":
		return nil, errors.New("container cannot be connected to multiple networks with one of the networks in private (none) mode")
	case "user":
		return nil, fmt.Errorf("container %s is on runwhp's default network, which cannot be joined with another; make it with --network %s instead", c.ID, a.Name)
	}
	for _, x := range c.Networks {
		if x.Network == a.Network {
			return nil, fmt.Errorf("endpoint with name %s already exists in network %s", c.ID[:12], a.Name)
		}
	}
	n := *c
	n.Networks = append(append([]Attachment(nil), c.Networks...), a)
	SortNetworks(n.Networks)
	n.Network = true
	return &n, nil
}

// Disconnect is the configuration with the container off the network with
// the ID id, called name. Off its last network, it has loopback alone, as
// with Docker.
func (c *Config) Disconnect(id, name string) (*Config, error) {
	n := *c
	n.Networks = nil
	for _, x := range c.Networks {
		if x.Network != id {
			n.Networks = append(n.Networks, x)
		}
	}
	if len(n.Networks) == len(c.Networks) {
		return nil, fmt.Errorf("container %s is not connected to network %s", c.ID, name)
	}
	n.Network = len(n.Networks) > 0
	return &n, nil
}

// SortNetworks puts the attachments in the order the guest is given them:
// by name, the primary first.
func SortNetworks(as []Attachment) {
	sort.SliceStable(as, func(i, j int) bool { return as[i].Name < as[j].Name })
}

// ContainerdTask is the containerd namespace and container ID of a
// container a containerd shim runs.
type ContainerdTask struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

// Log drivers.
const (
	LogDriverLocal    = "local"
	LogDriverNone     = "none"
	LogDriverJSONFile = "json-file"
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
	// Root, if set, is the store the volume is in, instead of the
	// container's: a build cache mount's are in the build cache's.
	Root string `json:"root,omitempty"`
	// Exclusive says the container has the volume's disk itself, shared
	// or not, and never shares it: a volume's server's own, and the
	// BuildKit builder's, whose overlayfs cannot have its upper layer on
	// FUSE.
	Exclusive bool `json:"exclusive,omitempty"`
}
