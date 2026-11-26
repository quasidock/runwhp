//go:build windows && compose

package main

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"

	"github.com/quasidock/runwhp/internal/build"
	"github.com/quasidock/runwhp/internal/compose"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/network"
)

// specOf turns the container compose makes for a service into the spec
// run would make of the same flags, with the same checks.
func specOf(c compose.Container, labels map[string]string) (runSpec, error) {
	s := runSpec{
		image: c.Image, command: c.Command, entrypoint: c.Entrypoint, entrypointSet: c.EntrypointSet,
		env: c.Env, workdir: c.WorkingDir, user: c.User, hostname: c.Hostname, interactive: c.StdinOpen, tty: c.TTY,
		labels: withLabels(c.Labels, labels),
	}
	var err error
	memory := ""
	if c.Memory > 0 {
		memory = strconv.FormatInt(c.Memory, 10)
	}
	if s.memory, err = parseMemory(memory); err != nil {
		return s, err
	}
	cpus := ""
	if c.CPUs > 0 {
		cpus = strconv.Itoa(c.CPUs)
	}
	if s.cpus, err = parseCPUs(cpus, numCPU()); err != nil {
		return s, err
	}
	if s.network, err = networkOf(c); err != nil {
		return s, err
	}
	for _, m := range c.Mounts {
		ms, err := mountOf(m)
		if err != nil {
			return s, err
		}
		s.mounts = append(s.mounts, ms)
	}
	var storage []string
	if c.StorageSize != "" {
		storage = []string{"size=" + c.StorageSize}
	}
	if s.storageSize, err = parseStorageOpts(storage); err != nil {
		return s, err
	}
	if s.logs, err = parseLogOptions(c.LogDriver, c.LogOpts); err != nil {
		return s, err
	}
	if s.restart, err = container.ParseRestartPolicy(c.Restart); err != nil {
		return s, err
	}
	if s.stopSignal, err = parseStopSignal(c.StopSignal); err != nil {
		return s, err
	}
	s.stopTimeout = c.StopTimeout
	if h := c.Health; h != nil {
		s.health = healthOverrides{test: h.Test, interval: h.Interval, timeout: h.Timeout, startPeriod: h.StartPeriod,
			startInterval: h.StartInterval, retries: h.Retries, disable: h.Disable}
		if err := s.health.check(); err != nil {
			return s, err
		}
	}
	return s, nil
}

// networkOf is what a compose container asks of the network.
func networkOf(c compose.Container) (networkRequest, error) {
	var r networkRequest
	switch {
	case c.NoNetwork:
		r.attach = []networkWant{{name: network.None}}
	case c.DefaultNetwork:
	default:
		for _, a := range c.Networks {
			r.attach = append(r.attach, networkWant{name: a.Name, ip: a.IPv4, aliases: a.Aliases})
		}
	}
	for _, p := range c.Ports {
		ip := netip.MustParseAddr("127.0.0.1")
		if p.HostIP != "" {
			var err error
			if ip, err = netip.ParseAddr(p.HostIP); err != nil {
				return r, fmt.Errorf("port %d: %q is not an IP address", p.Target, p.HostIP)
			}
		}
		r.publish = append(r.publish, netstack.Port{Host: netip.AddrPortFrom(ip, p.HostPort), Guest: p.Target})
	}
	for _, h := range c.ExtraHosts {
		line, gateway, err := netstack.HostsLine(h)
		if err != nil {
			return r, err
		}
		r.hosts = append(r.hosts, line)
		r.hostGateway = r.hostGateway || gateway
	}
	return r, nil
}

// mountOf is a compose mount as run's flags would give it.
func mountOf(m compose.Mount) (mountSpec, error) {
	if m.Kind == compose.MountTmpfs {
		s := m.Target
		if m.Options != "" {
			s += ":" + m.Options
		}
		return parseTmpfs(s)
	}
	target, err := containerPath(m.Target)
	if err != nil {
		return mountSpec{}, err
	}
	switch m.Kind {
	case compose.MountBind:
		return mountSpec{kind: guestspec.MountBind, source: m.Source, target: target, readOnly: m.ReadOnly, create: m.Create, delegated: m.Delegated}, nil
	case compose.MountVolume:
		return mountSpec{kind: guestspec.MountVolume, source: m.Source, target: target, readOnly: m.ReadOnly, noCopy: m.NoCopy}, nil
	}
	return mountSpec{}, fmt.Errorf("%s: mounts of kind %s are not supported", m.Target, m.Kind)
}

// buildRequestOf turns a service's build into build's request.
func buildRequestOf(b *compose.Build, noCache, pull bool) (buildRequest, error) {
	opts := build.Options{Target: b.Target, NoCache: b.NoCache || noCache, NoNetwork: b.NoNetwork, Hosts: b.Hosts,
		BuildArgs: b.Args, Labels: b.Labels, Secrets: map[string]build.Secret{}}
	if opts.Labels == nil {
		opts.Labels = map[string]string{}
	}
	var data []byte
	if b.Inline != "" {
		data = []byte(b.Inline)
	}
	if _, err := os.Stat(b.Context); err != nil {
		return buildRequest{}, fmt.Errorf("build context %s: %w", b.Context, err)
	}
	if err := openBuildContext(&opts, b.Context, b.Dockerfile, data); err != nil {
		return buildRequest{}, err
	}
	for id, path := range b.Secrets {
		opts.Secrets[id] = build.Secret{Path: path}
	}
	var refs []string
	for _, t := range b.Tags {
		n, err := image.Normalize(t)
		if err != nil {
			return buildRequest{}, fmt.Errorf("image name %q: %w", t, err)
		}
		refs = append(refs, n)
	}
	mem, err := parseMemory("")
	if err != nil {
		return buildRequest{}, err
	}
	ncpu, err := parseCPUs("", numCPU())
	if err != nil {
		return buildRequest{}, err
	}
	return buildRequest{opts: opts, refs: refs, pull: b.Pull || pull, memory: mem, cpus: ncpu}, nil
}
