package layer

import (
	"errors"
	"testing"

	"github.com/quasidock/runwhp/internal/disk"
)

// TestRemoveInUse removes a layer a guest has open, as a VM opens it.
func TestRemoveInUse(t *testing.T) {
	s := newStore(t)
	d := putTar(t, s, "a")
	p, _ := s.Path(d)
	f, err := disk.Open(p, disk.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(d); !errors.Is(err, ErrInUse) || !s.Has(d) {
		t.Errorf("Remove of an open layer: %v; still there: %v", err, s.Has(d))
	}
	f.Close()
	if err := s.Remove(d); err != nil || s.Has(d) {
		t.Errorf("Remove once closed: %v", err)
	}
}
