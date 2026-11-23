package virtio

import "testing"

func TestQueueAddPublish(t *testing.T) {
	mem := make(testMem, 0x1000)
	q := &Queue{Size: 4, Ready: true, Desc: 0x100, Avail: 0x200, Used: 0x300}
	used := func() uint16 { return le.Uint16(mem[q.Used+2:]) }
	elem := func(i int) (uint32, uint32) {
		e := mem[q.Used+4+8*uint64(i):]
		return le.Uint32(e), le.Uint32(e[4:])
	}

	q.Add(mem, 1, 10)
	q.Add(mem, 2, 20)
	if used() != 0 {
		t.Fatalf("used index %d before Publish", used())
	}
	q.Publish(mem)
	if used() != 2 {
		t.Fatalf("used index %d after Publish, want 2", used())
	}
	if h, n := elem(0); h != 1 || n != 10 {
		t.Errorf("element 0 = %d, %d", h, n)
	}
	if h, n := elem(1); h != 2 || n != 20 {
		t.Errorf("element 1 = %d, %d", h, n)
	}
	q.Publish(mem) // nothing added: nothing changes
	q.Push(mem, 3, 30)
	if used() != 3 {
		t.Fatalf("used index %d after Push, want 3", used())
	}
	// Elements wrap around the ring.
	q.Add(mem, 0, 1)
	q.Add(mem, 1, 2)
	q.Publish(mem)
	if h, _ := elem(3); used() != 5 || h != 0 {
		t.Errorf("used %d, element 3's head %d", used(), h)
	}
	if h, n := elem(0); h != 1 || n != 2 {
		t.Errorf("element 0 after wrapping = %d, %d", h, n)
	}
}
