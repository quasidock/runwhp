//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/cache"
	cacheconfig "github.com/moby/buildkit/cache/config"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/progress"
	"golang.org/x/sync/errgroup"
)

// mobyExporter is the exporter Docker's embedded BuildKit calls moby: it
// puts the image built in the daemon's store, runwhp's, and pushes it if
// asked. BuildKit's own image exporter commits and pushes it; the image is
// then loaded into runwhp's store, through the API, as an OCI layout.
type mobyExporter struct {
	image   exporter.Exporter
	content content.Store
	api     *dockerAPI
}

func (e *mobyExporter) Resolve(ctx context.Context, id int, attrs map[string]string) (exporter.ExporterInstance, error) {
	inner := maps.Clone(attrs)
	if inner == nil {
		inner = map[string]string{}
	}
	// What buildx asks of Docker's moby exporter with the containerd
	// image store, and runwhp's does anyway: the image's ID, its config's
	// digest, is what it answers.
	delete(inner, "prefer-image-digest")
	// BuildKit's own image store names nothing: runwhp's has the image.
	inner["store"] = "false"
	push, _ := strconv.ParseBool(inner[string(exptypes.OptKeyPush)])
	if _, ok := inner["compression"]; !ok && !push {
		// The layers go no further than runwhp's store, which keeps them
		// uncompressed: compressing them is time lost twice.
		inner["compression"] = "uncompressed"
	}
	in, err := e.image.Resolve(ctx, id, inner)
	if err != nil {
		return nil, err
	}
	return &mobyInstance{ExporterInstance: in, e: e, attrs: attrs}, nil
}

type mobyInstance struct {
	exporter.ExporterInstance
	e     *mobyExporter
	attrs map[string]string
}

func (i *mobyInstance) Type() string             { return mobyExporterName }
func (i *mobyInstance) Name() string             { return "exporting to image" }
func (i *mobyInstance) Attrs() map[string]string { return i.attrs }

func (i *mobyInstance) Export(ctx context.Context, src *exporter.Source, buildInfo exporter.ExportBuildInfo) (map[string]string, exporter.FinalizeFunc, exporter.DescriptorReference, error) {
	resp, finalize, ref, err := i.ExporterInstance.Export(ctx, src, buildInfo)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (map[string]string, exporter.FinalizeFunc, exporter.DescriptorReference, error) {
		if ref != nil {
			ref.Release()
		}
		return nil, nil, nil, err
	}
	desc := ref.Descriptor()

	// The image's ID in runwhp's store, as in Docker's without the
	// containerd image store: its linux/amd64 config's digest.
	config := resp[exptypes.ExporterImageConfigDigestKey]
	if config == "" {
		m, err := images.Manifest(ctx, i.e.content, desc, platforms.Only(localPlatform))
		if err != nil {
			return fail(fmt.Errorf("finding the image's configuration: %w", err))
		}
		config = m.Config.Digest.String()
		resp[exptypes.ExporterImageConfigDigestKey] = config
	}
	resp[exptypes.ExporterImageDigestKey] = config

	var names []string
	for _, n := range strings.Split(resp[exptypes.ExporterImageNameKey], ",") {
		// A dangling image's name is its digest: runwhp's store lists it
		// as <none>, as Docker's does.
		if n != "" && !strings.Contains(n, "@") {
			names = append(names, n)
		}
	}
	// An image the store has already, as a build from the cache makes
	// again, is only named.
	if info, err := i.e.api.inspect(ctx, config); err == nil && info.ID == config {
		named := progress.OneOff(ctx, "naming the image in runwhp's store")
		var err error
		for _, n := range names {
			if err = i.e.api.tag(ctx, config, n); err != nil {
				break
			}
		}
		if err := named(err); err != nil {
			return fail(fmt.Errorf("naming the image in runwhp's store: %w", err))
		}
		return resp, finalize, ref, nil
	}

	provider, err := i.provider(ctx, src, buildInfo)
	if err != nil {
		return fail(err)
	}
	loaded := progress.OneOff(ctx, "loading the image into runwhp's store")
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(archive.Export(ctx, provider, pw,
			archive.WithManifest(desc, names...), archive.WithAllPlatforms(), archive.WithSkipDockerManifest()))
	}()
	err = i.e.api.load(ctx, pr)
	pr.CloseWithError(err)
	if err := loaded(err); err != nil {
		return fail(fmt.Errorf("loading the image into runwhp's store: %w", err))
	}
	return resp, finalize, ref, nil
}

// provider gives the image's blobs, its layers fetched where the build
// left them lazy, as an image built FROM another, changing nothing of its
// layers, does.
func (i *mobyInstance) provider(ctx context.Context, src *exporter.Source, buildInfo exporter.ExportBuildInfo) (*contentutil.MultiProvider, error) {
	mprovider := contentutil.NewMultiProvider(i.e.content)
	var refs []cache.ImmutableRef
	if src.Ref != nil {
		refs = append(refs, src.Ref)
	}
	for _, r := range src.Refs {
		if r != nil {
			refs = append(refs, r)
		}
	}
	refCfg := cacheconfig.RefConfig{Compression: i.Config().Compression()}
	eg, egCtx := errgroup.WithContext(ctx)
	for _, r := range refs {
		eg.Go(func() error {
			remotes, err := r.GetRemotes(egCtx, false, refCfg, false, session.NewGroup(buildInfo.SessionID))
			if err != nil {
				return err
			}
			remote := remotes[0]
			if unlazier, ok := remote.Provider.(cache.Unlazier); ok {
				if err := unlazier.Unlazy(egCtx); err != nil {
					return err
				}
			}
			for _, d := range remote.Descriptors {
				mprovider.Add(d.Digest, remote.Provider)
			}
			return nil
		})
	}
	return mprovider, eg.Wait()
}
