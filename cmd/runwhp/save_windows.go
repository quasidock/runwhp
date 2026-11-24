//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/storefile"
	"github.com/quasidock/runwhp/internal/vmm"
)

const saveUsage = `Usage: runwhp save [-o FILE] IMAGE...

Write images to an archive, as Docker 29's docker save writes it, which
docker load, podman load and runwhp load take, with the images' IDs and
their layers' diffIDs as they were pulled. An image is named by its
reference, saved under it; by its ID, saved with no name; or by a
repository without a tag, saved under every tag of it in the store.

runwhp keeps each layer as an ext4 image and its tar's metadata, and save
rebuilds the tar from both, reading the files in a guest it boots for the
purpose. Layers pulled before runwhp kept that metadata get it from the
registry they came from, once.

  -o, --output FILE  write FILE rather than stdout
`

func cmdSave(args []string, stdout io.Writer) int {
	fs := newFlags("save", saveUsage)
	var output string
	var debug bool
	fs.StringVar(&output, "o", "", "")
	fs.StringVar(&output, "output", "", "")
	fs.BoolVar(&debug, "debug", false, "")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if f, ok := stdout.(*os.File); ok && output == "" && vmm.IsTerminal(f) {
		errorf("save: cowardly refusing to save to a terminal. Use the -o flag or redirect")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// A prune waits until the archive is written, and keeps its layers.
	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	is := image.Open(root)
	is.Progress = os.Stderr
	groups, err := resolveSave(is, fs.Args())
	if err != nil {
		errorf("save: %v", err)
		return 1
	}
	var imgs []image.SaveImage
	for _, g := range groups {
		if err := is.Backfill(ctx, g.recs); err != nil {
			errorf("save: %v", err)
			return 1
		}
		img := g.recs[0].Image
		raw, err := is.RawConfig(img)
		if err != nil {
			errorf("save: %v", err)
			return 1
		}
		cf, err := img.ConfigFile()
		if err != nil {
			errorf("save: %v", err)
			return 1
		}
		si := image.SaveImage{Refs: g.refs, Config: raw, ConfigDigest: img.ID()}
		for _, d := range cf.RootFS.DiffIDs {
			si.DiffIDs = append(si.DiffIDs, d.String())
		}
		imgs = append(imgs, si)
	}

	src := vmm.NewLayerServers(ctx, root, is.Layers, image.SaveLayers(imgs), debug)
	defer src.Close()
	write := func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 1<<20)
		if err := image.WriteArchive(bw, imgs, is.Layers, src); err != nil {
			return err
		}
		return bw.Flush()
	}
	if output == "" {
		err = write(stdout)
	} else {
		err = writeFileAtomically(output, write)
	}
	if err != nil {
		errorf("save: %v", err)
		return 1
	}
	return 0
}

// writeFileAtomically writes path through write, to a file beside it that
// takes its name once it is whole, and is removed if it is not.
func writeFileAtomically(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+storefile.TempSuffix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// saveGroup is one image to save: its records, and the references to save
// it under, none if it was named by ID alone.
type saveGroup struct {
	recs []*image.Record
	refs []string
}

// resolveSave finds the images args name, as docker save does: by
// reference, by ID, or by a repository alone, which names every tag of it.
func resolveSave(is *image.Store, args []string) ([]*saveGroup, error) {
	var groups []*saveGroup
	byID := map[string]*saveGroup{}
	add := func(rec *image.Record, ref string) error {
		if !rec.Complete {
			return fmt.Errorf("image %s has lost layers: pull it again", image.Familiar(rec.Ref))
		}
		g := byID[rec.ID()]
		if g == nil {
			g = &saveGroup{}
			byID[rec.ID()] = g
			groups = append(groups, g)
		}
		if !slices.Contains(g.recs, rec) {
			g.recs = append(g.recs, rec)
		}
		if ref != "" && !slices.Contains(g.refs, ref) {
			g.refs = append(g.refs, ref)
		}
		return nil
	}
	all, err := is.List()
	if err != nil {
		return nil, err
	}
	for _, arg := range args {
		if repo, ok := repositoryOnly(arg); ok {
			found := false
			for _, rec := range all {
				if r, err := name.ParseReference(rec.Ref); err == nil && rec.Image != nil && r.Context().Name() == repo {
					if _, isTag := r.(name.Tag); isTag {
						if err := add(rec, rec.Ref); err != nil {
							return nil, err
						}
						found = true
					}
				}
			}
			if found {
				continue
			}
		}
		recs, err := is.Find(arg)
		if err != nil {
			return nil, fmt.Errorf("no such image: %s", arg)
		}
		// One record whose reference arg names is saved under it; records
		// an ID names, under none, as Docker saves an image named by ID.
		ref := ""
		if r, err := name.ParseReference(arg); err == nil && len(recs) == 1 && recs[0].Ref == r.Name() {
			if _, isTag := r.(name.Tag); isTag {
				ref = recs[0].Ref
			}
		}
		for _, rec := range recs {
			if err := add(rec, ref); err != nil {
				return nil, err
			}
		}
	}
	// Any record of the image serves to fetch from its registry.
	for _, g := range groups {
		for _, rec := range all {
			if rec.Image != nil && rec.ID() == g.recs[0].ID() && !slices.ContainsFunc(g.recs, func(r *image.Record) bool { return r.Path == rec.Path }) {
				g.recs = append(g.recs, rec)
			}
		}
	}
	return groups, nil
}

// repositoryOnly reports whether arg names a repository without a tag or
// digest, and returns it normalized.
func repositoryOnly(arg string) (string, bool) {
	if strings.Contains(arg, "@") || strings.Contains(arg[strings.LastIndex(arg, "/")+1:], ":") {
		return "", false
	}
	r, err := name.NewRepository(arg)
	if err != nil {
		return "", false
	}
	return r.Name(), true
}
