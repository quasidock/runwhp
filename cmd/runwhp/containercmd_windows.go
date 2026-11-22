//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/container"
)

// The container commands return 1 when they fail and 2 for a usage error,
// as volume and pull do.

// openContainers opens the store's containers, or reports why it cannot.
func openContainers() (*container.Store, bool) {
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return nil, false
	}
	return container.Open(root), true
}

func newFlags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	return fs
}

const listUsage = `Usage: runwhp list [-q] [--format table|json]

List containers, newest first, running or not.

  -q, --quiet        print only their IDs
      --format FMT   table (the default), or json: what runwhp state prints, for each
`

func cmdList(args []string, stdout io.Writer) int {
	fs := newFlags("list", listUsage)
	var quiet bool
	var format string
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.StringVar(&format, "format", "table", "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	if format != "table" && format != "json" {
		errorf("list: unknown format %q; use table or json", format)
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	cs, err := store.List()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	switch {
	case quiet:
		for _, c := range cs {
			fmt.Fprintln(stdout, c.ID)
		}
	case format == "json":
		out := make([]containerState, 0, len(cs))
		for _, c := range cs {
			out = append(out, stateOf(c))
		}
		printJSON(stdout, out)
	default:
		now := time.Now()
		tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "CONTAINER ID\tNAME\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS")
		for _, c := range cs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Short(), c.Name, imageOf(c.Config), commandOf(c.Config, 20),
				ago(now, c.Config.Created), statusText(c.State, now), portsOf(c))
		}
		tw.Flush()
	}
	return 0
}

const stateUsage = `Usage: runwhp state CONTAINER

Describe a container's state as JSON, as runc state does, with runwhp's own
fields added. CONTAINER is its ID, a unique prefix of it, or its name.
`

func cmdState(args []string, stdout io.Writer) int {
	fs := newFlags("state", stateUsage)
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	c, err := store.Resolve(fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return 1
	}
	printJSON(stdout, stateOf(c))
	return 0
}

const deleteUsage = `Usage: runwhp delete [-f] CONTAINER...

Remove containers, with their writable layers, logs and anonymous volumes.

  -f, --force   kill a running container first
`

