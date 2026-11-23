package storefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestReplaceWhileRead replaces a file a reader has open, and one a plain
// os.Open has open, which Windows refuses.
func TestReplaceWhileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	if err := Replace(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, []byte("two")); err != nil {
		t.Errorf("replacing a file open shared: %v", err)
	}
	buf := make([]byte, 8)
	if n, _ := f.Read(buf); string(buf[:n]) != "one" {
		t.Errorf("the reader read %q, want the contents it opened", buf[:n])
	}
	f.Close()
	if data, err := Read(path); err != nil || string(data) != "two" {
		t.Errorf("Read = %q, %v", data, err)
	}
	if _, err := Read(path + ".missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read of a missing file: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
}

// TestReplaceAtOnce replaces one file from many goroutines while others
// read it: every write succeeds and every read sees one whole write.
func TestReplaceAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	Replace(path, []byte(strings.Repeat("0", 4096)))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 20 {
				if err := Replace(path, []byte(strings.Repeat(fmt.Sprint((i+j)%10), 4096))); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				data, err := Read(path)
				if err != nil || len(data) != 4096 || strings.Count(string(data), string(data[:1])) != 4096 {
					t.Errorf("read %d bytes, %v", len(data), err)
				}
			}
		}()
	}
	wg.Wait()
}
