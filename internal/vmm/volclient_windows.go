//go:build windows

package vmm

import (
	"context"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/volume"
)

const (
	// heldBootWait bounds the wait for a holder that is booting.
	heldBootWait = 30 * time.Second
	// heldUnknownWait bounds the wait for a disk whose holder no
	// container's state names: a guest serving a stopped container's
	// files, say, or a volume's server stopping.
	heldUnknownWait = 3 * time.Second
)

// resolveHeld is called when the disk of v, which is not shared, is
// another's, and container cfg needs v too. It makes v shared, if its
// holder lets it, and returns it, shared, once cfg can reach it through
// its server; or it returns why cfg cannot have it now.
func resolveHeld(ctx context.Context, root string, v *volume.Volume, cfg *container.Config, logf func(string, ...any)) (*volume.Volume, error) {
	store := volume.Open(root)
	start := time.Now()
	for {
		// Another container may have shared it meanwhile.
		cur, err := store.Get(v.Name)
		if err != nil {
			return nil, err
		}
		if cur.Shared {
			return cur, nil
		}
		h := findHolder(root, v.Name, cfg.ID)
		switch decideHeld(h) {
		case heldRefuse:
			return nil, &volume.InUseError{Name: v.Name, Holder: h.Name, Hint: "it needs the volume to itself"}
		case heldRefuseShared:
			if _, err := store.SetShared(v.Name); err != nil {
				return nil, err
			}
			logf("volume %s: shared from now on, as container %s holds it", v.Name, h.Name)
			return nil, &volume.InUseError{Name: v.Name, Holder: h.Name,
				Hint: "the volume is shared from now on, so that containers can use it at once, from the next time " + h.Name + " starts: restart it, then try again"}
		case heldReprepare:
			if _, err := store.SetShared(v.Name); err != nil {
				return nil, err
			}
			logf("volume %s: shared from now on; booting created container %s again to share it", v.Name, h.Name)
			actx, cancel := context.WithTimeout(ctx, volumeStart)
			rep, err := container.Ask(actx, root, h.ID, container.Request{Op: container.OpReprepare})
			cancel()
			if err == nil {
				err = container.ReplyError(rep)
			}
			if err == nil {
				return store.Get(v.Name)
			}
			logf("volume %s: booting container %s again: %v", v.Name, h.Name, err)
		}
		limit := heldBootWait
		if !h.Found {
			limit = heldUnknownWait
		}
		if time.Since(start) > limit {
			if h.Found {
				return nil, &volume.InUseError{Name: v.Name, Holder: h.Name}
			}
			return nil, &volume.InUseError{Name: v.Name}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// findHolder finds the container, other than self, whose VM has the disk
// of the volume called name.
func findHolder(root, name, self string) holder {
	cs, err := container.Open(root).Referencing(name)
	if err != nil {
		return holder{}
	}
	for _, c := range cs {
		if c.ID == self || !c.State.Active() {
			continue
		}
		for _, d := range c.State.DirectVolumes {
			if !strings.EqualFold(d, name) {
				continue
			}
			h := holder{Found: true, ID: c.ID, Name: c.Name, Status: c.State.Status}
			if h.Name == "" {
				h.Name = c.Short()
			}
			for _, u := range c.Config.Volumes {
				if strings.EqualFold(u.Name, name) && u.Exclusive {
					h.Exclusive = true
				}
			}
			return h
		}
	}
	return holder{}
}