func cmdDelete(args []string, stdout io.Writer) int {
	fs := newFlags("delete", deleteUsage)
	var force bool
	fs.BoolVar(&force, "f", false, "")
	fs.BoolVar(&force, "force", false, "")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	code := 0
	for _, ref := range fs.Args() {
		if err := deleteContainer(store, ref, force); err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

func deleteContainer(store *container.Store, ref string, force bool) error {
	c, err := store.Resolve(ref)
	if err != nil {
		return err
	}
	if c.State.Active() {
		// A created container's process never started, so it is deleted
		// as runc deletes one, without being forced.
		if !force && c.State.Status != container.Created {
			return fmt.Errorf("container %s is %s: stop it first, or delete it with -f", c.Short(), c.State.Status)
		}
		if err := killAndWait(store, c); err != nil {
			return err
		}
		if err := store.Remove(c); err != nil && !errors.Is(err, container.ErrNotFound) {
			return err
		}
		return nil // gone: removed by its runwhp, if it was run with --rm
	}
	err = store.Remove(c)
	if errors.Is(err, container.ErrRunning) {
		return fmt.Errorf("container %s is running: stop it first, or delete it with -f", c.Short())
	}
	return err
}

const renameUsage = `Usage: runwhp rename CONTAINER NAME

Give a container a new name.
`

func cmdRename(args []string) int {
	fs := newFlags("rename", renameUsage)
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	c, err := store.Resolve(fs.Arg(0))
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if err := store.Rename(c, fs.Arg(1)); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}

// containerState is what runwhp state prints: runc's fields, then runwhp's.
type containerState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Status      container.Status  `json:"status"`
	Pid         int               `json:"pid"` // the runwhp process running it, or 0
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`

	Name        string    `json:"name,omitempty"`
	Image       string    `json:"image,omitempty"`
	ImageDigest string    `json:"imageDigest,omitempty"`
	Command     []string  `json:"command"`
	Created     time.Time `json:"created"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
	ExitCode    *int      `json:"exitCode,omitempty"`
	Error       string    `json:"error,omitempty"`
	GuestPid    int       `json:"guestPid,omitempty"`
	AutoRemove  bool      `json:"autoRemove,omitempty"`
	Output      string    `json:"output,omitempty"`
	Ports       []string  `json:"ports,omitempty"`
	Volumes     []string  `json:"volumes,omitempty"`
}

func stateOf(c *container.Container) containerState {
	s := containerState{
		OCIVersion: "1.2.0", ID: c.ID, Status: c.State.Status, Pid: c.State.Pid, Bundle: c.Dir(),
		Name: c.Name, Image: imageOf(c.Config), ImageDigest: c.Config.ImageDigest, Command: c.Config.Args,
		Created: c.Config.Created, StartedAt: c.State.StartedAt, FinishedAt: c.State.FinishedAt,
		Error: c.State.Error, GuestPid: c.State.GuestPid, AutoRemove: c.Config.AutoRemove, Output: c.State.Output,
	}
	if c.State.Status == container.Stopped && !c.State.FinishedAt.IsZero() {
		code := c.State.ExitCode
		s.ExitCode = &code
	}
	for _, p := range c.Config.Publish {
		s.Ports = append(s.Ports, p.String())
	}
	for _, v := range c.Config.Volumes {
		s.Volumes = append(s.Volumes, v.Name)
	}
	return s
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")
	enc.Encode(v)
}

// imageOf is the image a container was made from, as it was given.
func imageOf(cfg *container.Config) string {
	if cfg.Image != "" {
		return cfg.Image
	}
	if len(cfg.LayerFiles) > 0 {
		return "(--layer)"
	}
	return ""
}

// commandOf is a container's command, quoted and cut to n characters as
// docker ps shows it.
func commandOf(cfg *container.Config, n int) string {
	cmd := strings.Join(cfg.Args, " ")
	if r := []rune(cmd); len(r) > n {
		cmd = string(r[:n-1]) + "…"
	}
	return strconv.Quote(cmd)
}

func portsOf(c *container.Container) string {
	var ps []string
	for _, p := range c.Config.Publish {
		ps = append(ps, p.String())
	}
	return strings.Join(ps, ", ")
}

// statusText describes a container's state as docker ps does.
func statusText(st container.State, now time.Time) string {
	switch st.Status {
	case container.Creating:
		return "Creating"
	case container.Created:
		return "Created"
	case container.Running:
		if st.StartedAt.IsZero() {
			return "Up"
		}
		return "Up " + humanDuration(now.Sub(st.StartedAt))
	case container.Stopped:
		if st.FinishedAt.IsZero() {
			return "Created" // made, but never started
		}
		return fmt.Sprintf("Exited (%d) %s", st.ExitCode, ago(now, st.FinishedAt))
	}
	return string(st.Status)
}

func ago(now, t time.Time) string { return humanDuration(now.Sub(t)) + " ago" }

// humanDuration says how long d is, roughly, as Docker does.
func humanDuration(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch s := int(d.Seconds()); {
	case s < 1:
		return "Less than a second"
	case s < 60:
		return plural(s, "second")
	case d.Minutes() < 2:
		return "About a minute"
	case d.Minutes() < 60:
		return plural(int(d.Minutes()), "minute")
	case d.Hours() < 2:
		return "About an hour"
	case d.Hours() < 48:
		return plural(int(d.Hours()), "hour")
	case d.Hours() < 24*7*2:
		return plural(int(d.Hours()/24), "day")
	case d.Hours() < 24*30*2:
		return plural(int(d.Hours()/24/7), "week")
	case d.Hours() < 24*365*2:
		return plural(int(d.Hours()/24/30), "month")
	}
	return plural(int(d.Hours()/24/365), "year")
}
