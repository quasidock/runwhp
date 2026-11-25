package build

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/rootfs"
)

// fakeFile is a file in a fakeRoot.
type fakeFile struct {
	dir      bool
	link     string
	mode     int64
	uid, gid int
	data     string
	mtime    time.Time
}

// fakeRoot is a guest's root in memory: what its steps put into it.
type fakeRoot struct {
	mu    sync.Mutex
	files map[string]*fakeFile
	puts  []fakePut
}

// fakePut is one Put, as the root was asked it.
type fakePut struct {
	dir     string
	opts    rootfs.PutOptions
	entries []string // "name mode uid:gid", in order
}

func newFakeRoot() *fakeRoot {
	return &fakeRoot{files: map[string]*fakeFile{"/": {dir: true, mode: 0o755}, "/etc": {dir: true, mode: 0o755}}}
}

func (r *fakeRoot) Stat(_ context.Context, p string) (*guestspec.PathStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[path.Clean("/"+p)]
	if !ok {
		return nil, &rootfs.NotFoundError{Path: p}
	}
	st := &guestspec.PathStat{Name: path.Base(p), Size: int64(len(f.data)), Mode: fs.FileMode(f.mode)}
	if f.dir {
		st.Mode |= fs.ModeDir
	}
	if f.link != "" {
		st.Mode |= fs.ModeSymlink
		st.LinkTarget = f.link
	}
	return st, nil
}

