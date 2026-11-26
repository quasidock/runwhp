//go:build windows

package build

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/rootfs"
	"github.com/quasidock/runwhp/internal/vmm"
)

// cacheVolumeSize is a RUN --mount=type=cache volume's, as a named
// volume's: sparse, so it takes only what is written.
const cacheVolumeSize = 64 << 30

// VM is the Worker that runs a build's steps in guests, on the store at
// Root.
type VM struct {
	Root   string
	Images *image.Store
	Policy image.Policy
	// Memory, CPUs and StorageSize size each RUN step's guest and its
	// writable layer.
	Memory      uint64
	CPUs        int
	StorageSize int64
	// Kernel is the kernel to boot, if not runwhp's own.
	Kernel string
	Debug  bool
}

// Image is Worker's.
func (v *VM) Image(ctx context.Context, ref string) (string, []byte, error) {
	img, err := v.Images.Ensure(ctx, ref, v.Policy)
	if err != nil {
		return "", nil, err
	}
	raw, err := v.Images.RawConfig(img)
	if errors.Is(err, image.ErrNoBlob) {
		// A record from before the store kept configuration blobs: its
		// own copy says the same, if not in the same bytes.
		raw, err = img.Config, nil
	}
	if err != nil {
		return "", nil, err
	}
	return img.ID(), raw, nil
}

// HasLayer is Worker's.
func (v *VM) HasLayer(diffID string) bool { return v.Images.Layers.Has(diffID) }

// Import is Worker's.
func (v *VM) Import(r io.Reader) (Layer, error) {
	d, empty, err := v.Images.ImportDiff(r)
	return Layer{DiffID: d, Empty: empty}, err
}

// config is a guest's on layers. An empty layer, as an image's history
// may list one, is no file in the store, and no disk in the guest.
func (v *VM) config(layers []string) *container.Config {
	layers = slices.DeleteFunc(slices.Clone(layers), func(d string) bool { return d == layer.EmptyDiffID })
	return &container.Config{
		ID: container.NewID(), Layers: layers, Hostname: Hostname,
		Memory: v.Memory, CPUs: v.CPUs, StorageSize: v.StorageSize,
		Kernel: v.Kernel, Debug: v.Debug,
	}
}

// Open is Worker's: a guest serving layers, with no writable layer of its
// own.
func (v *VM) Open(ctx context.Context, layers []string) (SourceRoot, error) {
	cfg := v.config(layers)
	if err := vmm.CheckDevices(cfg, 0); err != nil {
		return nil, err
	}
	g, err := vmm.BootStep(ctx, v.Root, vmm.Step{Config: cfg})
	if err != nil {
		return nil, err
	}
	return &guestRoot{client(g, "the stage copied from"), g}, nil
}

// guestRoot is a guest's root, closed with its guest.
type guestRoot struct {
	*rootfs.Client
	g *vmm.Guest
}

func (r *guestRoot) Close() error {
	r.g.Close()
	return nil
}

// client reaches g's root.
func client(g *vmm.Guest, name string) *rootfs.Client {
	return &rootfs.Client{Name: name, Dial: func(ctx context.Context) (io.ReadWriteCloser, *bufio.Reader, error) {
		conn, err := g.Dial(ctx)
		if err != nil {
			return nil, nil, err
		}
		return conn, bufio.NewReader(conn), nil
	}}
}

