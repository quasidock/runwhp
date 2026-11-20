package virtio

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// fsHandlerFunc adapts a function to FsHandler.
type fsHandlerFunc func(req []byte, reply [][]byte) uint32

func (f fsHandlerFunc) Serve(req []byte, reply [][]byte) uint32 { return f(req, reply) }

func TestFsConfig(t *testing.T) {
	f := &Fs{Tag: "runwhp"}
	d := newDriver(t, f)
	tag := make([]byte, 0, fsTagLen)
	for i := 0; i < fsTagLen; i++ {
		tag = append(tag, byte(d.tr.Read(regConfig+uint64(i), 1)))
	}
	if got := string(bytes.TrimRight(tag, "\x00")); got != "runwhp" {
		t.Errorf("tag = %q", got)
	}
	if n := d.tr.Read(regConfig+fsTagLen, 4); n != 1 {
		t.Errorf("num_request_queues = %d, want 1", n)
	}
	if f.DeviceID() != 26 || f.NumQueues() != 2 {
		t.Errorf("device %d with %d queues, want 26 with 2", f.DeviceID(), f.NumQueues())
	}
	// There is no DAX cache window: shared memory region 0 reads as
	// length -1, which is how the driver learns it is missing.
	d.tr.Write(0x0ac, 4, 0) // SHMSel
	if lo, hi := d.tr.Read(regSHMLenLo, 4), d.tr.Read(regSHMLenHi, 4); lo != 0xffffffff || hi != 0xffffffff {
		t.Errorf("shared memory region length %#x:%#x, want -1", hi, lo)
	}
}

func TestFsRequest(t *testing.T) {
	var got []byte
	f := &Fs{Tag: "t", Handler: fsHandlerFunc(func(req []byte, reply [][]byte) uint32 {
		got = append([]byte(nil), req...)
		// A reply that spans both writable buffers.
		n := copy(reply[0], "0123")
		n += copy(reply[1], "456")
		return uint32(n)
	})}
	d := newDriver(t, f)
	in1, in2 := d.alloc(5), d.alloc(3)
	copy(d.mem[in1:], "hello")
	copy(d.mem[in2:], "abc")
	out1, out2 := d.alloc(4), d.alloc(16)
	head := d.add(fsRequestQueue, buf{in1, 5, false}, buf{in2, 3, false}, buf{out1, 4, true}, buf{out2, 16, true})
	d.kick(fsRequestQueue)
	used := d.collectN(fsRequestQueue, 1)
	if used[0].head != head || used[0].len != 7 {
		t.Fatalf("used %+v, want head %d len 7", used[0], head)
	}
	if string(got) != "helloabc" {
		t.Errorf("handler saw %q, want the readable buffers joined", got)
	}
	if string(d.mem[out1:out1+4])+string(d.mem[out2:out2+3]) != "0123456" {
		t.Errorf("reply not written to guest memory")
	}
	if f.Requests.Load() != 1 || f.BytesIn.Load() != 8 || f.BytesOut.Load() != 7 {
		t.Errorf("counters %d %d %d", f.Requests.Load(), f.BytesIn.Load(), f.BytesOut.Load())
	}
	if d.interrupts() == 0 {
		t.Error("no interrupt")
	}
}

// A slow request does not hold up a later one.
func TestFsOutOfOrder(t *testing.T) {
	release := make(chan struct{})
	f := &Fs{Tag: "t", Handler: fsHandlerFunc(func(req []byte, reply [][]byte) uint32 {
		if req[0] == 's' {
			<-release
		}
		return 0
	})}
	d := newDriver(t, f)
	a, b := d.alloc(1), d.alloc(1)
	d.mem[a], d.mem[b] = 's', 'f'
	slow := d.add(fsRequestQueue, buf{a, 1, false})
	fast := d.add(fsRequestQueue, buf{b, 1, false})
	d.kick(fsRequestQueue)
	if used := d.collectN(fsRequestQueue, 1); used[0].head != fast {
		t.Fatalf("first completion is head %d, want the fast request %d", used[0].head, fast)
	}
	close(release)
	if used := d.collectN(fsRequestQueue, 1); used[0].head != slow {
		t.Fatalf("second completion is head %d, want %d", used[0].head, slow)
	}
}

func TestFsHiprio(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	f := &Fs{Tag: "t", Handler: fsHandlerFunc(func(req []byte, reply [][]byte) uint32 {
		mu.Lock()
		seen = append(seen, string(req))
		mu.Unlock()
		if len(reply) != 0 {
			t.Errorf("forget came with %d reply buffers", len(reply))
		}
		return 0
	})}
	d := newDriver(t, f)
	a := d.alloc(6)
	copy(d.mem[a:], "forget")
	d.add(fsHiprioQueue, buf{a, 6, false})
	d.kick(fsHiprioQueue)
	if used := d.collectN(fsHiprioQueue, 1); used[0].len != 0 {
		t.Errorf("forget returned with length %d", used[0].len)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "forget" {
		t.Errorf("handler saw %q", seen)
	}
}

// Requests still being served when the driver resets the device are not
// returned to the new driver.
func TestFsResetDropsInflight(t *testing.T) {
	release := make(chan struct{})
	served := make(chan struct{}, 1)
	f := &Fs{Tag: "t", Handler: fsHandlerFunc(func(req []byte, reply [][]byte) uint32 {
		<-release
		served <- struct{}{}
		return 0
	})}
	d := newDriver(t, f)
	a := d.alloc(1)
	d.add(fsRequestQueue, buf{a, 1, false})
	d.kick(fsRequestQueue)
	d.tr.Write(regStatus, 4, 0) // reset
	for i := 0; i < f.NumQueues(); i++ {
		d.setupQueue(i, 16)
	}
	close(release)
	<-served
	if !f.Drain(context.Background()) {
		t.Fatal("Drain reported busy")
	}
	if used := d.collect(fsRequestQueue); len(used) != 0 {
		t.Errorf("a request from before the reset was returned: %+v", used)
	}
}

func TestFsDrain(t *testing.T) {
	release := make(chan struct{})
	f := &Fs{Tag: "t", Handler: fsHandlerFunc(func(req []byte, reply [][]byte) uint32 {
		<-release
		return 0
	})}
	d := newDriver(t, f)
	a := d.alloc(1)
	d.add(fsRequestQueue, buf{a, 1, false})
	d.kick(fsRequestQueue)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if f.Drain(ctx) {
		t.Fatal("Drain reported idle with a request in progress")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(release)
	}()
	if !f.Drain(context.Background()) {
		t.Fatal("Drain reported busy after the request finished")
	}
}
