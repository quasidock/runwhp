package layer

import (
	"errors"
	"hash/crc64"
	"io"
	"sync"

	"github.com/vbatts/tar-split/archive/tar"
	"github.com/vbatts/tar-split/tar/storage"
)

// A layer is read once and used three ways: hashed, converted to ext4, and
// taken apart into tar metadata. Each takes about as long as the others,
// so they run at once, each on its own goroutine, fed the same chunks.

const (
	chunkSize  = 1 << 20
	chunksHeld = 8 // per consumer: at most 8 MiB waits for each
)

// fanout reads src in chunks and hands each chunk to every consumer, which
// reads them in order through an io.Reader. It returns the first error any
// consumer or src returned. A consumer that fails stops the others: they
// read errAborted.
func fanout(src io.Reader, consumers ...func(io.Reader) error) error {
	stop := make(chan struct{})
	var stopOnce sync.Once
	abort := func() { stopOnce.Do(func() { close(stop) }) }

	readers := make([]*chunkReader, len(consumers))
	errs := make([]error, len(consumers)+1)
	var wg sync.WaitGroup
	for i, consume := range consumers {
		r := &chunkReader{chunks: make(chan []byte, chunksHeld), stop: stop}
		readers[i] = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := consume(r); err != nil {
				errs[i] = err
				abort()
			}
		}()
	}

	// The producer: chunks are never written again once sent, so the
	// consumers can share them.
	func() {
		defer func() {
			for _, r := range readers {
				close(r.chunks)
			}
		}()
		for {
			buf := make([]byte, chunkSize)
			n, err := io.ReadFull(src, buf)
			if n > 0 {
				for _, r := range readers {
					select {
					case r.chunks <- buf[:n]:
					case <-stop:
						return
					}
				}
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			if err != nil {
				errs[len(consumers)] = err
				abort()
				return
			}
		}
	}()
	wg.Wait()
	// The source's error first: a consumer's is then only errAborted.
	if err := errs[len(consumers)]; err != nil {
		return err
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, errAborted) {
			return err
		}
	}
	return nil
}

var errAborted = errors.New("layer: reading stopped")

// chunkReader reads the chunks fanout sends it.
type chunkReader struct {
	chunks chan []byte
	stop   chan struct{}
	cur    []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		select {
		case c, ok := <-r.chunks:
			if !ok {
				return 0, io.EOF
			}
			r.cur = c
		case <-r.stop:
			return 0, errAborted
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// disassemble records a layer tar's metadata to p, as tar-split's
// asm.NewInputTarStream does, but reading r itself rather than through a
// pipe of its own: every byte but the files' contents, as segments, and
// each file's name, size and CRC64. It reads r to its end, the padding
// after the end-of-archive marker included. The loop follows tar-split's
// runInputTarStream (BSD-3-Clause, Vincent Batts).
func disassemble(r io.Reader, p storage.Packer) error {
	tr := tar.NewReader(r)
	tr.RawAccounting = true
	segment := func() error {
		if b := tr.RawBytes(); len(b) > 0 {
			_, err := p.AddEntry(storage.Entry{Type: storage.SegmentType, Payload: b})
			return err
		}
		return nil
	}
	buf := make([]byte, 32<<10)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// The end-of-archive marker.
			if err := segment(); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return err
		}
		if err := segment(); err != nil {
			return err
		}
		var sum []byte
		if hdr.Size > 0 {
			c := crc64.New(storage.CRCTable)
			if _, err := io.CopyBuffer(c, tr, buf); err != nil {
				return err
			}
			sum = c.Sum(nil)
		}
		entry := storage.Entry{Type: storage.FileType, Size: hdr.Size, Payload: sum}
		entry.SetName(hdr.Name) // keeps names that are not UTF-8
		if _, err := p.AddEntry(entry); err != nil {
			return err
		}
		if err := segment(); err != nil {
			return err
		}
	}
	// Whatever follows the marker: more zeros, often.
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, err := p.AddEntry(storage.Entry{Type: storage.SegmentType, Payload: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
