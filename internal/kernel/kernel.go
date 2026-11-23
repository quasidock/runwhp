// Package kernel publishes runwhp's guest kernel as an OCI artifact and
// fetches the build runwhp is pinned to into the local store.
//
// The kernel itself is built by kernel/Dockerfile; "Kernel" in README.md
// describes the whole arrangement.
package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/quasidock/runwhp/internal/boot"
)

// maxBzImage bounds what a registry can make runwhp download and keep.
const maxBzImage = 64 << 20

// ErrNotPresent means the pinned kernel is not in the store and fetching it
// was not allowed.
var ErrNotPresent = errors.New("kernel not present locally")

// Store keeps fetched kernels under Root, next to images and layers.
type Store struct {
	Root string // holds kernels/ and tmp/
	// Progress receives one line per fetch step. nil discards them.
	Progress io.Writer
	// Remote options, for tests.
	RemoteOptions []remote.Option
}

// Open returns the store under root, creating nothing until it is written.
func Open(root string) *Store { return &Store{Root: root} }

// Path is where the pinned build's bzImage is kept.
func (s *Store) Path(p Pin) (string, error) {
	h, err := v1.NewHash(p.Digest)
	if err != nil || h.Algorithm != "sha256" {
		return "", fmt.Errorf("kernel pin %s: unsupported digest %q", p.Tag, p.Digest)
	}
	return filepath.Join(s.Root, "kernels", "sha256", h.Hex, "bzImage"), nil
}

// Ensure returns the path of the pinned kernel, fetching it first if it is
// not in the store and allowFetch is true. A stored kernel is never fetched
// again: its digest cannot change.
func (s *Store) Ensure(ctx context.Context, p Pin, allowFetch bool) (string, error) {
	ref, err := p.Reference()
	if err != nil {
		return "", err
	}
	dst, err := s.Path(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if !allowFetch {
		return "", fmt.Errorf("runwhp's kernel %s: %w", p, ErrNotPresent)
	}

	s.progressf("pulling kernel %s (%s)", p, p.Repository)
	opts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous)}, s.RemoteOptions...)
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("pulling kernel %s: %w", p, err)
	}
	if sum := sha256.Sum256(desc.Manifest); "sha256:"+hex.EncodeToString(sum[:]) != p.Digest {
		return "", fmt.Errorf("pulling kernel %s: manifest does not match its digest", p)
	}
	layer, err := bzImageLayer(desc.Manifest)
	if err != nil {
		return "", fmt.Errorf("pulling kernel %s: %w", p, err)
	}
	s.progressf("  %.19s  fetching %.1f MB", layer.Digest, float64(layer.Size)/1e6)
	blob, err := remote.Layer(ref.Context().Digest(layer.Digest.String()), opts...)
	if err != nil {
		return "", fmt.Errorf("pulling kernel %s: %w", p, err)
	}
	if err := s.store(blob, layer, dst); err != nil {
		return "", fmt.Errorf("pulling kernel %s: %w", p, err)
	}
	s.progressf("pulled kernel %s (%s)", p, p.Digest)
	return dst, nil
}

// bzImageLayer checks that a manifest is runwhp's kernel artifact and
// returns the descriptor of its bzImage.
func bzImageLayer(manifest []byte) (v1.Descriptor, error) {
	var m v1.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return v1.Descriptor{}, fmt.Errorf("bad manifest: %w", err)
	}
	if m.ArtifactType != ArtifactType {
		return v1.Descriptor{}, fmt.Errorf("not a runwhp kernel (artifact type %q)", m.ArtifactType)
	}
	var found []v1.Descriptor
	for _, l := range m.Layers {
		if l.MediaType == MediaBzImage {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		return v1.Descriptor{}, fmt.Errorf("artifact has %d bzImage layers, not one", len(found))
	}
	l := found[0]
	if l.Size <= 0 || l.Size > maxBzImage || l.Digest.Algorithm != "sha256" {
		return v1.Descriptor{}, fmt.Errorf("unacceptable bzImage layer (%d bytes, %s)", l.Size, l.Digest)
	}
	return l, nil
}

// store downloads a bzImage, checks it, and moves it into place.
func (s *Store) store(blob v1.Layer, want v1.Descriptor, dst string) error {
	rc, err := blob.Compressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, want.Size+1))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != want.Size || hex.EncodeToString(sum[:]) != want.Digest.Hex {
		return fmt.Errorf("bzImage does not match its digest %s", want.Digest)
	}
	if _, err := boot.ParseKernel(data); err != nil {
		return fmt.Errorf("bzImage: %w", err)
	}

	tmpDir := filepath.Join(s.Root, "tmp")
	for _, dir := range []string{tmpDir, filepath.Dir(dst)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(tmpDir, "kernel-"+want.Digest.Hex[:12]+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		// Another runwhp may have stored the same kernel meanwhile, and a
		// starting guest may hold it open.
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) progressf(format string, a ...any) {
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, format+"\n", a...)
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// List returns the digests of the kernels in the store, as "sha256:HEX".
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "kernels", "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if h, err := v1.NewHash("sha256:" + e.Name()); err == nil && e.IsDir() {
			out = append(out, h.String())
		}
	}
	return out, nil
}

// Dir is the directory that holds the kernel with the given digest.
func (s *Store) Dir(digest string) (string, error) {
	h, err := v1.NewHash(digest)
	if err != nil || h.Algorithm != "sha256" {
		return "", fmt.Errorf("unsupported kernel digest %q", digest)
	}
	return filepath.Join(s.Root, "kernels", "sha256", h.Hex), nil
}

// Remove removes a kernel from the store. A runwhp booting it reads it
// whole first, and does not hold it open.
func (s *Store) Remove(digest string) error {
	dir, err := s.Dir(digest)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
