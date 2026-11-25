package build

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/patternmatcher"

	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/rootfs"
)

// srcStat is what a copy needs of a source path: where its symlinks lead,
// and whether it is a directory.
type srcStat struct {
	path string
	dir  bool
}

// walkFunc takes each file a walk finds: its name from the path walked,
// "." for the path itself, its header, and, for a regular file, a way to
// read it, good until walkFunc returns.
type walkFunc func(name string, hdr *tar.Header, open func() (io.ReadCloser, error)) error

// sourceFS is what COPY copies from: the build context, or a guest's root.
type sourceFS interface {
	glob(ctx context.Context, pattern string) ([]string, error)
	stat(ctx context.Context, p string) (srcStat, error)
	walk(ctx context.Context, p string, fn walkFunc) error
}

// rootSource is a guest's root to copy from: COPY --from's.
type rootSource struct{ r Root }

func (s rootSource) glob(ctx context.Context, pattern string) ([]string, error) {
	return s.r.Glob(ctx, path.Clean("/"+pattern))
}

func (s rootSource) stat(ctx context.Context, p string) (srcStat, error) {
	cur := path.Clean("/" + p)
	for range 40 {
		st, err := s.r.Stat(ctx, cur)
		var nf *rootfs.NotFoundError
		if errors.As(err, &nf) {
			return srcStat{}, notFound(p)
		}
		if err != nil {
			return srcStat{}, err
		}
		if st.LinkTarget == "" {
			return srcStat{path: cur, dir: st.Mode.IsDir()}, nil
		}
		cur = st.LinkTarget
	}
	return srcStat{}, fmt.Errorf("%s: too many levels of symbolic links", p)
}

func (s rootSource) walk(ctx context.Context, p string, fn walkFunc) error {
	st, err := s.stat(ctx, p)
	if err != nil {
		return err
	}
	rc, _, err := s.r.Get(ctx, st.path)
	if err != nil {
		return err
	}
	defer rc.Close()
	base := path.Base(st.path)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if st.path == "/" {
			name = strings.TrimPrefix(strings.TrimPrefix(name, "/"), "./")
		} else {
			name = strings.TrimPrefix(strings.TrimPrefix(name, base), "/")
		}
		if name == "" || name == "." {
			name = "."
		}
		hdr.Name = name
		var open func() (io.ReadCloser, error)
		if hdr.Typeflag == tar.TypeReg {
			open = func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }
		}
		if err := fn(name, hdr, open); err != nil {
			return err
		}
	}
}

// copyCmd is a COPY or ADD to dispatch.
type copyCmd struct {
	params    instructions.SourcesAndDest
	add       bool
	from      *stage
	fromImage string
	chown     string
	chmod     string
	link      bool
	exclude   []string
	parents   bool
	checksum  string
	unpack    bool
	code      string
	onBuild   bool
}

// copySrc is one source of a COPY or ADD.
type copySrc struct {
	fs      sourceFS
	pattern string
	// local says the source is the build context's, which ADD unpacks if
	// it is an archive.
	local bool
	// file is a source that is not in fs: a heredoc's contents, or what
	// ADD downloaded.
	file *srcFile
}

// srcFile is a file a COPY or ADD has the contents of.
type srcFile struct {
	name  string
	data  []byte
	host  string // a file on the host, instead of data
	mode  int64
	mtime time.Time
}

// copyOpts say how files are copied.
type copyOpts struct {
	chown   string
	chmod   func(mode int64, dir bool) int64
	unpack  bool
	exclude *patternmatcher.PatternMatcher
	parents bool
	now     time.Time
}

