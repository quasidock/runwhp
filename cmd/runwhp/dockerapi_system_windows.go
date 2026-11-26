//go:build windows

package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/vm"
)

func (s *apiServer) getVersion(w http.ResponseWriter, _ *http.Request) error {
	v := system.VersionResponse{
		Platform:      system.PlatformInfo{Name: "runwhp"},
		Version:       versionString(),
		APIVersion:    dockerapi.APIVersion,
		MinAPIVersion: dockerapi.MinAPIVersion,
		Os:            "linux",
		Arch:          "amd64",
		GitCommit:     gitCommit(),
		GoVersion:     runtime.Version(),
		KernelVersion: kernel.Default.Tag,
		Components: []system.ComponentVersion{{
			Name: "Engine", Version: versionString(),
			Details: map[string]string{
				"ApiVersion": dockerapi.APIVersion, "MinAPIVersion": dockerapi.MinAPIVersion,
				"GitCommit": gitCommit(), "GoVersion": runtime.Version(),
				"Os": "linux", "Arch": "amd64", "KernelVersion": kernel.Default.Tag,
				"Experimental": "false",
			},
		}, {
			Name: "runwhp", Version: versionString(),
			Details: map[string]string{"Host": windowsVersion(), "Kernel": kernel.Default.Repository + ":" + kernel.Default.Tag},
		}},
	}
	writeJSON(w, v)
	return nil
}

// windowsVersion is the host's Windows version, as ver prints it.
func windowsVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

func (s *apiServer) getInfo(w http.ResponseWriter, _ *http.Request) error {
	info := system.Info{
		ID:                 "runwhp" + storeSuffix(s.root),
		Driver:             "runwhp",
		DriverStatus:       [][2]string{{"Layers", "ext4 images, one a layer, overlaid in each container's VM"}},
		Plugins:            system.PluginsInfo{Volume: []string{"local"}, Network: []string{"bridge", "null"}, Log: []string{"local", "json-file", "none"}},
		MemoryLimit:        true,
		PidsLimit:          false,
		NFd:                0,
		NGoroutines:        runtime.NumGoroutine(),
		SystemTime:         time.Now().Format(time.RFC3339Nano),
		LoggingDriver:      "local",
		CgroupDriver:       "none",
		NEventsListener:    s.eventListeners(),
		KernelVersion:      kernel.Default.Tag,
		OperatingSystem:    "runwhp on " + windowsVersion(),
		OSVersion:          windowsVersion(),
		OSType:             "linux",
		Architecture:       "x86_64",
		IndexServerAddress: "https://index.docker.io/v1/",
		NCPU:               runtime.NumCPU(),
		MemTotal:           int64(vm.MaxMemory()),
		DockerRootDir:      s.root,
		ServerVersion:      versionString(),
		Runtimes:           map[string]system.RuntimeWithStatus{"runwhp": {}},
		DefaultRuntime:     "runwhp",
		Swarm:              swarm.Info{LocalNodeState: swarm.LocalNodeStateInactive},
		Isolation:          "",
		InitBinary:         "runwhp-init",
		SecurityOptions:    []string{"name=vm"},
		Labels:             []string{},
		CDISpecDirs:        []string{},
		Warnings: []string{
			"WARNING: runwhp runs each container in a virtual machine of its own; a created container's machine is already booted",
			"WARNING: runwhp publishes ports on 127.0.0.1 unless an address is given",
		},
	}
	info.Name, _ = os.Hostname()
	info.HTTPProxy, info.HTTPSProxy, info.NoProxy = os.Getenv("HTTP_PROXY"), os.Getenv("HTTPS_PROXY"), os.Getenv("NO_PROXY")
	if cs, err := s.containers.List(); err == nil {
		info.Containers = len(cs)
		for _, c := range cs {
			switch {
			case c.State.Paused && c.State.Active():
				info.ContainersPaused++
			case c.State.Status == container.Running || c.State.Status == container.Restarting:
				info.ContainersRunning++
			default:
				info.ContainersStopped++
			}
		}
	}
	if recs, err := image.Open(s.root).List(); err == nil {
		ids := map[string]bool{}
		for _, r := range recs {
			if r.Image != nil && r.Complete {
				ids[r.ID()] = true
			}
		}
		info.Images = len(ids)
	}
	writeJSON(w, info)
	return nil
}
