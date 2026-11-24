package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"

	"github.com/quasidock/runwhp/internal/layer"
)

// SaveImage is an image to save: its configuration blob, and the
// references to save it under, normalized; none saves it untagged, as
// docker save does an image named by ID.
type SaveImage struct {
	Refs         []string
	Config       []byte
	ConfigDigest string
	DiffIDs      []string // every layer, bottom first, empty ones included
}

// FileSource gives the contents of layers' files, for WriteArchive to
// rebuild their tars from their metadata.
type FileSource interface {
	// Open starts reading a layer's files: Get is then called with each of
	// names in turn, whose sizes the metadata gives.
	Open(diffID string, names []string, sizes []int64) (LayerFiles, error)
}

// LayerFiles is what FileSource.Open gives.
type LayerFiles interface {
	storage.FileGetter
	Close() error
}

// SaveLayers lists the layers WriteArchive rebuilds, in the order it
// rebuilds them: each image's, bottom first, each layer once, empty
// layers left out.
func SaveLayers(imgs []SaveImage) []string {
	seen := map[string]bool{}
	var out []string
	for _, img := range imgs {
		for _, d := range img.DiffIDs {
			if d != layer.EmptyDiffID && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// Media types and annotations as Docker 29's save writes them.
const (
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
	mediaConfig   = "application/vnd.oci.image.config.v1+json"
	mediaLayer    = "application/vnd.oci.image.layer.v1.tar"
	annoName      = "io.containerd.image.name"
	annoRef       = "org.opencontainers.image.ref.name"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type ociIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type dockerEntry struct {
	Config       string
	RepoTags     []string
	Layers       []string
	LayerSources map[string]descriptor `json:"LayerSources,omitempty"`
}

// WriteArchive writes imgs to w as Docker 29's docker save does: an OCI
// image layout, every layer as a plain tar named by its diffID, and
// Docker's manifest.json and repositories over the same blobs, which
// Docker of any version, Podman and go-containerregistry load. Each layer
// is rebuilt from its tar metadata, which every layer must have, and
// files from src, and must give back its diffID, or WriteArchive fails.
func WriteArchive(w io.Writer, imgs []SaveImage, layers *layer.Store, src FileSource) error {
	tw := tar.NewWriter(w)
	for _, d := range []string{"blobs/", "blobs/sha256/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: 0o755, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
	}
	written := map[string]bool{}
	sizes := map[string]int64{}
	for _, d := range SaveLayers(imgs) {
		n, err := writeLayer(tw, d, layers, src)
		if err != nil {
			return err
		}
		sizes[d], written[d] = n, true
	}
	var index ociIndex
	index.SchemaVersion, index.MediaType = 2, mediaIndex
	var manifest []dockerEntry
	repositories := map[string]map[string]string{}
	for _, img := range imgs {
		m := ociManifest{SchemaVersion: 2, MediaType: mediaManifest,
			Config: descriptor{MediaType: mediaConfig, Digest: img.ConfigDigest, Size: int64(len(img.Config))}}
		entry := dockerEntry{Config: blobPath(img.ConfigDigest), LayerSources: map[string]descriptor{}}
		for _, d := range img.DiffIDs {
			if d == layer.EmptyDiffID && !written[d] {
				if err := writeBlob(tw, d, layer.EmptyTar); err != nil {
					return err
				}
				written[d], sizes[d] = true, int64(len(layer.EmptyTar))
			}
			desc := descriptor{MediaType: mediaLayer, Digest: d, Size: sizes[d]}
			m.Layers = append(m.Layers, desc)
			entry.Layers = append(entry.Layers, blobPath(d))
			entry.LayerSources[d] = desc
		}
		if !written[img.ConfigDigest] {
			if err := writeBlob(tw, img.ConfigDigest, img.Config); err != nil {
				return err
			}
			written[img.ConfigDigest] = true
		}
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		md := digestOf(data)
		if !written[md] {
			if err := writeBlob(tw, md, data); err != nil {
				return err
			}
			written[md] = true
		}
		desc := descriptor{MediaType: mediaManifest, Digest: md, Size: int64(len(data))}
		if len(img.Refs) == 0 {
			index.Manifests = append(index.Manifests, desc)
		}
		for _, ref := range img.Refs {
			r, err := name.NewTag(ref)
			if err != nil {
				return fmt.Errorf("saving %s: %w", ref, err)
			}
			tagged := desc
			tagged.Annotations = map[string]string{annoName: containerdName(r), annoRef: r.TagStr()}
			index.Manifests = append(index.Manifests, tagged)
			entry.RepoTags = append(entry.RepoTags, Familiar(ref))
			repo := Familiar(r.Context().Name())
			if repositories[repo] == nil {
				repositories[repo] = map[string]string{}
			}
			if n := len(img.DiffIDs); n > 0 {
				repositories[repo][r.TagStr()] = strings.TrimPrefix(img.DiffIDs[n-1], "sha256:")
			}
		}
		manifest = append(manifest, entry)
	}
	for _, f := range []struct {
		name string
		v    any
	}{{"index.json", index}, {"manifest.json", manifest}} {
		data, err := json.Marshal(f.v)
		if err != nil {
			return err
		}
		if err := writeFile(tw, f.name, data); err != nil {
			return err
		}
	}
	if err := writeFile(tw, "oci-layout", []byte(`{"imageLayoutVersion": "1.0.0"}`)); err != nil {
		return err
	}
	if len(repositories) > 0 {
		data, err := json.Marshal(repositories)
		if err != nil {
			return err
		}
		if err := writeFile(tw, "repositories", data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// containerdName is a tag as containerd names it: docker.io/library/alpine:3.20.
func containerdName(t name.Tag) string {
	registry := t.RegistryStr()
	if registry == name.DefaultRegistry {
		registry = "docker.io"
	}
	return registry + "/" + t.RepositoryStr() + ":" + t.TagStr()
}

func blobPath(digest string) string { return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:") }

func writeBlob(tw *tar.Writer, digest string, data []byte) error {
	return writeFile(tw, blobPath(digest), data)
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// layerPlan is what a layer's metadata says before it is rebuilt: its
// files with contents, in order, and the tar's size.
type layerPlan struct {
	names []string
	sizes []int64
	size  int64
}

func planLayer(layers *layer.Store, diffID string) (*layerPlan, error) {
	r, err := layers.Split(diffID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	up := storage.NewJSONUnpacker(r)
	p := &layerPlan{}
	for {
		e, err := up.Next()
		if err == io.EOF {
			return p, nil
		}
		if err != nil {
			return nil, fmt.Errorf("layer %s's tar metadata: %w", diffID, err)
		}
		switch e.Type {
		case storage.SegmentType:
			p.size += int64(len(e.Payload))
		case storage.FileType:
			p.size += e.Size
			if e.Size > 0 {
				p.names = append(p.names, e.GetName())
				p.sizes = append(p.sizes, e.Size)
			}
		}
	}
}

// writeLayer rebuilds a layer's tar into the archive, and returns its size.
func writeLayer(tw *tar.Writer, diffID string, layers *layer.Store, src FileSource) (int64, error) {
	plan, err := planLayer(layers, diffID)
	if err != nil {
		return 0, err
	}
	files, err := src.Open(diffID, plan.names, plan.sizes)
	if err != nil {
		return 0, fmt.Errorf("layer %s: %w", diffID, err)
	}
	defer files.Close()
	split, err := layers.Split(diffID)
	if err != nil {
		return 0, err
	}
	defer split.Close()
	if err := tw.WriteHeader(&tar.Header{Name: blobPath(diffID), Mode: 0o644, Size: plan.size, ModTime: time.Unix(0, 0)}); err != nil {
		return 0, err
	}
	out := asm.NewOutputTarStream(files, storage.NewJSONUnpacker(split))
	defer out.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, digest), out)
	if err != nil {
		return 0, fmt.Errorf("rebuilding layer %s: %w", diffID, err)
	}
	if got := "sha256:" + hex.EncodeToString(digest.Sum(nil)); n != plan.size || got != diffID {
		return 0, fmt.Errorf("layer %s rebuilt as %d bytes with digest %s, not as it was pulled", diffID, n, got)
	}
	return n, nil
}

// ErrMismatch means a file a FileSource gave is not the one asked for.
var ErrMismatch = errors.New("file out of order")
