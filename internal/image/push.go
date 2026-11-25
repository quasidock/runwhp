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
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/gzip"

	"github.com/quasidock/runwhp/internal/layer"
)

// Push writes an image to a registry as docker push does, from a store
// that keeps no compressed layers and no manifests: each layer is rebuilt
// from its tar metadata and its files, as save rebuilds it, gzipped, and
// uploaded unless the registry has a blob of that digest already; then the
// configuration blob, byte for byte, so the image keeps its ID; then a
// Docker v2 schema 2 manifest. Rebuilding is one layer at a time, as the
// files come from one guest, while up to pushJobs layers upload.
//
// gzip is deterministic, so pushing the same image again gives the same
// layer digests, which the registry has; a pulled image pushed elsewhere
// gets digests of its own, not the ones it was pulled with, as with
// Docker's classic store.

// pushJobs is how many layers upload at once.
const pushJobs = 3

// PushStatus is told what became of each layer: its diff ID, and
// "Pushed" or "Layer already exists", as docker push says.
type PushStatus func(diffID, status string)

// Pushed is a push's manifest.
type Pushed struct {
	Digest string
	Size   int
}

// Push pushes the image whose configuration blob raw is, under tag, its
// layers' files from src, spooling each compressed layer in tmp until it is
// uploaded.
func (s *Store) Push(ctx context.Context, tag name.Tag, raw []byte, src FileSource, tmp string, status PushStatus) (*Pushed, error) {
	var cf v1.ConfigFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("the image's configuration: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(Keychain)}, s.RemoteOptions...)
	repo := tag.Context()

	descs := make([]pushDescriptor, len(cf.RootFS.DiffIDs))
	type spooled struct {
		i    int
		l    *spoolLayer
		diff string
	}
	ch := make(chan spooled)
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	go func() {
		defer close(ch)
		for i, d := range cf.RootFS.DiffIDs {
			l, err := s.spool(d.String(), src, tmp)
			if err != nil {
				fail(err)
				return
			}
			select {
			case ch <- spooled{i, l, d.String()}:
			case <-ctx.Done():
				l.remove()
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range pushJobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sp := range ch {
				err := func() error {
					defer sp.l.remove()
					what := "Pushed"
					if exists(repo, sp.l.digest, opts) {
						what = "Layer already exists"
					} else if err := remote.WriteLayer(repo, sp.l, opts...); err != nil {
						return fmt.Errorf("pushing layer %s: %w", sp.diff, err)
					}
					descs[sp.i] = pushDescriptor{MediaType: string(types.DockerLayer), Size: sp.l.size, Digest: sp.l.digest.String()}
					if status != nil {
						status(sp.diff, what)
					}
					return nil
				}()
				if err != nil {
					fail(err)
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfgDigest, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	cfgLayer := &memLayer{data: raw, digest: cfgDigest, mediaType: types.DockerConfigJSON}
	if !exists(repo, cfgDigest, opts) {
		if err := remote.WriteLayer(repo, cfgLayer, opts...); err != nil {
			return nil, fmt.Errorf("pushing the configuration: %w", err)
		}
	}
	m := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        pushDescriptor   `json:"config"`
		Layers        []pushDescriptor `json:"layers"`
	}{2, string(types.DockerManifestSchema2), pushDescriptor{MediaType: string(types.DockerConfigJSON), Size: int64(len(raw)), Digest: cfgDigest.String()}, descs}
	// As Docker's client writes a manifest: indented by three spaces.
	manifest, err := json.MarshalIndent(m, "", "   ")
	if err != nil {
		return nil, err
	}
	if err := remote.Put(tag, rawManifest(manifest), opts...); err != nil {
		return nil, fmt.Errorf("pushing the manifest: %w", err)
	}
	sum := sha256.Sum256(manifest)
	return &Pushed{Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: len(manifest)}, nil
}

// exists reports whether the registry has a blob of digest d; asking
// fails safe, as an upload checks again.
func exists(repo name.Repository, d v1.Hash, opts []remote.Option) bool {
	l, err := remote.Layer(repo.Digest(d.String()), opts...)
	if err != nil {
		return false
	}
	e, ok := l.(interface{ Exists() (bool, error) })
	if !ok {
		return false
	}
	found, err := e.Exists()
	return err == nil && found
}

// spool rebuilds the layer diffID and gzips it into a file in tmp.
func (s *Store) spool(diffID string, src FileSource, tmp string) (*spoolLayer, error) {
	f, err := os.CreateTemp(tmp, "push-*.tar.gz")
	if err != nil {
		return nil, err
	}
	l := &spoolLayer{path: f.Name(), diffID: diffID}
	err = func() error {
		h := sha256.New()
		cw := &countWriter{w: io.MultiWriter(f, h)}
		zw, err := gzip.NewWriterLevel(cw, gzip.DefaultCompression)
		if err != nil {
			return err
		}
		if diffID == layer.EmptyDiffID {
			// An empty layer is no file in the store: its tar is the end
			// marker alone.
			_, err = zw.Write(make([]byte, 1024))
		} else {
			_, err = RebuildLayer(zw, s.Layers, diffID, src)
		}
		if err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		l.digest = v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h.Sum(nil))}
		l.size = cw.n
		return nil
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(l.path)
		return nil, err
	}
	return l, nil
}

// spoolLayer is a gzipped layer in a file, as remote.WriteLayer takes it.
type spoolLayer struct {
	path, diffID string
	digest       v1.Hash
	size         int64
}

func (l *spoolLayer) remove() { os.Remove(l.path) }

func (l *spoolLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l *spoolLayer) DiffID() (v1.Hash, error) { return v1.NewHash(l.diffID) }
func (l *spoolLayer) Compressed() (io.ReadCloser, error) {
	return os.Open(l.path)
}
func (l *spoolLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("a spooled layer is read compressed")
}
func (l *spoolLayer) Size() (int64, error)                { return l.size, nil }
func (l *spoolLayer) MediaType() (types.MediaType, error) { return types.DockerLayer, nil }

// memLayer is a blob in memory, as remote.WriteLayer takes it: the
// configuration.
type memLayer struct {
	data      []byte
	digest    v1.Hash
	mediaType types.MediaType
}

func (l *memLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l *memLayer) DiffID() (v1.Hash, error) { return l.digest, nil }
func (l *memLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.data)), nil
}
func (l *memLayer) Uncompressed() (io.ReadCloser, error) { return l.Compressed() }
func (l *memLayer) Size() (int64, error)                 { return int64(len(l.data)), nil }
func (l *memLayer) MediaType() (types.MediaType, error)  { return l.mediaType, nil }

// rawManifest is a manifest, as remote.Put takes it.
type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error)        { return r, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return types.DockerManifestSchema2, nil }

// countWriter counts what it writes.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// pushDescriptor is a manifest's descriptor, its fields in the order
// Docker writes them.
type pushDescriptor struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
}
