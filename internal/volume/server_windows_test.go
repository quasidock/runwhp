//go:build windows

package volume

import (
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Microsoft/go-winio"
)

func TestShared(t *testing.T) {
	s := Open(testRoot(t))
	v, err := s.CreateOpts("plain", Options{Size: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if v.Shared {
		t.Fatal("a new volume is shared")
	}
	if changed, err := s.SetShared("PLAIN"); err != nil || !changed {
		t.Fatalf("SetShared: %v %v", changed, err)
	}
	if changed, err := s.SetShared("plain"); err != nil || changed {
		t.Fatalf("SetShared again: %v %v, want no change", changed, err)
	}
	got, err := s.Get("plain")
	if err != nil || !got.Shared || got.Size != 1<<30 || got.Name != "plain" {
		t.Fatalf("after SetShared: %+v %v", got, err)
	}
	if _, err := s.SetShared("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetShared of a missing volume: %v", err)
	}
	// EnsureOpts makes a shared volume, or marks an existing one.
	if v, made, err := s.EnsureOpts("made", Options{Size: 1 << 30, Shared: true}); err != nil || !made || !v.Shared {
		t.Fatalf("EnsureOpts made %+v %v %v", v, made, err)
	}
	s.CreateOpts("later", Options{Size: 1 << 30})
	if v, made, err := s.EnsureOpts("later", Options{Shared: true}); err != nil || made || !v.Shared {
		t.Fatalf("EnsureOpts of an existing volume: %+v %v %v", v, made, err)
	}
	if v, _ := s.Get("later"); !v.Shared {
		t.Fatal("EnsureOpts did not record the volume shared")
	}
}

func TestPipeName(t *testing.T) {
	root := testRoot(t)
	a, b := PipeName(root, "Data"), PipeName(strings.ToUpper(root), "data")
	if a != b {
		t.Errorf("pipe names differ by case: %s, %s", a, b)
	}
	if !strings.HasPrefix(a, `\\.\pipe\runwhp-`) || !strings.HasSuffix(a, "-vol-data") {
		t.Errorf("pipe name %s", a)
	}
	if PipeName(root+"x", "data") == a {
		t.Error("two stores share a volume's pipe name")
	}
}

// serve answers a volume's pipe as a server would, with clients
// containers, counting the stops it is asked for.
func serve(t *testing.T, v *Volume, clients int, stops *atomic.Int32) {
	t.Helper()
	l, err := winio.ListenPipe(v.PipeName(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := ReadRequest(c)
				if err != nil {
					return
				}
				switch req.Op {
				case OpStatus:
					WriteReply(c, ServerReply{OK: true, PID: 7, Clients: clients})
				case OpStop:
					if clients > 0 {
						WriteReply(c, ServerReply{Clients: clients})
						return
					}
					stops.Add(1)
					WriteReply(c, ServerReply{OK: true})
				case OpConnect:
					WriteReply(c, ServerReply{OK: true})
					c.Write([]byte("frames"))
				}
			}(c)
		}
	}()
}

func TestServerControl(t *testing.T) {
	s := Open(testRoot(t))
	idle, _ := s.CreateOpts("idle", Options{Size: 1 << 30, Shared: true})
	busy, _ := s.CreateOpts("busy", Options{Size: 1 << 30, Shared: true})
	if _, err := idle.ServerStatus(); !errors.Is(err, ErrNoServer) {
		t.Fatalf("status with no server: %v", err)
	}
	if idle.StopIdleServer() {
		t.Fatal("stopped a server that is not there")
	}
	var idleStops, busyStops atomic.Int32
	serve(t, idle, 0, &idleStops)
	serve(t, busy, 2, &busyStops)
	if st, err := busy.ServerStatus(); err != nil || st.Clients != 2 || st.PID != 7 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if busy.StopIdleServer() || busyStops.Load() != 0 {
		t.Fatal("stopped a server with clients")
	}
	if !idle.StopIdleServer() || idleStops.Load() != 1 {
		t.Fatal("did not stop an idle server")
	}
	c, err := idle.Connect(t.Context(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 6)
	if _, err := c.Read(buf); err != nil || string(buf) != "frames" {
		t.Fatalf("after connecting: %q %v", buf, err)
	}
}

func TestLockServer(t *testing.T) {
	s := Open(testRoot(t))
	v, _ := s.CreateOpts("locked", Options{Size: 1 << 30, Shared: true})
	l, err := v.LockServer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.LockServer(); !errors.Is(err, ErrServerBusy) {
		t.Fatalf("second lock: %v, want ErrServerBusy", err)
	}
	// A server holding its lock keeps the volume from being removed.
	if err := s.Remove("locked"); err == nil {
		t.Fatal("removed a volume whose server runs")
	}
	l.Close()
	if err := s.Remove("locked"); err != nil {
		t.Fatalf("remove once the server let go: %v", err)
	}
}
