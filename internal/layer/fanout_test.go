package layer

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
)

func TestFanout(t *testing.T) {
	src := bytes.Repeat([]byte("0123456789abcdef"), 3<<20/16+7) // three chunks and a bit
	got := make([][]byte, 3)
	var consumers []func(io.Reader) error
	for i := range got {
		consumers = append(consumers, func(r io.Reader) error {
			// Readers that read in different sizes still see every byte.
			buf := make([]byte, 1+i*4096)
			for {
				n, err := r.Read(buf)
				got[i] = append(got[i], buf[:n]...)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
		})
	}
	if err := fanout(bytes.NewReader(src), consumers...); err != nil {
		t.Fatal(err)
	}
	for i, g := range got {
		if !bytes.Equal(g, src) {
			t.Errorf("consumer %d read %d bytes, not the %d sent", i, len(g), len(src))
		}
	}
}

// TestFanoutError stops every consumer when one fails, and reports that
// consumer's error, not the others' being stopped.
func TestFanoutError(t *testing.T) {
	boom := errors.New("boom")
	done := make(chan error, 1)
	go func() {
		done <- fanout(strings.NewReader(strings.Repeat("x", 64<<20)),
			func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
			func(r io.Reader) error { r.Read(make([]byte, 10)); return boom },
			func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
		)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Errorf("fanout = %v, want boom", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fanout hung after a consumer failed")
	}
	// A source that fails gives its error.
	err := fanout(io.MultiReader(strings.NewReader("abc"), iotestErr{boom}),
		func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	if !errors.Is(err, boom) {
		t.Errorf("fanout of a failing source = %v", err)
	}
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }

// TestDisassembleAsTarSplit checks that disassemble records what
// tar-split's own disassembler does, entry for entry.
func TestDisassembleAsTarSplit(t *testing.T) {
	b, _ := oddTar(t)
	var ours, theirs bytes.Buffer
	if err := disassemble(bytes.NewReader(b), storage.NewJSONPacker(&ours)); err != nil {
		t.Fatal(err)
	}
	pr, done, _ := asm.NewInputTarStreamWithDone(bytes.NewReader(b), storage.NewJSONPacker(&theirs), nil)
	io.Copy(io.Discard, pr)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ours.String() != theirs.String() {
		t.Errorf("metadata differs from tar-split's:\nours   %.400s\ntheirs %.400s", ours.String(), theirs.String())
	}
}
