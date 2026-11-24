package image

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
	if from == nil && !config {
		// An image tagged, committed or loaded from another has no
		// registry of its own: its layers come from images that have
		// them and do.
		return s.backfillFromOthers(ctx, recs[0], missing)
	}
	if from == nil {
		return fmt.Errorf("image %s lacks its layers' tar metadata, and no record says what registry it came from: pull it again", Familiar(recs[0].Ref))
	}
	return s.backfillFrom(ctx, from, recs, missing, config)
}

// backfillFromOthers fetches the tar metadata of the layers missing, of
// the image rec, through other images' records that came from a registry
// and list them.
func (s *Store) backfillFromOthers(ctx context.Context, rec *Record, missing []string) error {
	all, err := s.List()
	if err != nil {
		return err
	}
	bySource := map[*Record][]string{}
	var order []*Record
	for _, d := range missing {
		var src *Record
		for _, r := range all {
			if r.Image == nil || r.Digest == "" || Untagged(r.Ref) {
				continue
			}
			if layers, err := r.Layers(); err == nil && slices.Contains(layers, d) {
				src = r
				break
			}
		}
		if src == nil {
			return fmt.Errorf("image %s lacks its layers' tar metadata, and no record says what registry it came from: pull it again, or an image it was made from", Familiar(rec.Ref))
		}
		if bySource[src] == nil {
			order = append(order, src)
		}
		bySource[src] = append(bySource[src], d)
	}
	for _, src := range order {
		if err := s.backfillFrom(ctx, src, nil, bySource[src], false); err != nil {
			return err
		}
	}
	return nil
}

// backfillFrom fetches the layers' tar metadata, and with config the
// configuration blob of recs' image, through from's registry.
func (s *Store) backfillFrom(ctx context.Context, from *Record, recs []*Record, missing []string, config bool) error {
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
