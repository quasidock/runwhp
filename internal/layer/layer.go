// Package layer converts OCI image layers to ext4 images and keeps them in a
// cache keyed by diffID (the digest of the uncompressed layer tar). A cached
// file is never modified once it is in place, so any number of guests can
// attach it read-only at the same time. Files are removed only once no image
// record or container lists them, which package gc works out.
//
// Beside each ext4 image the store keeps the layer tar's own metadata, in
// tar-split's format, as Docker does: every byte of the tar but the files'
// contents, which the ext4 image holds. The two together give back the tar
// byte for byte, and so a layer runwhp saves has the diffID it was pulled
// with. The metadata is a few tenths of a percent of the tar, gzipped as
// <hex>.tar-split.gz. Layers stored before iteration 14 have none.
package layer

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
	"github.com/quasidock/runwhp/internal/storefile"
	"github.com/vbatts/tar-split/tar/storage"
)

// EmptyDiffID is the digest of a tar holding nothing but its end marker,
// which Dockerfile instructions such as ENV used to produce. Such layers add
// nothing to the root filesystem and are not attached.
const EmptyDiffID = "sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef"

// EmptyTar is the layer EmptyDiffID names: 1024 zero bytes.
var EmptyTar = make([]byte, 1024)

const (
	ext4Suffix  = ".ext4"
	splitSuffix = ".tar-split.gz"
)

// Store is a directory of ext4 layer images.
type Store struct {
	Dir string // holds sha256/<hex>.ext4 and sha256/<hex>.tar-split.gz
	Tmp string // scratch space on the same volume, for atomic renames
}

// ErrNoSplit means a layer's tar metadata is not in the store: the layer
// was stored before runwhp kept it.
var ErrNoSplit = errors.New("the layer's tar metadata is not in the store")

func parseDiffID(diffID string) (string, error) {
	hexPart, ok := strings.CutPrefix(diffID, "sha256:")
	if !ok || len(hexPart) != 64 {
		return "", fmt.Errorf("unsupported layer digest %q", diffID)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("unsupported layer digest %q", diffID)
	}
	return hexPart, nil
}

func (s *Store) file(diffID, suffix string) (string, error) {
	h, err := parseDiffID(diffID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, "sha256", h+suffix), nil
}

// Path is where the ext4 image of a layer is kept.
func (s *Store) Path(diffID string) (string, error) { return s.file(diffID, ext4Suffix) }

// SplitPath is where a layer's tar metadata is kept.
func (s *Store) SplitPath(diffID string) (string, error) { return s.file(diffID, splitSuffix) }

// Files are the paths of a layer's files, whether they exist or not.
func (s *Store) Files(diffID string) []string {
	p, err := s.Path(diffID)
	if err != nil {
		return nil
	}
	sp, _ := s.SplitPath(diffID)
	return []string{p, sp}
}

