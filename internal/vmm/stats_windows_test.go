//go:build windows

package vmm

import (
	"sync"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// TestGuestStats checks that an answer is matched to its request: a late
// answer to one that gave up is not taken for the next.
func TestGuestStats(t *testing.T) {
	var mu sync.Mutex
	listening := false
	var sent []guestspec.Control
	g := &guestStats{
		listening: func() bool { mu.Lock(); defer mu.Unlock(); return listening },
		send:      func(c guestspec.Control) bool { mu.Lock(); sent = append(sent, c); mu.Unlock(); return true },
		replies:   make(chan *guestspec.GuestStats, 1),
	}
	if g.get(time.Millisecond) != nil || len(sent) != 0 {
		t.Error("asked init before it listened")
	}
	mu.Lock()
	listening = true
	mu.Unlock()
	// Nobody answers the first request.
	if g.get(10*time.Millisecond) != nil || len(sent) != 1 || sent[0].Stats != 1 {
		t.Fatalf("sent %+v", sent)
	}
	// Its answer arrives late, just as the second is answered.
	g.deliver(&guestspec.GuestStats{Seq: 1, Tasks: 1})
	go func() {
		for {
			mu.Lock()
			n := len(sent)
			mu.Unlock()
			if n == 2 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		g.deliver(&guestspec.GuestStats{Seq: 2, Tasks: 2})
	}()
	if st := g.get(5 * time.Second); st == nil || st.Seq != 2 || st.Tasks != 2 {
		t.Errorf("second answer %+v", st)
	}
	// deliver never blocks, even with nobody waiting.
	g.deliver(&guestspec.GuestStats{Seq: 9})
	g.deliver(&guestspec.GuestStats{Seq: 10})
}

func TestEventWriterStats(t *testing.T) {
	var got []uint64
	w := eventWriter{onStats: func(st *guestspec.GuestStats) { got = append(got, st.Seq) }}
	w.Write([]byte(`{"stats":{"seq":3,"memTotal":1}}` + "\n" + `{"started":true,"pid":5}` + "\n"))
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("stats events: %v", got)
	}
}