// dispatchCopy copies files in, as BuildKit's dispatchCopy.
func (b *builder) dispatchCopy(ctx context.Context, s *stage, c copyCmd) (err error) {
	dest := c.params.DestPath
	if !path.IsAbs(dest) {
		if dest == "." || dest == "" {
			dest = "./"
		}
		trailing := strings.HasSuffix(dest, "/")
		dest = path.Join("/", workdirOf(s.img), dest)
		if trailing && dest != "/" {
			dest += "/"
		}
	}
	if c.chown != "" && c.link && chownByName(c.chown) {
		return fmt.Errorf("--chown=%s: user and group names can't be resolved when used with --link, use numeric uid[:gid] instead", c.chown)
	}
	opts := copyOpts{chown: c.chown, unpack: c.add && c.unpack, parents: c.parents, now: b.now()}
	if c.chmod != "" {
		f, err := parseChmod(c.chmod)
		if err != nil {
			return err
		}
		opts.chmod = f
	}
	if len(c.exclude) > 0 {
		pm, err := patternmatcher.New(c.exclude)
		if err != nil {
			return err
		}
		opts.exclude = pm
	}
	if c.checksum != "" {
		if !c.add {
			return errors.New("checksum can't be specified for COPY")
		}
		if len(c.params.SourcePaths) != 1 {
			return errors.New("checksum can't be specified for multiple sources")
		}
		if !isHTTP(c.params.SourcePaths[0]) {
			return errors.New("checksum requires HTTP(S) or Git sources")
		}
	}

	v := b.progress.start(b.prefix(s, onBuildPrefix(c.onBuild)+c.code))
	cached := false
	defer func() { v.end(err, cached) }()
	msg := "COPY"
	if c.add {
		msg = "ADD"
	}
	if c.parents {
		msg += " --parents"
	}
	if c.chown != "" {
		msg += " --chown=" + c.chown
	}
	if c.chmod != "" {
		msg += " --chmod=" + c.chmod
	}

	var fsys sourceFS
	var srcKey string
	var sourceLayers []string
	switch {
	case c.from != nil:
		srcKey = "stage:" + c.from.key
		sourceLayers = layersOf(c.from.img)
	case c.fromImage != "":
		id, raw, err := b.w.Image(ctx, c.fromImage)
		if err != nil {
			return err
		}
		var img dockerspec.DockerOCIImage
		if err := json.Unmarshal(raw, &img); err != nil {
			return err
		}
		srcKey = "image:" + id
		sourceLayers = layersOf(img)
	default:
		if b.o.Context != nil {
			fsys = b.o.Context
		}
	}

	key := []string{"dest=" + dest, "chown=" + c.chown, "chmod=" + c.chmod, fmt.Sprint("unpack=", opts.unpack, " parents=", c.parents), "exclude=" + strings.Join(c.exclude, ",")}
	var srcs []copySrc
	for _, src := range c.params.SourcePaths {
		msg += " " + src
		if isHTTP(src) {
			if !c.add {
				return errors.New("source can't be a URL for COPY")
			}
			f, err := b.download(ctx, src, c.checksum)
			if err != nil {
				return err
			}
			srcs = append(srcs, copySrc{file: f})
			sum := sha256.Sum256(f.data)
			key = append(key, "url", f.name, hex.EncodeToString(sum[:]), f.mtime.String())
			continue
		}
		if srcKey == "" {
			if b.o.Context == nil {
				return notFound(src)
			}
			sum, err := b.contextKey(src)
			if err != nil {
				return err
			}
			key = append(key, "context", src, sum)
			srcs = append(srcs, copySrc{pattern: src, local: true})
			continue
		}
		key = append(key, srcKey, src)
		srcs = append(srcs, copySrc{pattern: src})
	}
	for _, sc := range c.params.SourceContents {
		msg += " <<" + sc.Path
		key = append(key, "heredoc", sc.Path, sc.Data)
		srcs = append(srcs, copySrc{file: &srcFile{name: sc.Path, data: []byte(sc.Data), mode: 0o644, mtime: opts.now}})
	}
	msg += " " + c.params.DestPath + " # buildkit"

	setFS := func() {
		for i := range srcs {
			if srcs[i].file == nil {
				srcs[i].fs = fsys
			}
		}
	}
	step := &Step{Fill: func(ctx context.Context, r Root) error {
		setFS()
		return copyInto(ctx, r, srcs, dest, opts)
	}}
	if srcKey != "" {
		// Another stage's or image's files, read from a guest of its own
		// before the step's boots, and spooled meanwhile.
		var spool *spoolFS
		defer func() {
			if spool != nil {
				spool.close()
			}
		}()
		step.Prepare = func(ctx context.Context) error {
			r, err := b.w.Open(ctx, sourceLayers)
			if err != nil {
				return err
			}
			defer r.Close()
			spool, err = newSpool(ctx, rootSource{r}, srcs, b.o.TempDir)
			fsys = spool
			return err
		}
	}
	if c.link && c.chmod == "" {
		step.Build = func(ctx context.Context) (Layer, error) {
			setFS()
			return b.linkLayer(ctx, srcs, dest, opts)
		}
	}
	cached, err = b.layerStep(ctx, s, msg, key, step)
	return err
}

