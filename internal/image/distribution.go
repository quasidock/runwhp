package image

import (
	"context"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Distribution looks up what a registry has for ref, without pulling it,
// as docker's distribution inspect does: its manifest's descriptor, and
// the platforms it offers, each of an index's images or the one image's.
func (s *Store) Distribution(ctx context.Context, ref string) (v1.Descriptor, []v1.Platform, error) {
	r, err := normalize(ref)
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	opts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(Keychain)}, s.RemoteOptions...)
	d, err := remote.Get(r, opts...)
	if err != nil {
		return v1.Descriptor{}, nil, fmt.Errorf("looking up %s: %w", r.Name(), err)
	}
	desc := v1.Descriptor{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size}
	var platforms []v1.Platform
	if d.MediaType.IsIndex() {
		idx, err := d.ImageIndex()
		if err != nil {
			return desc, nil, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return desc, nil, err
		}
		for _, m := range im.Manifests {
			if m.Platform != nil {
				platforms = append(platforms, *m.Platform)
			}
		}
		return desc, platforms, nil
	}
	img, err := d.Image()
	if err != nil {
		return desc, nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return desc, nil, err
	}
	return desc, []v1.Platform{{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant, OSVersion: cf.OSVersion}}, nil
}
