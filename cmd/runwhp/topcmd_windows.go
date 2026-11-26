//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/guestspec"
)

const topUsage = `Usage: runwhp top CONTAINER [-ef|aux]

List a running container's processes, as ps -ef shows them, or ps aux.
They are the guest's processes but init's, with the guest's PIDs.
`

func cmdTop(args []string, stdout io.Writer) int {
	fs := newFlags("top", topUsage)
	// ps's options are not flags of top's: everything after the
	// container goes to ps, as with docker top.
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if fs.Parse(args) != nil || fs.NArg() < 1 {
			fs.Usage()
			return 2
		}
		args = fs.Args()
	}
	if len(args) < 1 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	c, err := store.Resolve(args[0])
	if err != nil {
		errorf("%v", err)
		return 1
	}
	procs, err := containerTop(context.Background(), store.Root(), c)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	titles, rows, err := dockerapi.Top(procs, strings.Join(args[1:], " "), time.Local)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 8, 3, ' ', 0)
	fmt.Fprintln(tw, strings.Join(titles, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
	return 0
}

// containerTop asks a running or paused container's runwhp for its
// processes.
func containerTop(ctx context.Context, root string, c *container.Container) (*guestspec.Processes, error) {
	notRunning := container.Refuse(container.ErrNotRunning, "container %s is not running", c.ID)
	if c.State.Status != container.Running {
		return nil, notRunning
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rep, err := container.Ask(ctx, root, c.ID, container.Request{Op: container.OpTop})
	if errors.Is(err, container.ErrNoRunwhp) {
		return nil, notRunning
	}
	if err != nil {
		return nil, fmt.Errorf("container %s: %w", c.Short(), err)
	}
	if err := container.ReplyError(rep); err != nil {
		return nil, fmt.Errorf("container %s: %w", c.Short(), err)
	}
	if rep.Top == nil {
		return nil, fmt.Errorf("container %s: its runwhp is too old to list processes", c.Short())
	}
	return rep.Top, nil
}