func exists(p string, err error) bool {
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Has reports whether the layer is already converted.
func (s *Store) Has(diffID string) bool { return exists(s.Path(diffID)) }

// HasSplit reports whether the layer's tar metadata is in the store.
func (s *Store) HasSplit(diffID string) bool { return exists(s.SplitPath(diffID)) }

// Put converts an uncompressed layer tar to ext4 and stores it, with the
// tar's metadata. The tar's digest must match diffID. OCI whiteouts become
// overlayfs whiteouts. A layer already converted gets its metadata only.
func (s *Store) Put(diffID string, tarStream io.Reader) (string, error) {
	want, err := parseDiffID(diffID)
	if err != nil {
		return "", err
	}
	dst, _ := s.Path(diffID)
	if s.Has(diffID) {
		if !s.HasSplit(diffID) {
			if err := s.PutSplit(diffID, tarStream); err != nil {
				return "", err
			}
		}
		return dst, nil
	}
	_, path, err := s.put(want, tarStream, true)
	return path, err
}

// PutSplit stores only a layer's tar metadata, from the whole tar, whose
// digest must match diffID. It is for layers stored before runwhp kept it.
func (s *Store) PutSplit(diffID string, tarStream io.Reader) error {
	want, err := parseDiffID(diffID)
	if err != nil {
		return err
	}
	_, _, err = s.put(want, tarStream, false)
	return err
}

// Import stores a layer whose diffID is not known ahead, as `runwhp import`
// has, and returns its diffID and the ext4 image's path.
func (s *Store) Import(tarStream io.Reader) (string, string, error) {
	return s.put("", tarStream, true)
}

// put reads a layer tar, keeping its metadata and, if convert is set,
// converting it to ext4. want is the tar's sha256 in hex, or empty to take
// whatever it turns out to be. The metadata goes in place before the ext4
// image: a layer is there once its ext4 image is.
func (s *Store) put(want string, tarStream io.Reader, convert bool) (diffID, path string, err error) {
	if err := os.MkdirAll(filepath.Join(s.Dir, "sha256"), 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(s.Tmp, 0o755); err != nil {
		return "", "", err
	}
	prefix := "import"
	if want != "" {
		prefix = want[:12]
	}
	split, err := os.CreateTemp(s.Tmp, prefix+"-*"+splitSuffix)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(split.Name())
	defer split.Close()
	var ext4 *os.File
	if convert {
		ext4, err = os.CreateTemp(s.Tmp, prefix+"-*"+ext4Suffix)
		if err != nil {
			return "", "", err
		}
		defer os.Remove(ext4.Name())
		defer ext4.Close()
	}

	got, err := consume(tarStream, split, ext4)
	if err != nil {
		return "", "", err
	}
	if want != "" && got != want {
		return "", "", fmt.Errorf("layer sha256:%s: content has digest sha256:%s", want, got)
	}
	diffID = "sha256:" + got
	if err := split.Sync(); err != nil {
		return "", "", err
	}
	if err := split.Close(); err != nil {
		return "", "", err
	}
	splitDst, _ := s.SplitPath(diffID)
	if err := place(split.Name(), splitDst); err != nil {
		return "", "", err
	}
	if !convert {
		return diffID, "", nil
	}
	if err := ext4.Sync(); err != nil {
		return "", "", err
	}
	if err := ext4.Close(); err != nil {
		return "", "", err
	}
	dst, _ := s.Path(diffID)
	if err := place(ext4.Name(), dst); err != nil {
		return "", "", err
	}
	return diffID, dst, nil
}

// place renames a finished file into the store. Another runwhp may have
// stored the same file meanwhile, and a running guest may hold it open; the
// files are the same either way.
func place(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil {
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// consume reads a whole layer tar, writing its metadata, gzipped, to split
// and, if ext4 is not nil, the ext4 image to ext4. It returns the tar's
// sha256 in hex. Hashing, taking the tar apart and converting it run at
// once, on chunks fanout shares among them.
func consume(tarStream io.Reader, split io.Writer, ext4 *os.File) (string, error) {
	digest := sha256.New()
	consumers := []func(io.Reader) error{
		func(r io.Reader) error {
			_, err := io.Copy(digest, r)
			return err
		},
		func(r io.Reader) error {
			gz := gzip.NewWriter(split)
			if err := disassemble(r, storage.NewJSONPacker(gz)); err != nil {
				return fmt.Errorf("reading the layer's tar: %w", err)
			}
			return gz.Close()
		},
	}
	if ext4 != nil {
		consumers = append(consumers, func(r io.Reader) error {
			if err := tar2ext4.Convert(r, ext4, tar2ext4.ConvertWhiteout); err != nil {
				return fmt.Errorf("converting the layer: %w", err)
			}
			// tar2ext4 stops at the end-of-archive marker; take the rest.
			_, err := io.Copy(io.Discard, r)
			return err
		})
	}
	if err := fanout(tarStream, consumers...); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Split opens a layer's tar metadata, ungzipped, for tar-split's unpacker.
// It gives ErrNoSplit for a layer stored before runwhp kept it.
func (s *Store) Split(diffID string) (io.ReadCloser, error) {
	p, err := s.SplitPath(diffID)
	if err != nil {
		return nil, err
	}
	f, err := storefile.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoSplit, diffID)
	} else if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("layer %s's tar metadata: %w", diffID, err)
	}
	return &splitReader{zr, f}, nil
}

type splitReader struct {
	*gzip.Reader
	f *os.File
}

func (r *splitReader) Close() error {
	r.Reader.Close()
	return r.f.Close()
}

// ErrInUse means a layer could not be removed because a guest has it open.
var ErrInUse = errors.New("layer is in use")

// List returns the diffIDs of the layers in the store: those with an ext4
// image, a tar metadata file, or both.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		h, ok := strings.CutSuffix(e.Name(), ext4Suffix)
		if !ok {
			h, ok = strings.CutSuffix(e.Name(), splitSuffix)
		}
		if !ok || seen[h] {
			continue
		}
		if _, err := parseDiffID("sha256:" + h); err == nil {
			seen[h] = true
			out = append(out, "sha256:"+h)
		}
	}
	return out, nil
}

// Remove removes a layer's files. A layer a running guest has open cannot be
// removed, since guests open layers without sharing delete, and gives
// ErrInUse; its metadata stays with it. A layer that is not there is not an
// error.
func (s *Store) Remove(diffID string) error {
	p, err := s.Path(diffID)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
	case inUse(err):
		return fmt.Errorf("%w: %s", ErrInUse, diffID)
	default:
		return err
	}
	sp, _ := s.SplitPath(diffID)
	if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
