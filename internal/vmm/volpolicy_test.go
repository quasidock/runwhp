package vmm

import (
	"testing"

	"github.com/quasidock/runwhp/internal/container"
)

func TestDecideHeld(t *testing.T) {
	for _, tc := range []struct {
		h    holder
		want heldAction
	}{
		{holder{}, heldWait},
		{holder{Found: true, Status: container.Creating}, heldWait},
		{holder{Found: true, Status: container.Created}, heldReprepare},
		{holder{Found: true, Status: container.Running}, heldRefuseShared},
		{holder{Found: true, Status: container.Restarting}, heldRefuseShared},
		{holder{Found: true, Status: container.Created, Exclusive: true}, heldRefuse},
		{holder{Found: true, Status: container.Running, Exclusive: true}, heldRefuse},
	} {
		if got := decideHeld(tc.h); got != tc.want {
			t.Errorf("decideHeld(%+v) = %v, want %v", tc.h, got, tc.want)
		}
	}
}