func (r *fakeRoot) Glob(_ context.Context, pattern string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for p := range r.files {
		if ok, _ := path.Match(pattern, p); ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *fakeRoot) Get(_ context.Context, p string) (io.ReadCloser, *guestspec.PathStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p = path.Clean("/" + p)
	if _, ok := r.files[p]; !ok {
		return nil, nil, &rootfs.NotFoundError{Path: p}
	}
	var names []string
	for q := range r.files {
		if q == p || strings.HasPrefix(q, p+"/") {
			names = append(names, q)
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Base(p)
	for _, q := range names {
		f := r.files[q]
		hdr := &tar.Header{Name: base + strings.TrimPrefix(q, p), Mode: f.mode, Uid: f.uid, Gid: f.gid, ModTime: f.mtime}
		switch {
		case f.dir:
			hdr.Typeflag = tar.TypeDir
		case f.link != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, f.link
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(f.data))
		}
		tw.WriteHeader(hdr)
		tw.Write([]byte(f.data))
	}
	tw.Close()
	return io.NopCloser(&buf), nil, nil
}

func (r *fakeRoot) Put(_ context.Context, dir string, content io.Reader, o rootfs.PutOptions) error {
	put := fakePut{dir: dir, opts: o}
	tr := tar.NewReader(content)
	r.mu.Lock()
	defer r.mu.Unlock()
	base := path.Clean("/" + dir)
	if o.MakeDir {
		for d := base; ; d = path.Dir(d) {
			if _, ok := r.files[d]; !ok {
				r.files[d] = &fakeFile{dir: true, mode: 0o755}
			}
			if d == "/" {
				break
			}
		}
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(tr)
		put.entries = append(put.entries, fmt.Sprintf("%s %o %d:%d", hdr.Name, hdr.Mode, hdr.Uid, hdr.Gid))
		p := path.Join(base, hdr.Name)
		r.files[p] = &fakeFile{dir: hdr.Typeflag == tar.TypeDir, link: hdr.Linkname, mode: hdr.Mode, uid: hdr.Uid, gid: hdr.Gid, data: string(data), mtime: hdr.ModTime}
	}
	r.puts = append(r.puts, put)
	return nil
}

func (r *fakeRoot) Close() error { return nil }

// fakeWorker runs steps in fakeRoots, and makes a layer of each step, its
// diff ID a hash of what the step was given.
type fakeWorker struct {
	mu     sync.Mutex
	images map[string]dockerspec.DockerOCIImage
	layers map[string]bool
	steps  []*Step
	roots  []*fakeRoot
	// fail, if set, fails a process whose arguments contain it with
	// exit code 3.
	fail string
	// source is what Open serves.
	source *fakeRoot
}

func newFakeWorker() *fakeWorker {
	w := &fakeWorker{images: map[string]dockerspec.DockerOCIImage{}, layers: map[string]bool{}}
	var base dockerspec.DockerOCIImage
	base.OS, base.Architecture = "linux", "amd64"
	base.RootFS.Type = "layers"
	base.RootFS.DiffIDs = []digest.Digest{digest.Digest("sha256:" + strings.Repeat("ab", 32))}
	created := time.Date(2026, 4, 16, 23, 53, 26, 0, time.UTC)
	base.History = []ocispecs.History{
		{Created: &created, CreatedBy: "ADD alpine-minirootfs-3.20.10-x86_64.tar.gz / # buildkit", Comment: historyComment},
		{Created: &created, CreatedBy: `CMD ["/bin/sh"]`, Comment: historyComment, EmptyLayer: true},
	}
	base.Config.Env = []string{"PATH=" + DefaultPath}
	base.Config.Cmd = []string{"/bin/sh"}
	w.images["alpine:3.20"] = base
	w.layers[base.RootFS.DiffIDs[0].String()] = true
	return w
}

func (w *fakeWorker) Image(_ context.Context, ref string) (string, []byte, error) {
	img, ok := w.images[ref]
	if !ok {
		return "", nil, fmt.Errorf("%s: no such image", ref)
	}
	raw, _ := json.Marshal(img)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), raw, nil
}

func (w *fakeWorker) HasLayer(d string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.layers[d]
}

func (w *fakeWorker) Step(ctx context.Context, s *Step) (Layer, error) {
	r := newFakeRoot()
	if s.Fill != nil {
		if err := s.Fill(ctx, r); err != nil {
			return Layer{}, err
		}
	}
	w.mu.Lock()
	w.steps = append(w.steps, s)
	w.roots = append(w.roots, r)
	w.mu.Unlock()
	h := sha256.New()
	fmt.Fprintln(h, s.Layers)
	for _, p := range r.puts {
		fmt.Fprintln(h, p.dir, p.opts, p.entries)
	}
	if s.Run != nil {
		fmt.Fprintln(h, s.Run.Args, s.Run.Env, s.Run.User, s.Run.Cwd)
		if w.fail != "" && strings.Contains(strings.Join(s.Run.Args, " "), w.fail) {
			return Layer{}, &ExitError{Code: 3}
		}
		fmt.Fprintf(s.Run.Output, "ran %s\n", strings.Join(s.Run.Args, " "))
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	w.mu.Lock()
	w.layers[d] = true
	w.mu.Unlock()
	return Layer{DiffID: d}, nil
}

func (w *fakeWorker) Open(context.Context, []string) (SourceRoot, error) {
	if w.source == nil {
		return newFakeRoot(), nil
	}
	return w.source, nil
}

func (w *fakeWorker) Import(r io.Reader) (Layer, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return Layer{}, err
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	w.mu.Lock()
	w.layers[d] = true
	w.mu.Unlock()
	return Layer{DiffID: d}, nil
}

// memCache is a build cache in memory.
type memCache struct {
	mu      sync.Mutex
	entries map[string]buildcache.Entry
}

func newMemCache() *memCache { return &memCache{entries: map[string]buildcache.Entry{}} }

func (c *memCache) Get(key string) (buildcache.Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

func (c *memCache) Put(e buildcache.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[e.Key] = e
	return nil
}

// built is a build's image configuration.
func built(t *testing.T, res *Result) dockerspec.DockerOCIImage {
	t.Helper()
	var img dockerspec.DockerOCIImage
	if err := json.Unmarshal(res.Config, &img); err != nil {
		t.Fatal(err)
	}
	return img
}

// createdBy lists an image's history after the base's two entries.
func createdBy(img dockerspec.DockerOCIImage) []string {
	var out []string
	for _, h := range img.History[2:] {
		s := h.CreatedBy
		if !h.EmptyLayer {
			s += " [layer]"
		}
		out = append(out, s)
	}
	return out
}

// steps is how many steps a fake worker ran.
func (w *fakeWorker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.steps)
}
