//go:build windows

package disk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testDir is a new empty directory, under RUNWHP_TEST_DIR when it is set, so
// the tests can run on another volume, such as one formatted with ReFS.
func testDir(t testing.TB) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "disk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func create(t testing.TB, size int64, m Mode) *File {
	t.Helper()
	f, err := Create(filepath.Join(testDir(t), "disk.img"), size, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestCreateSparse(t *testing.T) {
	const size = 64 << 30
	f := create(t, size, Exclusive)
	if got, err := f.Size(); err != nil || got != size {
		t.Fatalf("Size = %d, %v", got, err)
	}
	if got, err := Allocated(f.Name()); err != nil || got > 1<<20 {
		t.Errorf("a new 64 GiB file allocates %d bytes (%v)", got, err)
	}
	buf := bytes.Repeat([]byte{0xff}, 4096)
	if n, err := f.ReadAt(buf, size-4096); err != nil || n != 4096 || !bytes.Equal(buf, make([]byte, 4096)) {
		t.Errorf("reading the last block: %d, %v, nonzero data", n, err)
	}
	if n, err := f.ReadAt(buf, size-1024); err != io.EOF || n != 1024 {
		t.Errorf("reading across the end: %d, %v; want 1024, EOF", n, err)
	}
	if _, err := Create(f.Name(), size, Exclusive); err == nil {
		t.Error("Create succeeded on an existing file")
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	const workers, blocks, bs = 16, 64, 64 << 10
	f := create(t, workers*blocks*bs, Exclusive)
	pattern := func(w, b int) []byte {
		return bytes.Repeat([]byte{byte(w), byte(b), 0x5a, byte(w ^ b)}, bs/4)
	}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range blocks {
				off := int64((b*workers + w) * bs) // interleaved, so neighbours are written at once
				if _, err := f.WriteAt(pattern(w, b), off); err != nil {
					errs <- err
					return
				}
			}
			got := make([]byte, bs)
			for b := range blocks {
				off := int64((b*workers + w) * bs)
				if _, err := f.ReadAt(got, off); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, pattern(w, b)) {
					errs <- fmt.Errorf("worker %d block %d read back wrong data", w, b)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestZero(t *testing.T) {
	const size = 64 << 20
	f := create(t, size, Exclusive)
	data := bytes.Repeat([]byte{1}, 16<<20)
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	before, _ := Allocated(f.Name())
	if before < 16<<20 {
		t.Fatalf("16 MiB written but only %d bytes allocated", before)
	}
	if err := f.Zero(1<<20, 8<<20); err != nil {
		t.Fatal(err)
	}
	after, _ := Allocated(f.Name())
	t.Logf("allocated %d bytes before zeroing 8 MiB, %d after", before, after)
	if after > before-7<<20 {
		t.Errorf("zeroing 8 MiB gave back only %d bytes", before-after)
	}
	got := make([]byte, 10<<20)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:1<<20], data[:1<<20]) || !bytes.Equal(got[1<<20:9<<20], make([]byte, 8<<20)) || !bytes.Equal(got[9<<20:], data[:1<<20]) {
		t.Error("zeroed range or its neighbours read back wrong")
	}
}

// TestZeroGranularity reports which zeroed ranges give space back. It sets
// the discard alignment the block device advertises.
func TestZeroGranularity(t *testing.T) {
	for _, unit := range []int64{4 << 10, 16 << 10, 64 << 10, 256 << 10} {
		f := create(t, 16<<20, Exclusive)
		if _, err := f.WriteAt(bytes.Repeat([]byte{1}, 8<<20), 0); err != nil {
			t.Fatal(err)
		}
		f.Sync()
		before, _ := Allocated(f.Name())
		// Every other unit, aligned to the unit.
		for off := int64(0); off < 8<<20; off += 2 * unit {
			if err := f.Zero(off, unit); err != nil {
				t.Fatal(err)
			}
		}
		after, _ := Allocated(f.Name())
		t.Logf("zeroing every other %3d KiB of 8 MiB: %4d KiB given back of %d", unit>>10, (before-after)>>10, 4<<10)
	}
}

func TestExclusive(t *testing.T) {
	f := create(t, 1<<20, Exclusive)
	start := time.Now()
	if _, err := Open(f.Name(), Exclusive); !errors.Is(err, ErrInUse) {
		t.Errorf("second exclusive open: %v, want ErrInUse", err)
	}
	if d := time.Since(start); d < sharingRetries*sharingDelay {
		t.Errorf("gave up after %v without retrying", d)
	}
	if _, err := Open(f.Name(), ReadOnly); !errors.Is(err, ErrInUse) {
		t.Errorf("read-only open of an exclusive file: %v, want ErrInUse", err)
	}
	if _, err := Allocated(f.Name()); err != nil {
		t.Errorf("Allocated while the file is open exclusively: %v", err)
	}
	if busy, err := InUse(f.Name()); err != nil || !busy {
		t.Errorf("InUse of a file open exclusively = %v, %v", busy, err)
	}
	if err := os.Remove(f.Name()); err == nil {
		t.Error("removed a file open exclusively")
	}
	f.Close()
	if busy, err := InUse(f.Name()); err != nil || busy {
		t.Errorf("InUse of a closed file = %v, %v", busy, err)
	}
	g, err := Open(f.Name(), Exclusive)
	if err != nil {
		t.Fatalf("reopening after Close: %v", err)
	}
	g.Close()
	if _, err := InUse(f.Name() + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("InUse of a missing file: %v", err)
	}
}

func TestReadOnlyShared(t *testing.T) {
	path := filepath.Join(testDir(t), "layer.ext4")
	if err := os.WriteFile(path, []byte("layer"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(path, ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, ReadOnly)
	if err != nil {
		t.Fatalf("second read-only open: %v", err)
	}
	defer b.Close()
	if _, err := b.WriteAt([]byte("x"), 0); err == nil {
		t.Error("wrote through a read-only file")
	}
	if _, err := Open(path, Exclusive); !errors.Is(err, ErrInUse) {
		t.Errorf("exclusive open of a shared file: %v, want ErrInUse", err)
	}
	if _, err := Open(filepath.Join(filepath.Dir(path), "missing"), ReadOnly); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("opening a missing file: %v, want ErrNotExist", err)
	}
}

func TestClosed(t *testing.T) {
	f := create(t, 1<<20, Exclusive)
	f.Close()
	if _, err := f.ReadAt(make([]byte, 1), 0); err != ErrClosed {
		t.Errorf("ReadAt after Close: %v", err)
	}
	if err := f.Close(); err != ErrClosed {
		t.Errorf("second Close: %v", err)
	}
}

func TestScratchDeletedOnClose(t *testing.T) {
	f := create(t, 1<<30, Scratch)
	if _, err := os.Stat(f.Name()); err != nil {
		t.Fatalf("scratch file missing while open: %v", err)
	}
	if err := os.Remove(f.Name()); err == nil {
		t.Error("removed a scratch file in use")
	}
	f.Close()
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("scratch file still there after Close: %v", err)
	}
}

// TestScratchDeletedWhenKilled kills a process holding a scratch file, as
// Task Manager or a crash would, and expects Windows to delete the file.
func TestScratchDeletedWhenKilled(t *testing.T) {
	if path := os.Getenv("RUNWHP_DISK_HOLD"); path != "" {
		if _, err := Create(path, 1<<30, Scratch); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("holding")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	path := filepath.Join(testDir(t), "scratch.ext4")
	cmd := exec.Command(os.Args[0], "-test.run=^TestScratchDeletedWhenKilled$")
	cmd.Env = append(os.Environ(), "RUNWHP_DISK_HOLD="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, 64)
	n, _ := out.Read(line)
	if !bytes.HasPrefix(line[:n], []byte("holding")) {
		cmd.Process.Kill()
		t.Fatalf("child said %q", line[:n])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scratch file missing while the child holds it: %v", err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	waitGone := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(waitGone) {
			t.Fatal("scratch file still there after its process was killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The benchmarks compare a File with os.File, whose ReadAt and WriteAt run
// one at a time, for random 4 KiB and sequential 1 MiB requests from 1, 4
// and 16 goroutines. The data is in the system cache, as most of a disk's
// reads are, so they measure what the host adds per request.

type readerWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

func benchFiles(b *testing.B, size int64) map[string]readerWriterAt {
	f := create(b, size, Exclusive)
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	for off := int64(0); off < size; off += int64(len(data)) {
		if _, err := f.WriteAt(data, off); err != nil {
			b.Fatal(err)
		}
	}
	f.Close()
	g, err := Open(f.Name(), ReadOnly)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { g.Close() })
	o, err := os.Open(f.Name())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { o.Close() })
	return map[string]readerWriterAt{"disk": g, "osfile": o}
}

func BenchmarkRead(b *testing.B) {
	const size = 256 << 20
	files := benchFiles(b, size)
	for _, kind := range []struct {
		name string
		bs   int
		rnd  bool
	}{{"rand4k", 4 << 10, true}, {"seq1m", 1 << 20, false}} {
		for _, name := range []string{"disk", "osfile"} {
			for _, g := range []int{1, 4, 16} {
				b.Run(fmt.Sprintf("%s/%s/g%d", kind.name, name, g), func(b *testing.B) {
					f := files[name]
					b.SetBytes(int64(kind.bs))
					var next atomic.Int64
					var wg sync.WaitGroup
					b.ResetTimer()
					for w := range g {
						wg.Add(1)
						go func() {
							defer wg.Done()
							buf := make([]byte, kind.bs)
							r := rand.New(rand.NewPCG(uint64(w), 1))
							for {
								i := next.Add(1) - 1
								if i >= int64(b.N) {
									return
								}
								off := i * int64(kind.bs) % size
								if kind.rnd {
									off = r.Int64N(size/int64(kind.bs)) * int64(kind.bs)
								}
								if _, err := f.ReadAt(buf, off); err != nil {
									b.Error(err)
									return
								}
							}
						}()
					}
					wg.Wait()
				})
			}
		}
	}
}

func BenchmarkWriteSparse(b *testing.B) {
	const size = 1 << 30
	for _, temp := range []Mode{Exclusive, Scratch} {
		for _, g := range []int{1, 4, 16} {
			name := map[Mode]string{Exclusive: "plain", Scratch: "temporary"}[temp]
			b.Run(fmt.Sprintf("rand4k/%s/g%d", name, g), func(b *testing.B) {
				f := create(b, size, temp)
				b.SetBytes(4 << 10)
				var next atomic.Int64
				var wg sync.WaitGroup
				b.ResetTimer()
				for w := range g {
					wg.Add(1)
					go func() {
						defer wg.Done()
						buf := bytes.Repeat([]byte{byte(w + 1)}, 4<<10)
						r := rand.New(rand.NewPCG(uint64(w), 2))
						for next.Add(1) <= int64(b.N) {
							if _, err := f.WriteAt(buf, r.Int64N(size/4096)*4096); err != nil {
								b.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()
			})
		}
	}
}
