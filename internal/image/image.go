// Package image pulls OCI images from registries and keeps a local record of
// each image: its manifest digest, its configuration, and its layers as ext4
// images in a layer.Store.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/quasidock/runwhp/internal/layer"
)

// Platform is the only platform runwhp runs.
var Platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Store is the local image cache.
type Store struct {
	Root   string // holds images/ and layers/
	Layers *layer.Store
	// Progress receives one line per pull step. nil discards them.
	Progress io.Writer
	// Remote options, for tests.
	RemoteOptions []remote.Option
}

// Open returns the store under root, creating nothing until it is written.
func Open(root string) *Store {
	return &Store{
		Root:   root,
		Layers: &layer.Store{Dir: filepath.Join(root, "layers"), Tmp: filepath.Join(root, "tmp")},
	}
}

// Image is a pulled image.
type Image struct {
	Ref    string          `json:"ref"`    // normalized reference it was pulled by
	Digest string          `json:"digest"` // manifest digest
	Config json.RawMessage `json:"config"` // the image configuration blob
}

// ConfigFile parses the image configuration.
func (img *Image) ConfigFile() (*v1.ConfigFile, error) {
	var cf v1.ConfigFile
	if err := json.Unmarshal(img.Config, &cf); err != nil {
		return nil, fmt.Errorf("image %s: bad configuration: %w", img.Ref, err)
	}
	return &cf, nil
}

// Layers returns the diffIDs of the layers that hold files, bottom first.
func (img *Image) Layers() ([]string, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range cf.RootFS.DiffIDs {
		if s := h.String(); s != layer.EmptyDiffID {
			out = append(out, s)
		}
	}
	return out, nil
}

// Policy says when Ensure contacts the registry.
type Policy string

const (
	PullMissing Policy = "missing"
	PullAlways  Policy = "always"
	PullNever   Policy = "never"
)

func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(s); p {
	case PullMissing, PullAlways, PullNever:
		return p, nil
	}
	return "", fmt.Errorf("pull policy must be missing, always or never, not %q", s)
}

// ErrNotPresent means the image is not in the local store.
var ErrNotPresent = errors.New("image not present locally")

func normalize(ref string) (name.Reference, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	return r, nil
}

func (s *Store) recordPath(ref name.Reference) string {
	sum := sha256.Sum256([]byte(ref.Name()))
	return filepath.Join(s.Root, "images", hex.EncodeToString(sum[:])+".json")
}

func (s *Store) progressf(format string, a ...any) {
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, format+"\n", a...)
	}
}

// Get returns the local record of ref if it and all its layers are present.
func (s *Store) Get(ref string) (*Image, error) {
	r, err := normalize(ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.recordPath(r))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPresent
	} else if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, fmt.Errorf("image record for %s: %w", r.Name(), err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	for _, d := range layers {
		if !s.Layers.Has(d) {
			return nil, ErrNotPresent
		}
	}
	return &img, nil
}

// Ensure returns the image, pulling it as the policy allows.
func (s *Store) Ensure(ctx context.Context, ref string, policy Policy) (*Image, error) {
	if policy != PullAlways {
		img, err := s.Get(ref)
		if err == nil || !errors.Is(err, ErrNotPresent) {
			return img, err
		}
		if policy == PullNever {
			return nil, fmt.Errorf("image %s is not present locally and --pull=never was given", ref)
		}
	}
	return s.Pull(ctx, ref)
}

// Pull fetches the linux/amd64 image for ref, converts any layers not yet in
// the layer store, and records the image.
func (s *Store) Pull(ctx context.Context, ref string) (*Image, error) {
	r, err := normalize(ref)
	if err != nil {
		return nil, err
	}
	opts := append([]remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(Keychain),
		remote.WithPlatform(Platform),
	}, s.RemoteOptions...)
	s.progressf("pulling %s", r.Name())
	remoteImg, err := remote.Image(r, opts...)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", r.Name(), err)
	}
	digest, err := remoteImg.Digest()
	if err != nil {
		return nil, err
	}
	raw, err := remoteImg.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("pulling %s: configuration: %w", r.Name(), err)
	}
	img := &Image{Ref: r.Name(), Digest: digest.String(), Config: raw}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	if cf.OS != Platform.OS || cf.Architecture != Platform.Architecture {
		return nil, fmt.Errorf("image %s is for %s/%s; runwhp runs only %s/%s",
			r.Name(), cf.OS, cf.Architecture, Platform.OS, Platform.Architecture)
	}

	layers, err := remoteImg.Layers()
	if err != nil {
		return nil, err
	}
	if len(layers) != len(cf.RootFS.DiffIDs) {
		return nil, fmt.Errorf("image %s lists %d layers but its configuration has %d diffIDs",
			r.Name(), len(layers), len(cf.RootFS.DiffIDs))
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, 3)
	)
	for i, l := range layers {
		diffID := cf.RootFS.DiffIDs[i].String()
		if diffID == layer.EmptyDiffID || s.Layers.Has(diffID) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s.fetchLayer(ctx, l, diffID); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	if err := s.writeRecord(r, img); err != nil {
		return nil, err
	}
	s.progressf("pulled %s (%s)", r.Name(), img.Digest)
	return img, nil
}

func (s *Store) fetchLayer(ctx context.Context, l v1.Layer, diffID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	size, _ := l.Size()
	s.progressf("  %.19s  fetching %.1f MB and converting to ext4", diffID, float64(size)/1e6)
	rc, err := l.Uncompressed()
	if err != nil {
		return fmt.Errorf("layer %s: %w", diffID, err)
	}
	defer rc.Close()
	if _, err := s.Layers.Put(diffID, rc); err != nil {
		return err
	}
	return nil
}

func (s *Store) writeRecord(r name.Reference, img *Image) error {
	data, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	path := s.recordPath(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
