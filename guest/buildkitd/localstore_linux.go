//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/util/bklog"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/singleflight"
)

// localLease is how long an image taken from runwhp's store is kept in
// BuildKit's content store once imported, without a build using it.
const localLease = 48 * time.Hour

// localStore is runwhp's image store as BuildKit's worker sees images of
// its own, for the resolver to find a FROM in before it asks a registry,
// as Docker's embedded BuildKit finds Docker's images: buildx's docker
// driver asks it to, unless --pull. An image is imported into BuildKit's
// content store, through the API's save, the first time a build uses it.
// BuildKit names nothing here: images it builds go to runwhp's store
// through the moby exporter.
type localStore struct {
	api     *dockerAPI
	content content.Store
	leases  leases.Manager

	mu       sync.Mutex
	imported map[string]ocispecs.Descriptor // by image ID
	flight   singleflight.Group
}

func newLocalStore(api *dockerAPI, cs content.Store, lm leases.Manager) *localStore {
	return &localStore{api: api, content: cs, leases: lm, imported: map[string]ocispecs.Descriptor{}}
}

var _ images.Store = (*localStore)(nil)

// Platform is the only one runwhp's store holds.
var localPlatform = ocispecs.Platform{OS: "linux", Architecture: "amd64"}

func (s *localStore) Get(ctx context.Context, name string) (images.Image, error) {
	info, err := s.api.inspect(ctx, name)
	if errors.Is(err, errNoImage) {
		return images.Image{}, fmt.Errorf("image %q: %w", name, cerrdefs.ErrNotFound)
	}
	if err != nil {
		// The API being out of reach is no reason to fail a build that
		// a registry can serve.
		bklog.G(ctx).Warnf("looking for %s in runwhp's store: %v", name, err)
		return images.Image{}, fmt.Errorf("image %q: %w", name, cerrdefs.ErrNotFound)
	}
	if info.Os != localPlatform.OS || info.Architecture != localPlatform.Architecture {
		return images.Image{}, fmt.Errorf("image %q is %s/%s: %w", name, info.Os, info.Architecture, cerrdefs.ErrNotFound)
	}
	v, err, _ := s.flight.Do(info.ID, func() (any, error) { return s.importImage(ctx, name, info.ID) })
	if err != nil {
		return images.Image{}, err
	}
	return images.Image{Name: name, Target: v.(ocispecs.Descriptor)}, nil
}

// importImage brings an image of runwhp's store into the content store,
// unless it is there already, and returns its manifest's descriptor.
func (s *localStore) importImage(ctx context.Context, name, id string) (ocispecs.Descriptor, error) {
	s.mu.Lock()
	desc, ok := s.imported[id]
	s.mu.Unlock()
	// Once its lease has expired, and BuildKit's GC taken its content, the
	// image is imported again.
	if ok {
		if _, err := s.content.Info(ctx, desc.Digest); err == nil {
			return desc, nil
		}
	}
	ctx, err := s.lease(ctx, id)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	rc, err := s.api.save(ctx, name)
	if err != nil {
		return ocispecs.Descriptor{}, fmt.Errorf("reading %s from runwhp's store: %w", name, err)
	}
	defer rc.Close()
	index, err := archive.ImportIndex(ctx, s.content, rc)
	if err != nil {
		return ocispecs.Descriptor{}, fmt.Errorf("importing %s from runwhp's store: %w", name, err)
	}
	data, err := content.ReadBlob(ctx, s.content, index)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	var idx ocispecs.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return ocispecs.Descriptor{}, err
	}
	if len(idx.Manifests) == 0 {
		return ocispecs.Descriptor{}, fmt.Errorf("runwhp's store gave %s with no manifest", name)
	}
	desc = idx.Manifests[0]
	desc.Annotations = nil
	desc.Platform = &localPlatform
	s.mu.Lock()
	s.imported[id] = desc
	s.mu.Unlock()
	return desc, nil
}

func leaseID(imageID string) string {
	return "runwhp-local-" + strings.TrimPrefix(imageID, "sha256:")
}

// lease returns ctx with the lease that keeps an imported image's content,
// made or renewed.
func (s *localStore) lease(ctx context.Context, imageID string) (context.Context, error) {
	id := leaseID(imageID)
	_, err := s.leases.Create(ctx, leases.WithID(id), leases.WithExpiration(localLease))
	if err != nil && !errors.Is(err, cerrdefs.ErrAlreadyExists) {
		return ctx, err
	}
	return leases.WithLease(ctx, id), nil
}

// List is empty: BuildKit lists images only to keep what they use from
// its prunes, and the content of runwhp's images is runwhp's to keep.
func (s *localStore) List(context.Context, ...string) ([]images.Image, error) { return nil, nil }

// Create, Update and Delete name nothing: images go to runwhp's store
// through the moby exporter.
func (s *localStore) Create(_ context.Context, img images.Image) (images.Image, error) {
	return img, nil
}

func (s *localStore) Update(_ context.Context, img images.Image, _ ...string) (images.Image, error) {
	return img, nil
}

func (s *localStore) Delete(context.Context, string, ...images.DeleteOpt) error { return nil }
