package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/runwhp/internal/storefile"
)

// The store keeps each image's configuration blob as the registry or the
// archive gave it, byte for byte, in blobs/sha256/<hex>: a record holds the
// configuration re-indented, and an image's ID is the digest of the blob's
// own bytes. Records written before iteration 14 have none, and `save`
// fetches it along with their layers' tar metadata.

// ErrNoBlob means a blob is not in the store.
var ErrNoBlob = errors.New("blob not in the store")

func (s *Store) blobPath(digest string) (string, error) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(h) != 64 {
		return "", fmt.Errorf("unsupported blob digest %q", digest)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("unsupported blob digest %q", digest)
	}
	return filepath.Join(s.Root, "blobs", "sha256", h), nil
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PutBlob stores data, whose digest must be digest.
func (s *Store) PutBlob(digest string, data []byte) error {
	p, err := s.blobPath(digest)
	if err != nil {
		return err
	}
	if got := digestOf(data); got != digest {
		return fmt.Errorf("blob %s has digest %s", digest, got)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return storefile.Replace(p, data)
}

// Blob reads a blob, checking its digest. It gives ErrNoBlob for one not in
// the store.
func (s *Store) Blob(digest string) ([]byte, error) {
	p, err := s.blobPath(digest)
	if err != nil {
		return nil, err
	}
	data, err := storefile.Read(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoBlob, digest)
	} else if err != nil {
		return nil, err
	}
	if got := digestOf(data); got != digest {
		return nil, fmt.Errorf("blob %s in the store is damaged: its digest is %s", digest, got)
	}
	return data, nil
}

// Blobs lists the digests of the blobs in the store.
func (s *Store) Blobs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "blobs", "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		d := "sha256:" + e.Name()
		if _, err := s.blobPath(d); err == nil && !e.IsDir() {
			out = append(out, d)
		}
	}
	return out, nil
}

// BlobSize is the space a blob takes, or 0 if it is not there.
func (s *Store) BlobSize(digest string) int64 {
	p, err := s.blobPath(digest)
	if err != nil {
		return 0
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.Size()
}

// RemoveBlob removes a blob. One that is not there is not an error.
func (s *Store) RemoveBlob(digest string) error {
	p, err := s.blobPath(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RawConfig returns img's configuration blob, as its ID names it. It gives
// ErrNoBlob for a record written before the store kept it.
func (s *Store) RawConfig(img *Image) ([]byte, error) {
	if img.ConfigDigest == "" {
		return nil, fmt.Errorf("%w: image %s's configuration", ErrNoBlob, Familiar(img.Ref))
	}
	return s.Blob(img.ConfigDigest)
}
