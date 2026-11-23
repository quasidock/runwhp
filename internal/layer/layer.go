// Package layer converts OCI image layers to ext4 images and keeps them in a
// cache keyed by diffID (the digest of the uncompressed layer tar). A cached
// file is never modified once it is in place, so any number of guests can
// attach it read-only at the same time. Files are removed only once no image
// record or container lists them, which package gc works out.
package layer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
)

// EmptyDiffID is the digest of a tar holding nothing but its end marker,
// which Dockerfile instructions such as ENV used to produce. Such layers add
// nothing to the root filesystem and are not attached.
const EmptyDiffID = "sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef"

// Store is a directory of ext4 layer images.
type Store struct {
	Dir string // holds sha256/<hex>.ext4
	Tmp string // scratch space on the same volume, for atomic renames
}

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

// Path is where the ext4 image of a layer is kept.
func (s *Store) Path(diffID string) (string, error) {
	h, err := parseDiffID(diffID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, "sha256", h+".ext4"), nil
}

// Has reports whether the layer is already converted.
func (s *Store) Has(diffID string) bool {
	p, err := s.Path(diffID)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Put converts an uncompressed layer tar to ext4 and stores it. The tar's
// digest must match diffID. OCI whiteouts become overlayfs whiteouts.
func (s *Store) Put(diffID string, tarStream io.Reader) (string, error) {
	want, err := parseDiffID(diffID)
	if err != nil {
		return "", err
	}
	dst, _ := s.Path(diffID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.Tmp, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Tmp, want[:12]+"-*.ext4")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	digest := sha256.New()
	tee := io.TeeReader(tarStream, digest)
	if err := tar2ext4.Convert(tee, tmp, tar2ext4.ConvertWhiteout); err != nil {
		return "", fmt.Errorf("converting layer %s: %w", diffID, err)
	}
	// tar2ext4 stops at the end-of-archive marker; hash any padding after it.
	if _, err := io.Copy(digest, tarStream); err != nil {
		return "", fmt.Errorf("reading layer %s: %w", diffID, err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		return "", fmt.Errorf("layer %s: content has digest sha256:%s", diffID, got)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		// Another runwhp may have stored the same layer meanwhile, and a
		// running guest may hold it open.
		if _, statErr := os.Stat(dst); statErr == nil {
			return dst, nil
		}
		return "", err
	}
	return dst, nil
}

// ErrInUse means a layer could not be removed because a guest has it open.
var ErrInUse = errors.New("layer is in use")

// List returns the diffIDs of the layers in the store.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		h, ok := strings.CutSuffix(e.Name(), ".ext4")
		if !ok || e.IsDir() {
			continue
		}
		if _, err := parseDiffID("sha256:" + h); err == nil {
			out = append(out, "sha256:"+h)
		}
	}
	return out, nil
}

// Remove removes a layer's file. A layer a running guest has open cannot be
// removed, since guests open layers without sharing delete, and gives
// ErrInUse. A layer that is not there is not an error.
func (s *Store) Remove(diffID string) error {
	p, err := s.Path(diffID)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
		return nil
	case inUse(err):
		return fmt.Errorf("%w: %s", ErrInUse, diffID)
	}
	return err
}
