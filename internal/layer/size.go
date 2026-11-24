package layer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"

	"github.com/vbatts/tar-split/tar/storage"
)

// DiffSize is the size Docker gives a layer: what its files take once
// unpacked, regular files' sizes and symlinks' targets' lengths, as
// Docker sums them walking the layer's directory. It reads only the tar's
// headers, from its metadata, and gives ErrNoSplit for a layer stored
// before runwhp kept that.
func (s *Store) DiffSize(diffID string) (int64, error) {
	r, err := s.Split(diffID)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	hs, err := newHeaderStream(storage.NewJSONUnpacker(r))
	if err != nil {
		return 0, fmt.Errorf("layer %s's tar metadata: %w", diffID, err)
	}
	tr := tar.NewReader(hs)
	var n int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, fmt.Errorf("layer %s's tar metadata: %w", diffID, err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			n += hdr.Size
		case tar.TypeSymlink:
			n += int64(len(hdr.Linkname))
		}
	}
}

// headerStream is a layer's tar with every file's contents zeros: the
// metadata's segments, the headers among them, and runs of zeros where
// the contents were. tar.Reader seeks over the runs rather than read them.
type headerStream struct {
	parts []part
	i     int   // the part being read
	off   int64 // into it
}

type part struct {
	data  []byte
	zeros int64
}

func (p part) size() int64 { return int64(len(p.data)) + p.zeros }

func newHeaderStream(up storage.Unpacker) (*headerStream, error) {
	hs := &headerStream{}
	for {
		e, err := up.Next()
		if err == io.EOF {
			return hs, nil
		}
		if err != nil {
			return nil, err
		}
		switch e.Type {
		case storage.SegmentType:
			hs.parts = append(hs.parts, part{data: e.Payload})
		case storage.FileType:
			if e.Size > 0 {
				hs.parts = append(hs.parts, part{zeros: e.Size})
			}
		}
	}
}

func (h *headerStream) Read(p []byte) (int, error) {
	for h.i < len(h.parts) && h.off >= h.parts[h.i].size() {
		h.i, h.off = h.i+1, 0
	}
	if h.i == len(h.parts) {
		return 0, io.EOF
	}
	cur := h.parts[h.i]
	var n int
	if cur.data != nil {
		n = copy(p, cur.data[h.off:])
	} else {
		n = int(min(int64(len(p)), cur.zeros-h.off))
		clear(p[:n])
	}
	h.off += int64(n)
	return n, nil
}

// Seek moves forward only, as tar.Reader does to skip contents.
func (h *headerStream) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekCurrent || offset < 0 {
		return 0, errors.New("headerStream: only forward seeks")
	}
	for offset > 0 && h.i < len(h.parts) {
		left := h.parts[h.i].size() - h.off
		if offset < left {
			h.off += offset
			offset = 0
			break
		}
		offset -= left
		h.i, h.off = h.i+1, 0
	}
	return h.pos(), nil
}

func (h *headerStream) pos() int64 {
	var n int64
	for _, p := range h.parts[:h.i] {
		n += p.size()
	}
	return n + h.off
}
