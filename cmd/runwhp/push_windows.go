//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/vmm"
)

const pushUsage = `Usage: runwhp push [-a] [-q] NAME[:TAG]

Push an image to its registry, as docker push does: a Docker v2 schema 2
manifest, gzip layers, and the image's configuration as it is, so the
image keeps its ID. runwhp keeps no compressed layers, so it rebuilds each
from its metadata and files, in a guest it boots for the purpose, and
gzips it; the registry skips a layer it has. Credentials are those docker
login stored.

  -a, --all-tags  push every tag of the repository in the store
  -q, --quiet     print only the reference pushed
`

func cmdPush(args []string, stdout io.Writer) int {
	fs := newFlags("push", pushUsage)
	var allTags, quiet, debug bool
	fs.BoolVar(&allTags, "a", false, "")
	fs.BoolVar(&allTags, "all-tags", false, "")
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.BoolVar(&debug, "debug", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	arg := fs.Arg(0)
	ctx, stop := interruptible()
	defer stop()
	// A prune waits until the push ends, and keeps its layers.
	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	is := image.Open(root)
	is.Progress = os.Stderr

	recs, err := pushRecords(is, arg, allTags)
	if err != nil {
		errorf("push: %v", err)
		return 1
	}
	if err := pushTags(ctx, root, is, recs, quiet, debug, stdout); err != nil {
		if ctx.Err() != nil {
			errorf("push cancelled")
			return 130
		}
		errorf("push: %v", err)
		return 1
	}
	return 0
}

// pushTags pushes records to their registries, writing docker push's
// output to out, or only the references pushed if quiet. The caller holds
// the store from prunes.
func pushTags(ctx context.Context, root string, is *image.Store, recs []pushRecord, quiet, debug bool, out io.Writer) error {
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	repo := image.Familiar(recs[0].tag.Context().Name())
	if !quiet {
		fmt.Fprintf(out, "The push refers to repository [%s]\n", repo)
	}
	for _, r := range recs {
		if err := is.Backfill(ctx, []*image.Record{r.rec}); err != nil {
			return err
		}
		raw, err := is.RawConfig(r.rec.Image)
		if err != nil {
			return err
		}
		layers, err := r.rec.Layers()
		if err != nil {
			return err
		}
		var order []string
		for _, d := range layers {
			if d != layer.EmptyDiffID {
				order = append(order, d)
			}
		}
		src := vmm.NewLayerServers(ctx, root, is.Layers, order, debug)
		var mu sync.Mutex
		pushed, err := is.Push(ctx, r.tag, raw, src, tmp, func(diffID, status string) {
			if quiet {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(out, "%s: %s\n", strings.TrimPrefix(diffID, "sha256:")[:12], status)
		})
		src.Close()
		if err != nil {
			return err
		}
		if quiet {
			fmt.Fprintln(out, image.Familiar(r.tag.Name()))
		} else {
			fmt.Fprintf(out, "%s: digest: %s size: %d\n", r.tag.TagStr(), pushed.Digest, pushed.Size)
		}
		// The record remembers the manifest, as Docker's RepoDigests do.
		rd := repo + "@" + pushed.Digest
		if !slices.Contains(r.rec.RepoDigests, rd) {
			r.rec.RepoDigests = append(r.rec.RepoDigests, rd)
			if err := is.Put(r.rec.Ref, r.rec.Image); err != nil {
				errorf("push: recording the digest: %v", err)
			}
		}
	}
	return nil
}

// pushRecord is a record to push, and the tag to push it under.
type pushRecord struct {
	rec *image.Record
	tag name.Tag
}

// pushRecords finds what arg names: the record of a tag, or with all
// every tag of a repository; not an ID, which names no repository.
func pushRecords(is *image.Store, arg string, all bool) ([]pushRecord, error) {
	r, err := name.ParseReference(arg)
	if err != nil {
		return nil, fmt.Errorf("invalid reference %q: %w", arg, err)
	}
	if _, isDigest := r.(name.Digest); isDigest {
		return nil, fmt.Errorf("cannot push a digest reference %s", arg)
	}
	list, err := is.List()
	if err != nil {
		return nil, err
	}
	var out []pushRecord
	for _, rec := range list {
		if rec.Image == nil || image.Untagged(rec.Ref) {
			continue
		}
		t, err := name.NewTag(rec.Ref)
		if err != nil {
			continue
		}
		if all && t.Context().Name() == r.Context().Name() || !all && t.Name() == r.Name() {
			if !rec.Complete {
				return nil, fmt.Errorf("image %s has lost layers: pull it again", image.Familiar(rec.Ref))
			}
			out = append(out, pushRecord{rec, t})
		}
	}
	if len(out) == 0 {
		if all {
			return nil, fmt.Errorf("An image does not exist locally with the repository: %s", image.Familiar(r.Context().Name()))
		}
		return nil, fmt.Errorf("An image does not exist locally with the tag: %s", image.Familiar(r.Name()))
	}
	slices.SortFunc(out, func(a, b pushRecord) int { return strings.Compare(a.tag.TagStr(), b.tag.TagStr()) })
	return out, nil
}
