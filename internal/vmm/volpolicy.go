package vmm

import "github.com/quasidock/runwhp/internal/container"

// When a container needs a volume another container's VM has the disk of,
// the volume becomes shared, if it can be: from then on, its server serves
// it to every container using it. Whether the container can have it now
// depends on the holder.

// holder is the container whose VM has a volume's disk, as its state
// says.
type holder struct {
	Found  bool
	ID     string
	Name   string // its name, or short ID
	Status container.Status
	// Exclusive says it needs the disk to itself, as the BuildKit
	// builder does.
	Exclusive bool
}

// heldAction is what a container needing a volume held by h does.
type heldAction int

const (
	// heldWait: the holder is not known, or is booting; look again
	// shortly.
	heldWait heldAction = iota
	// heldReprepare: the holder is created, its process not started:
	// share the volume, and have the holder boot again, onto the server.
	heldReprepare
	// heldRefuseShared: the holder runs: share the volume, from the
	// holder's next start, and refuse the container for now.
	heldRefuseShared
	// heldRefuse: the holder keeps the volume to itself.
	heldRefuse
)

func decideHeld(h holder) heldAction {
	switch {
	case !h.Found:
		return heldWait
	case h.Exclusive:
		return heldRefuse
	case h.Status == container.Created:
		return heldReprepare
	case h.Status == container.Creating:
		return heldWait
	}
	return heldRefuseShared
}