// contextKey hashes what a context source selects.
func (b *builder) contextKey(src string) (string, error) {
	ctx := context.Background()
	paths := []string{src}
	if containsWildcards(src) {
		m, err := b.o.Context.glob(ctx, src)
		if err != nil {
			return "", err
		}
		paths = m
	}
	h := sha256.New()
	for _, p := range paths {
		sum, err := b.o.Context.hash(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %s\n", p, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyEntry is a source path a pattern found.
type copyEntry struct {
	src  copySrc
	path string
	dir  bool
}

// expand finds what each source names.
func expand(ctx context.Context, srcs []copySrc) ([]copyEntry, error) {
	var out []copyEntry
	for _, s := range srcs {
		if s.file != nil {
			out = append(out, copyEntry{src: s})
			continue
		}
		if s.fs == nil {
			return nil, notFound(s.pattern)
		}
		pattern := s.pattern
		if base, rest, ok := strings.Cut(pattern, "/./"); ok {
			pattern = path.Join(base, rest)
		}
		paths := []string{path.Clean("/" + pattern)}
		if containsWildcards(pattern) {
			m, err := s.fs.glob(ctx, pattern)
			if err != nil {
				return nil, err
			}
			paths = m
		}
		for _, p := range paths {
			st, err := s.fs.stat(ctx, p)
			if err != nil {
				return nil, err
			}
			out = append(out, copyEntry{src: s, path: p, dir: st.dir})
		}
	}
	return out, nil
}

// copyInto copies srcs into the root r at dest, as BuildKit's copy does.
func copyInto(ctx context.Context, r Root, srcs []copySrc, dest string, o copyOpts) error {
	entries, err := expand(ctx, srcs)
	if err != nil {
		return err
	}
	destDir := strings.HasSuffix(dest, "/") || len(entries) > 1
	clean := path.Clean(dest)
	if !destDir && len(entries) == 1 {
		e := entries[0]
		switch {
		case e.dir, o.unpack && e.src.local && isArchive(ctx, e):
			destDir = true
		default:
			if st, err := (rootSource{r}).stat(ctx, clean); err == nil && st.dir {
				destDir = true
			}
		}
	}
	for _, e := range entries {
		dir, name := clean, ""
		if !destDir {
			dir, name = path.Dir(clean), path.Base(clean)
		}
		if err := putEntry(ctx, r, dir, name, e, o); err != nil {
			return err
		}
	}
	return nil
}

// putEntry puts one source into dir, under name if set, else under its
// own, or, for a directory or an archive, its contents.
func putEntry(ctx context.Context, r Root, dir, name string, e copyEntry, o copyOpts) error {
	putDir := dir
	if putDir != "/" {
		putDir += "/"
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := writeEntry(ctx, tarEmit(tw), name, e, o)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	err := r.Put(ctx, putDir, pr, rootfs.PutOptions{MakeDir: true, Merge: true, Chown: o.chown, ChownParents: o.chown != ""})
	pr.CloseWithError(errors.New("the copy ended"))
	return err
}

// writeEntry writes one source's entries: a file under name, or its own
// base name; a directory's contents, with "." for the directory itself;
// an archive's contents.
// emitFunc takes an entry to write, and its contents, for a regular file.
type emitFunc func(hdr *tar.Header, body io.Reader) error

// tarEmit writes entries to tw.
func tarEmit(tw *tar.Writer) emitFunc {
	return func(hdr *tar.Header, body io.Reader) error {
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if body == nil {
			return nil
		}
		_, err := io.Copy(tw, body)
		return err
	}
}

func writeEntry(ctx context.Context, emit emitFunc, name string, e copyEntry, o copyOpts) error {
	if f := e.src.file; f != nil {
		if name == "" {
			name = path.Base("/" + f.name)
		}
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: f.mode, ModTime: f.mtime}
		var body io.Reader
		if f.host != "" {
			h, err := os.Open(f.host)
			if err != nil {
				return err
			}
			defer h.Close()
			fi, err := h.Stat()
			if err != nil {
				return err
			}
			hdr.Size, body = fi.Size(), h
		} else {
			hdr.Size, body = int64(len(f.data)), strings.NewReader(string(f.data))
		}
		if o.chmod != nil {
			hdr.Mode = o.chmod(hdr.Mode, false)
		}
		return emit(hdr, body)
	}
	if o.unpack && e.src.local && !e.dir && isArchive(ctx, e) {
		return unpackEntry(ctx, emit, e)
	}
	prefix := ""
	if o.parents {
		prefix = strings.TrimPrefix(parentsPath(e.src.pattern, e.path), "/")
	}
	return e.src.fs.walk(ctx, e.path, func(rel string, hdr *tar.Header, open func() (io.ReadCloser, error)) error {
		if o.exclude != nil && rel != "." {
			if ok, _ := o.exclude.MatchesOrParentMatches(rel); ok {
				return nil
			}
		}
		switch {
		case rel == "." && e.dir && prefix == "":
			// The destination directory itself: its mode and times, but
			// not its owner.
			hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
			hdr.Name = "."
		case rel == "." && !e.dir:
			hdr.Name = name
			if hdr.Name == "" {
				hdr.Name = path.Base(e.path)
			}
			if prefix != "" {
				hdr.Name = prefix
			}
		default:
			hdr.Name = path.Join(prefix, rel)
		}
		if hdr.Name != "." && o.chmod != nil {
			hdr.Mode = o.chmod(hdr.Mode, hdr.Typeflag == tar.TypeDir)
		}
		if open == nil {
			return emit(hdr, nil)
		}
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return emit(hdr, rc)
	})
}

// parentsPath is where COPY --parents puts p: its path from the pattern's
// pivot, the "/./" in it, or from the root.
func parentsPath(pattern, p string) string {
	base, _, ok := strings.Cut(pattern, "/./")
	if !ok {
		return p
	}
	return "/" + strings.TrimPrefix(strings.TrimPrefix(p, path.Clean("/"+base)), "/")
}

// isArchive reports whether a context file is a tar, compressed or not.
func isArchive(ctx context.Context, e copyEntry) bool {
	found := false
	e.src.fs.walk(ctx, e.path, func(_ string, hdr *tar.Header, open func() (io.ReadCloser, error)) error {
		if open == nil {
			return nil
		}
		rc, err := open()
		if err != nil {
			return nil
		}
		defer rc.Close()
		dr, err := image.Decompress(rc)
		if err != nil {
			return nil
		}
		defer dr.Close()
		_, err = tar.NewReader(dr).Next()
		found = err == nil
		return nil
	})
	return found
}

// unpackEntry writes an archive's entries, as ADD unpacks it.
func unpackEntry(ctx context.Context, emit emitFunc, e copyEntry) error {
	return e.src.fs.walk(ctx, e.path, func(_ string, _ *tar.Header, open func() (io.ReadCloser, error)) error {
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		dr, err := image.Decompress(rc)
		if err != nil {
			return err
		}
		defer dr.Close()
		tr := tar.NewReader(dr)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			name := path.Clean(strings.TrimLeft(hdr.Name, "/"))
			if name == "." || strings.HasPrefix(name, "../") || name == ".." {
				continue
			}
			hdr.Name = name
			if err := emit(hdr, tr); err != nil {
				return err
			}
		}
	})
}

// linkLayer makes COPY --link's layer on the host: the files at dest,
// with the directories above them, as if copied onto an empty root.
func (b *builder) linkLayer(ctx context.Context, srcs []copySrc, dest string, o copyOpts) (Layer, error) {
	entries, err := expand(ctx, srcs)
	if err != nil {
		return Layer{}, err
	}
	uid, gid := 0, 0
	if o.chown != "" {
		u, g, hasG := strings.Cut(o.chown, ":")
		n, err := strconv.Atoi(u)
		if err != nil {
			return Layer{}, fmt.Errorf("--chown=%s: not a uid", o.chown)
		}
		uid, gid = n, n
		if hasG {
			if gid, err = strconv.Atoi(g); err != nil {
				return Layer{}, fmt.Errorf("--chown=%s: not a gid", o.chown)
			}
		}
	}
	destDir := strings.HasSuffix(dest, "/") || len(entries) > 1 || len(entries) == 1 && entries[0].dir
	clean := path.Clean(dest)
	parent := clean
	if !destDir {
		parent = path.Dir(clean)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			// The directories above, made as a copy makes them.
			written := map[string]bool{}
			cur := ""
			for _, part := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
				if part == "" {
					continue
				}
				cur = path.Join(cur, part)
				if err := tw.WriteHeader(&tar.Header{Name: cur + "/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: uid, Gid: gid, ModTime: o.now}); err != nil {
					return err
				}
				written[cur+"/"] = true
			}
			for _, e := range entries {
				name := ""
				if !destDir {
					name = path.Base(clean)
				}
				if err := writeEntry(ctx, linkEmit(tarEmit(tw), written, strings.TrimPrefix(parent, "/"), uid, gid, o.chown != ""), name, e, o); err != nil {
					return err
				}
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
	}()
	l, err := b.w.Import(pr)
	pr.CloseWithError(errors.New("the copy ended"))
	return l, err
}

// linkEmit moves entries below prefix, as linkLayer writes them, gives
// them chown's owner, and leaves out those written already.
func linkEmit(emit emitFunc, written map[string]bool, prefix string, uid, gid int, chown bool) emitFunc {
	return func(hdr *tar.Header, body io.Reader) error {
		if hdr.Name == "." {
			if prefix == "" {
				return nil
			}
			hdr.Name = prefix
		} else {
			hdr.Name = path.Join(prefix, hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Name += "/"
		}
		if chown {
			hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = uid, gid, "", ""
		}
		if written[hdr.Name] {
			return nil // a directory above, written already
		}
		written[hdr.Name] = true
		return emit(hdr, body)
	}
}

// download fetches ADD's URL to memory, checked against checksum if set.
func (b *builder) download(ctx context.Context, src, checksum string) (*srcFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid response status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if checksum != "" && got != checksum {
		return nil, fmt.Errorf("digest mismatch %s: %s", got, checksum)
	}
	name := "__unnamed__"
	if u, err := url.Parse(src); err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			name = base
		}
	}
	mtime := b.now()
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			mtime = t
		}
	}
	return &srcFile{name: name, data: data, mode: 0o600, mtime: mtime}, nil
}

func isHTTP(src string) bool {
	return (strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")) && !strings.HasSuffix(src, ".git")
}

// chownByName reports whether a --chown names a user or group rather
// than numbering them.
func chownByName(chown string) bool {
	u, g, _ := strings.Cut(chown, ":")
	_, uerr := strconv.Atoi(u)
	if g == "" {
		return uerr != nil
	}
	_, gerr := strconv.Atoi(g)
	return uerr != nil || gerr != nil
}

// containsWildcards is BuildKit's.
func containsWildcards(name string) bool {
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '*', '?', '[':
			return true
		case '\\':
			i++
		}
	}
	return false
}