// Step is Worker's: a guest on s.Layers with a writable layer of its own,
// which Fill puts files into and Run's process changes, and then the
// changes read back as a layer, by the same guest for a copy, and by a
// second one, on the same writable layer, after a process, since a
// guest stops when its process does.
func (v *VM) Step(ctx context.Context, s *Step) (Layer, error) {
	cfg := v.config(s.Layers)
	if s.Run != nil {
		if err := v.process(cfg, s.Run); err != nil {
			return Layer{}, err
		}
	}
	if err := vmm.CheckDevices(cfg, len(cfg.Volumes)); err != nil {
		return Layer{}, err
	}
	rw, err := vmm.NewStepLayer(v.Root, v.StorageSize)
	if err != nil {
		return Layer{}, err
	}
	defer rw.Close()
	step := vmm.Step{Config: cfg, Process: s.Run != nil, Layer: rw}
	if s.Run != nil {
		step.IO = vmm.BootIO{Stdout: s.Run.Output, Stderr: s.Run.Output}
	}
	g, err := vmm.BootStep(ctx, v.Root, step)
	if err != nil {
		return Layer{}, err
	}
	defer func() {
		if g != nil {
			g.Close()
		}
	}()
	if s.Fill != nil {
		if err := s.Fill(ctx, client(g, "the build step")); err != nil {
			return Layer{}, err
		}
	}
	if s.Run != nil {
		code, err := g.Run(ctx)
		g.Close()
		g = nil
		if err != nil {
			return Layer{}, err
		}
		if code != 0 {
			return Layer{}, &ExitError{Code: code}
		}
		// The changes, from a guest on the layer the process left.
		if g, err = vmm.BootStep(ctx, v.Root, vmm.Step{Config: v.config(s.Layers), Layer: rw}); err != nil {
			return Layer{}, err
		}
	}
	diff, err := client(g, "the build step").Tar(ctx, guestspec.RootDiff)
	if err != nil {
		return Layer{}, err
	}
	defer diff.Close()
	d, empty, err := v.Images.ImportDiff(diff)
	if err != nil {
		return Layer{}, fmt.Errorf("storing the step's layer: %w", err)
	}
	return Layer{DiffID: d, Empty: empty}, nil
}

// process sets cfg up to run p.
func (v *VM) process(cfg *container.Config, p *Process) error {
	cfg.Args, cfg.Env, cfg.Cwd, cfg.User = p.Args, p.Env, p.Cwd, p.User
	if p.Hostname != "" {
		cfg.Hostname = p.Hostname
	}
	cfg.Network = p.Network
	cfg.HideMountPoints = true
	for _, h := range p.Hosts {
		line, gateway, err := netstack.HostsLine(h)
		if err != nil {
			return err
		}
		if gateway && !p.Network {
			return errors.New("--network none cannot be combined with --add-host NAME:host-gateway")
		}
		cfg.Hosts = append(cfg.Hosts, line)
		cfg.HostGateway = cfg.HostGateway || gateway
	}
	cacheRoot := buildcache.Open(v.Root).VolumeRoot()
	for _, m := range p.Mounts {
		gm := guestspec.Mount{Target: m.Target, ReadOnly: m.ReadOnly}
		switch m.Kind {
		case "cache":
			gm.Kind, gm.Volume, gm.Name, gm.NoCopy = guestspec.MountVolume, len(cfg.Volumes), m.ID, true
			if m.UID != nil || m.GID != nil || m.Mode != nil {
				in := &guestspec.MountInit{Mode: 0o755}
				if m.UID != nil {
					in.UID = int(*m.UID)
				}
				if m.GID != nil {
					in.GID = int(*m.GID)
				}
				if m.Mode != nil {
					in.Mode = uint32(*m.Mode)
				}
				gm.Init = in
			}
			cfg.Volumes = append(cfg.Volumes, container.Volume{Name: m.ID, Size: cacheVolumeSize, Root: cacheRoot})
		case "tmpfs":
			gm.Kind = guestspec.MountTmpfs
		case "bind", "secret":
			fi, err := os.Stat(m.Source)
			if err != nil {
				return err
			}
			gm.Kind, gm.Share, gm.File, gm.ReadOnly = guestspec.MountBind, len(cfg.Shares), !fi.IsDir(), true
			cfg.Shares = append(cfg.Shares, container.Share{Path: m.Source, File: gm.File, ReadOnly: true, Target: m.Target})
		default:
			return fmt.Errorf("a mount of kind %q", m.Kind)
		}
		cfg.Mounts = append(cfg.Mounts, gm)
	}
	sort.SliceStable(cfg.Mounts, func(i, j int) bool {
		return strings.Count(cfg.Mounts[i].Target, "/") < strings.Count(cfg.Mounts[j].Target, "/")
	})
	return nil
}
