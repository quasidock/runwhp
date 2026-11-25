// Package image pulls OCI images from registries and keeps a local record of
// each image: its manifest digest, its configuration, and its layers as ext4
// images in a layer.Store.
package image

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
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/storefile"
)

// Platform is the only platform runwhp runs.
var Platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Store is the local image cache.
type Store struct {
	Root   string // holds images/ and layers/
	Layers *layer.Store
	// Progress receives one line per pull step. nil discards them. Layers
	// are fetched concurrently; the store serializes its writes.
	Progress io.Writer
	// Remote options, for tests.
	RemoteOptions []remote.Option

	progressMu sync.Mutex
}

// Open returns the store under root, creating nothing until it is written.
func Open(root string) *Store {
	return &Store{
		Root:   root,
		Layers: &layer.Store{Dir: filepath.Join(root, "layers"), Tmp: filepath.Join(root, "tmp")},
	}
}

// Image is what an image record holds: an image pulled, loaded, imported
// or tagged.
type Image struct {
	// Ref is the normalized reference the image was pulled, loaded, imported
	// or tagged as, or sha256:<id> for an image loaded or imported without
	// one; see Untagged.
	Ref string `json:"ref"`
	// Digest is the registry manifest's digest, or empty for an image that
	// did not come from a registry by this reference: one loaded, imported
	// or tagged.
	Digest string `json:"digest"`
	// ConfigDigest is the configuration blob's digest, as the manifest names
	// it: the image's ID, as Docker shows it. Records written before
	// iteration 12 lack it.
	ConfigDigest string `json:"configDigest,omitempty"`
	// RepoDigests are the manifests runwhp push wrote of the image, as
	// REPOSITORY@DIGEST, besides Digest.
	RepoDigests []string `json:"repoDigests,omitempty"`
	// Config is the image configuration. The record holds it re-indented,
	// so its bytes are not the blob's.
	Config json.RawMessage `json:"config"`
}

// ID is the image's ID: its configuration's digest. A record without one
// has it worked out from the configuration it holds, compacted, which gives
// the blob's bytes back for every configuration written compactly with
// nothing JSON escapes for HTML, as registries' images are.
func (img *Image) ID() string {
	if img.ConfigDigest != "" {
		return img.ConfigDigest
	}
	var b bytes.Buffer
	if json.Compact(&b, img.Config) != nil {
		b.Reset()
		b.Write(img.Config)
	}
	sum := sha256.Sum256(b.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Created is when the image was built, as its configuration says, or zero.
func (img *Image) Created() time.Time {
	cf, err := img.ConfigFile()
	if err != nil {
		return time.Time{}
	}
	return cf.Created.Time
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

func (s *Store) recordPath(ref name.Reference) string { return s.recordAt(ref.Name()) }

func (s *Store) recordAt(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(s.Root, "images", hex.EncodeToString(sum[:])+".json")
}

// recordFile is the file of the record of ref: a reference, normalized
// here, or an untagged image's sha256:<id>.
func (s *Store) recordFile(ref string) (string, error) {
	if Untagged(ref) {
		return s.recordAt(ref), nil
	}
	r, err := normalize(ref)
	if err != nil {
		return "", err
	}
	return s.recordPath(r), nil
}

func (s *Store) progressf(format string, a ...any) {
	if s.Progress == nil {
		return
	}
	line := fmt.Sprintf(format+"\n", a...)
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	io.WriteString(s.Progress, line)
}

// Get returns the local record of ref if it and all its layers are present.
func (s *Store) Get(ref string) (*Image, error) {
	path, err := s.recordFile(ref)
	if err != nil {
		return nil, err
	}
	data, err := storefile.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPresent
	} else if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, fmt.Errorf("image record for %s: %w", ref, err)
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
		// An image ID, or a prefix of one, as `docker run` takes it.
		if recs, err := s.Find(ref); err == nil && recs[0].Complete {
			return recs[0].Image, nil
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
	configDigest, err := remoteImg.ConfigName()
	if err != nil {
		return nil, fmt.Errorf("pulling %s: configuration: %w", r.Name(), err)
	}
	img := &Image{Ref: r.Name(), Digest: digest.String(), ConfigDigest: configDigest.String(), Config: raw}
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

	if err := s.PutBlob(img.ConfigDigest, raw); err != nil {
		return nil, fmt.Errorf("pulling %s: configuration: %w", r.Name(), err)
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
	return s.writeRecordAt(s.recordPath(r), img)
}

func (s *Store) writeRecordAt(path string, img *Image) error {
	data, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Runs pulling the same image at once each write a record of their
	// own, and others may be reading the one they replace.
	return storefile.Replace(path, data)
}

// Normalize is ref's full name, as records are named: docker.io/library
// and :latest made explicit.
func Normalize(ref string) (string, error) {
	r, err := normalize(ref)
	if err != nil {
		return "", err
	}
	return r.Name(), nil
}
