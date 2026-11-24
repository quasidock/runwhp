//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/vmm"
)

const pauseUsage = `Usage: runwhp pause CONTAINER...

Pause containers: freeze every process in each, until runwhp unpause.
runwhp itself goes on serving the container: stats, logs, cp, diff and
export work, and kill and stop thaw it to deliver their signals.
`

const unpauseUsage = `Usage: runwhp unpause CONTAINER...

Unpause containers: thaw the processes runwhp pause froze.
`

func cmdPause(args []string, stdout io.Writer) int {
	return pauseCommand("pause", pauseUsage, container.OpPause, args, stdout)
}

func cmdUnpause(args []string, stdout io.Writer) int {
	return pauseCommand("unpause", unpauseUsage, container.OpUnpause, args, stdout)
}

func pauseCommand(name, usage, op string, args []string, stdout io.Writer) int {
	fs := newFlags(name, usage)
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	root, _ := storeRoot()
	code := 0
	for _, ref := range fs.Args() {
		c, err := store.Resolve(ref)
		if err == nil {
			err = askPause(root, c, op)
		}
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		fmt.Fprintln(stdout, ref)
	}
	return code
}

// askPause asks the runwhp running c to pause or unpause it, and gives
// its answer as Docker words it.
func askPause(root string, c *container.Container, op string) error {
	notRunning := fmt.Errorf("container %s is not running", c.ID)
	if op == container.OpUnpause {
		notRunning = fmt.Errorf("Container %s is not paused", c.ID)
	}
	if !c.State.Active() {
		return notRunning
	}
	ctx, cancel := context.WithTimeout(context.Background(), vmm.FreezeWait+5*time.Second)
	defer cancel()
	rep, err := container.Ask(ctx, root, c.ID, container.Request{Op: op})
	switch {
	case errors.Is(err, container.ErrNoRunwhp):
		return notRunning
	case err != nil:
		return fmt.Errorf("container %s: %w", c.Short(), err)
	case !rep.OK:
		return errors.New(rep.Error)
	}
	return nil
}
