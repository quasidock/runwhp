package image

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Missing lists what save needs of rec's image that the store lacks: the
// tar metadata of layers stored before runwhp kept it, and whether its
// configuration blob is missing too.
func (s *Store) Missing(img *Image) (layers []string, config bool, err error) {
	if _, err := s.RawConfig(img); errors.Is(err, ErrNoBlob) {
		config = true
	} else if err != nil {
		return nil, false, err
	}
	diffIDs, err := img.Layers()
	if err != nil {
		return nil, false, err
	}
	for _, d := range diffIDs {
		if !s.Layers.HasSplit(d) {
			layers = append(layers, d)
		}
	}
	return layers, config, nil
}

// Backfill fetches what Missing lists from the registry the image was
// pulled from, by its manifest's digest, through any of recs, the records
// of the image: each layer's tar metadata, by reading the layer once and
// keeping nothing else, and the configuration blob. A record written
// before runwhp kept configuration digests gets its digest. It does
// nothing for an image that lacks nothing.
func (s *Store) Backfill(ctx context.Context, recs []*Record) error {
	if len(recs) == 0 {
		return nil
	}
	missing, config, err := s.Missing(recs[0].Image)
	if err != nil || len(missing) == 0 && !config {
		return err
	}
	var from *Record
	for _, r := range recs {
		if r.Digest != "" && !Untagged(r.Ref) {
			from = r
			break
		}
	}
	if from == nil {
		return fmt.Errorf("image %s lacks its layers' tar metadata, and no record says what registry it came from: pull it again", Familiar(recs[0].Ref))
	}
	tag, err := name.ParseReference(from.Ref)
	if err != nil {
		return err
	}
	ref := tag.Context().Digest(from.Digest)
	opts := append([]remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(Keychain),
	}, s.RemoteOptions...)
	s.progressf("Fetching tar metadata for %d layers of %s", len(missing), Familiar(from.Ref))
	remoteImg, err := remote.Image(ref, opts...)
	if err != nil {
		return fmt.Errorf("fetching %s's layers' tar metadata from %s: %w", Familiar(from.Ref), ref, err)
	}
	if config {
		raw, err := remoteImg.RawConfigFile()
		if err != nil {
			return fmt.Errorf("fetching %s's configuration: %w", Familiar(from.Ref), err)
		}
		id := digestOf(raw)
		if from.ConfigDigest != "" && id != from.ConfigDigest {
			return fmt.Errorf("%s's configuration in the registry has digest %s, not %s", ref, id, from.ConfigDigest)
		}
		if err := s.PutBlob(id, raw); err != nil {
			return err
		}
		for _, r := range recs {
			if r.ConfigDigest == "" && r.ID() == id {
				img := *r.Image
				img.ConfigDigest = id
				if err := s.writeRecordAt(r.Path, &img); err != nil {
					return err
				}
				r.ConfigDigest = id
			}
		}
	}
	for _, d := range missing {
		h, err := v1.NewHash(d)
		if err != nil {
			return err
		}
		l, err := remoteImg.LayerByDiffID(h)
		if err != nil {
			return fmt.Errorf("layer %s: %w", d, err)
		}
		if err := s.fetchSplit(l, d); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) fetchSplit(l v1.Layer, diffID string) error {
	size, _ := l.Size()
	s.progressf("  %.19s  fetching %.1f MB for its tar metadata", diffID, float64(size)/1e6)
	rc, err := l.Uncompressed()
	if err != nil {
		return fmt.Errorf("layer %s: %w", diffID, err)
	}
	defer rc.Close()
	if err := s.Layers.PutSplit(diffID, rc); err != nil {
		return fmt.Errorf("layer %s: %w", diffID, err)
	}
	return nil
}
