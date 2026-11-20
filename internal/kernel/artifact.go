package kernel

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Media types of the kernel artifact. Its manifest is an OCI image manifest
// with an empty configuration; runwhp downloads only the bzImage layer, and
// the others are there so the corresponding source is published with the
// binary.
const (
	ArtifactType = "application/vnd.quasidock.runwhp.kernel.v1"

	MediaBzImage = types.MediaType("application/vnd.quasidock.runwhp.kernel.bzimage.v1")
	MediaConfig  = types.MediaType("application/vnd.quasidock.runwhp.kernel.config.v1")
	MediaBuild   = types.MediaType("application/vnd.quasidock.runwhp.kernel.build.v1.tar")
	MediaSource  = types.MediaType("application/vnd.quasidock.runwhp.kernel.source.v1.tar+xz")
)

// The OCI empty descriptor's content.
var emptyConfig = []byte("{}")

// Artifact is one build of the kernel, as kernel/Dockerfile's release target
// produces it.
type Artifact struct {
	BzImage []byte
	Config  []byte // the kernel's .config
	Build   []byte // tar of kernel/: Dockerfile, scripts and configuration
	Source  []byte // upstream tarball, as published on kernel.org
	// SourceName is the upstream tarball's file name, such as
	// linux-6.18.64.tar.xz.
	SourceName  string
	Annotations map[string]string
}

const titleAnnotation = "org.opencontainers.image.title"

// Manifest returns the artifact's manifest and its blobs, configuration
// first. The same artifact always gives the same manifest bytes.
func (a *Artifact) Manifest() ([]byte, []v1.Layer, error) {
	if len(a.BzImage) == 0 || len(a.Config) == 0 || len(a.Build) == 0 || len(a.Source) == 0 || a.SourceName == "" {
		return nil, nil, fmt.Errorf("kernel artifact is incomplete")
	}
	config := static.NewLayer(emptyConfig, types.OCIEmptyJSON)
	parts := []struct {
		data  []byte
		media types.MediaType
		title string
	}{
		{a.BzImage, MediaBzImage, "bzImage"},
		{a.Config, MediaConfig, "config"},
		{a.Build, MediaBuild, "build.tar"},
		{a.Source, MediaSource, a.SourceName},
	}
	m := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  ArtifactType,
		Annotations:   maps.Clone(a.Annotations),
	}
	var err error
	if m.Config, err = describe(config, nil); err != nil {
		return nil, nil, err
	}
	blobs := []v1.Layer{config}
	for _, p := range parts {
		l := static.NewLayer(p.data, p.media)
		d, err := describe(l, map[string]string{titleAnnotation: p.title})
		if err != nil {
			return nil, nil, err
		}
		m.Layers = append(m.Layers, d)
		blobs = append(blobs, l)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return raw, blobs, nil
}

func describe(l v1.Layer, annotations map[string]string) (v1.Descriptor, error) {
	digest, err := l.Digest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	size, err := l.Size()
	if err != nil {
		return v1.Descriptor{}, err
	}
	mt, err := l.MediaType()
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mt, Size: size, Digest: digest, Annotations: annotations}, nil
}

// Digest is the digest the artifact's manifest will have once pushed.
func (a *Artifact) Digest() (v1.Hash, error) {
	raw, _, err := a.Manifest()
	if err != nil {
		return v1.Hash{}, err
	}
	d, _, err := v1.SHA256(bytesReader(raw))
	return d, err
}

// Push uploads the artifact's blobs and then its manifest under tag.
func Push(tag name.Tag, a *Artifact, options ...remote.Option) (v1.Hash, error) {
	raw, blobs, err := a.Manifest()
	if err != nil {
		return v1.Hash{}, err
	}
	for _, b := range blobs {
		if err := remote.WriteLayer(tag.Context(), b, options...); err != nil {
			return v1.Hash{}, err
		}
	}
	if err := remote.Put(tag, rawManifest(raw), options...); err != nil {
		return v1.Hash{}, err
	}
	d, _, err := v1.SHA256(bytesReader(raw))
	return d, err
}

// rawManifest satisfies remote.Taggable, and tells remote.Put the media type.
type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error)        { return r, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }
