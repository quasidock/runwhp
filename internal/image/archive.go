package image

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"
)

// Image archives are what `docker save` writes and `docker load` reads: a
// tar of blobs plus an index of the images in it. Docker 25 and later
// write two indexes over the same blobs: manifest.json, Docker's own
// (an entry per image: its configuration's path, its tags and its layers'
// paths), and an OCI image layout (oci-layout, index.json and
// blobs/sha256/). Older Docker, Podman and go-containerregistry write
// manifest.json alone; skopeo and buildah's oci-archive, the OCI layout
// alone. Layers may be plain tars or compressed.

// Archive is an image archive opened for reading.
type Archive struct {
	f       *os.File
	entries map[string]archiveEntry
}

type archiveEntry struct{ off, size int64 }

// ArchiveImage is an image in an archive.
type ArchiveImage struct {
	// RepoTags are the references the archive names the image by, as given;
	// none for an image saved by ID.
	RepoTags     []string
	Config       []byte // the configuration blob, byte for byte
	ConfigDigest string // its digest: the image's ID
	ConfigFile   *v1.ConfigFile
	// Layers are the layers' blobs, bottom first, one per diffID in the
	// configuration.
	Layers []ArchiveLayer
}

// ArchiveLayer is a layer's blob in an archive.
type ArchiveLayer struct {
	DiffID string
	a      *Archive
	path   string
}

// Open opens the layer as a plain tar, decompressing it if it was
// compressed.
func (l ArchiveLayer) Open() (io.ReadCloser, error) {
	r, err := l.a.open(l.path)
	if err != nil {
		return nil, err
	}
	return Decompress(r)
}

// OpenArchive indexes the entries of the archive in f.
func OpenArchive(f *os.File) (*Archive, error) {
	a := &Archive{f: f, entries: map[string]archiveEntry{}}
	cr := &countingReader{f: f}
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		a.entries[path.Clean(hdr.Name)] = archiveEntry{cr.n, hdr.Size}
	}
	return a, nil
}

// countingReader tells tar.Reader where it is, and lets it seek past the
// entries' contents rather than read them.
type countingReader struct {
	f *os.File
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	r.n += int64(n)
	return n, err
}

func (r *countingReader) Seek(off int64, whence int) (int64, error) {
	n, err := r.f.Seek(off, whence)
	if err == nil {
		r.n = n
	}
	return n, err
}

func (a *Archive) open(name string) (io.Reader, error) {
	e, ok := a.entries[path.Clean(name)]
	if !ok {
		return nil, fmt.Errorf("the archive has no %s", name)
	}
	return io.NewSectionReader(a.f, e.off, e.size), nil
}

func (a *Archive) read(name string, max int64) ([]byte, error) {
	r, err := a.open(name)
	if err != nil {
		return nil, err
	}
	if e := a.entries[path.Clean(name)]; e.size > max {
		return nil, fmt.Errorf("the archive's %s is too large: %d bytes", name, e.size)
	}
	return io.ReadAll(r)
}

const maxJSON = 16 << 20

// Images lists the images in the archive, from manifest.json if it has
// one, else from its OCI layout.
func (a *Archive) Images() ([]*ArchiveImage, error) {
	if _, ok := a.entries["manifest.json"]; ok {
		return a.dockerImages()
	}
	if _, ok := a.entries["index.json"]; ok {
		return a.ociImages()
	}
	return nil, errors.New("this is not an image archive: it has neither manifest.json nor index.json")
}

func (a *Archive) dockerImages() ([]*ArchiveImage, error) {
	data, err := a.read("manifest.json", maxJSON)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("the archive's manifest.json: %w", err)
	}
	var out []*ArchiveImage
	for _, e := range entries {
		img, err := a.image(e.Config, e.RepoTags, e.Layers)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

// image reads an image's configuration and pairs its layers' paths with
// its diffIDs.
func (a *Archive) image(configPath string, tags, layers []string) (*ArchiveImage, error) {
	config, err := a.read(configPath, maxJSON)
	if err != nil {
		return nil, err
	}
	img := &ArchiveImage{RepoTags: tags, Config: config, ConfigDigest: digestOf(config)}
	// A configuration named by its digest must have it.
	if h := strings.TrimSuffix(path.Base(configPath), ".json"); len(h) == 64 && "sha256:"+h != img.ConfigDigest {
		return nil, fmt.Errorf("the archive's %s has digest %s", configPath, img.ConfigDigest)
	}
	cf, err := v1.ParseConfigFile(bytes.NewReader(config))
	if err != nil {
		return nil, fmt.Errorf("the archive's %s: %w", configPath, err)
	}
	img.ConfigFile = cf
	if len(layers) != len(cf.RootFS.DiffIDs) {
		return nil, fmt.Errorf("image %s lists %d layers but its configuration has %d diffIDs",
			img.ConfigDigest, len(layers), len(cf.RootFS.DiffIDs))
	}
	for i, p := range layers {
		if _, ok := a.entries[path.Clean(p)]; !ok {
			return nil, fmt.Errorf("the archive has no %s", p)
		}
		img.Layers = append(img.Layers, ArchiveLayer{DiffID: cf.RootFS.DiffIDs[i].String(), a: a, path: p})
	}
	return img, nil
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Platform    *v1.Platform      `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func blobName(digest string) (string, error) {
	alg, h, ok := strings.Cut(digest, ":")
	if !ok || alg != "sha256" || len(h) != 64 {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	return "blobs/sha256/" + h, nil
}

func (a *Archive) blob(digest string) ([]byte, error) {
	p, err := blobName(digest)
	if err != nil {
		return nil, err
	}
	data, err := a.read(p, maxJSON)
	if err != nil {
		return nil, err
	}
	if got := digestOf(data); got != digest {
		return nil, fmt.Errorf("the archive's %s has digest %s", p, got)
	}
	return data, nil
}

func (a *Archive) ociImages() ([]*ArchiveImage, error) {
	data, err := a.read("index.json", maxJSON)
	if err != nil {
		return nil, err
	}
	var index struct{ Manifests []ociDescriptor }
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("the archive's index.json: %w", err)
	}
	var out []*ArchiveImage
	byConfig := map[string]*ArchiveImage{}
	for _, d := range index.Manifests {
		img, err := a.ociImage(d, 0)
		if err != nil {
			return nil, err
		}
		if tag := ociName(d.Annotations); tag != "" {
			img.RepoTags = []string{tag}
		}
		// Each tag has an entry of its own in index.json.
		if prev := byConfig[img.ConfigDigest]; prev != nil {
			prev.RepoTags = append(prev.RepoTags, img.RepoTags...)
			continue
		}
		byConfig[img.ConfigDigest] = img
		out = append(out, img)
	}
	return out, nil
}

// ociName is the reference an OCI layout's annotations give an image:
// containerd's full name, else the ref.name annotation if it is a whole
// reference rather than a tag alone.
func ociName(ann map[string]string) string {
	if n := ann["io.containerd.image.name"]; n != "" {
		return n
	}
	if n := ann["org.opencontainers.image.ref.name"]; strings.ContainsAny(n, "/:") {
		if _, err := name.ParseReference(n); err == nil {
			return n
		}
	}
	return ""
}

// ociImage reads an image manifest, or in an image index, the linux/amd64
// one.
func (a *Archive) ociImage(d ociDescriptor, depth int) (*ArchiveImage, error) {
	data, err := a.blob(d.Digest)
	if err != nil {
		return nil, err
	}
	var m struct {
		MediaType string
		Config    ociDescriptor
		Layers    []ociDescriptor
		Manifests []ociDescriptor
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	if len(m.Manifests) > 0 {
		if depth > 2 {
			return nil, fmt.Errorf("image index %s nests too deep", d.Digest)
		}
		for _, sub := range m.Manifests {
			if sub.Platform != nil && sub.Platform.OS == Platform.OS && sub.Platform.Architecture == Platform.Architecture {
				if p, err := blobName(sub.Digest); err == nil {
					if _, ok := a.entries[p]; ok {
						return a.ociImage(sub, depth+1)
					}
				}
			}
		}
		return nil, fmt.Errorf("image index %s has no %s/%s image in the archive", d.Digest, Platform.OS, Platform.Architecture)
	}
	configPath, err := blobName(m.Config.Digest)
	if err != nil {
		return nil, err
	}
	var layers []string
	for _, l := range m.Layers {
		p, err := blobName(l.Digest)
		if err != nil {
			return nil, err
		}
		layers = append(layers, p)
	}
	return a.image(configPath, nil, layers)
}

// Decompress gives a layer as a plain tar, whether it is one or is
// compressed with gzip, zstd or bzip2, as it starts. xz is refused.
func Decompress(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	magic, _ := br.Peek(6)
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		return zr, nil
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	case bytes.HasPrefix(magic, []byte("BZh")):
		return io.NopCloser(bzip2.NewReader(br)), nil
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		return nil, errors.New("xz-compressed tars are not supported: decompress it first")
	}
	return io.NopCloser(br), nil
}
