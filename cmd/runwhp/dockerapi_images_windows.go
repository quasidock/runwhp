//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/events"
	dimage "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/guestspec"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
	"github.com/quasidock/runwhp/internal/vmm"
)

func (s *apiServer) imageRoutes() {
	s.handle("POST /images/create", s.postImagesCreate)
	s.handle("GET /images/json", s.getImages)
	s.handle("POST /images/load", s.postImagesLoad)
	s.handle("GET /images/get", s.getImagesSave)
	s.handle("POST /images/prune", s.postImagesPrune)
	s.handle("POST /commit", s.postCommit)
	s.handle("POST /build", s.postBuild)
	s.handle("POST /build/prune", s.postBuildPrune)
	s.handle("POST /auth", s.postAuth)
	// An image's name has slashes in it: what follows it says what is
	// asked of it.
	s.handle("/images/", s.imageByName)
}

func (s *apiServer) imageByName(w http.ResponseWriter, r *http.Request) error {
	rest := strings.TrimPrefix(r.URL.Path, "/images/")
	if r.Method == http.MethodDelete {
		return s.deleteImage(w, r, rest)
	}
	for _, op := range []struct {
		suffix, method string
		h              func(http.ResponseWriter, *http.Request, string) error
	}{
		{"/json", http.MethodGet, s.getImage},
		{"/history", http.MethodGet, s.getImageHistory},
		{"/tag", http.MethodPost, s.postImageTag},
		{"/push", http.MethodPost, s.postImagePush},
		{"/get", http.MethodGet, func(w http.ResponseWriter, r *http.Request, n string) error {
			return s.saveImages(w, r, []string{n})
		}},
	} {
		if n, ok := strings.CutSuffix(rest, op.suffix); ok && n != "" {
			if r.Method != op.method {
				return &apiError{http.StatusMethodNotAllowed, "method not allowed"}
			}
			return op.h(w, r, n)
		}
	}
	return notFound("page not found")
}

// registryAuth reads a request's X-Registry-Auth header: base64url JSON of
// the credentials the client has for the registry. Nil if there are none.
func registryAuth(r *http.Request) (*registry.AuthConfig, error) {
	h := r.Header.Get("X-Registry-Auth")
	if h == "" {
		return nil, nil
	}
	data, err := base64.URLEncoding.DecodeString(h)
	if err != nil {
		if data, err = base64.RawURLEncoding.DecodeString(h); err != nil {
			if data, err = base64.StdEncoding.DecodeString(h); err != nil {
				return nil, badRequest("invalid X-Registry-Auth header: %v", err)
			}
		}
	}
	var ac registry.AuthConfig
	if err := json.Unmarshal(data, &ac); err != nil {
		return nil, badRequest("invalid X-Registry-Auth header: %v", err)
	}
	if ac.Username == "" && ac.Password == "" && ac.Auth == "" && ac.IdentityToken == "" && ac.RegistryToken == "" {
		return nil, nil
	}
	return &ac, nil
}

// clientKeychain is a client's credentials, for every registry; for the
// one the request names, as the docker CLI sends them.
type clientKeychain struct{ ac registry.AuthConfig }

func (k clientKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.FromConfig(authn.AuthConfig{Username: k.ac.Username, Password: k.ac.Password, Auth: k.ac.Auth,
		IdentityToken: k.ac.IdentityToken, RegistryToken: k.ac.RegistryToken}), nil
}

// withAuth has is speak to registries with the client's credentials, if it
// sent any, over those Docker's config.json gives.
func withAuth(is *image.Store, ac *registry.AuthConfig) {
	if ac != nil {
		is.RemoteOptions = append(is.RemoteOptions, remote.WithAuthFromKeychain(clientKeychain{*ac}))
	}
}

// pullRef is what an images/create request names: fromImage with tag,
// which may be a digest.
func pullRef(fromImage, tag string) string {
	switch {
	case tag == "":
		return fromImage
	case strings.HasPrefix(tag, "sha256:"):
		return fromImage + "@" + tag
	}
	return fromImage + ":" + tag
}

func shortDiffID(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	return d[:min(12, len(d))]
}

func (s *apiServer) postImagesCreate(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	if p := q.Get("platform"); p != "" && p != "linux" && p != "linux/amd64" {
		return badRequest("runwhp runs linux/amd64 images only, not %s", p)
	}
	if src := q.Get("fromSrc"); src != "" {
		return s.importImage(w, r, src)
	}
	from := q.Get("fromImage")
	if from == "" {
		return badRequest("fromImage or fromSrc is required")
	}
	ac, err := registryAuth(r)
	if err != nil {
		return err
	}
	ref := pullRef(from, q.Get("tag"))
	if _, err := name.ParseReference(ref); err != nil {
		return badRequest("invalid reference format: %v", err)
	}
	full, err := image.Normalize(ref)
	if err != nil {
		return badRequest("%v", err)
	}
	release, err := gcHold(s.root)
	if err != nil {
		return err
	}
	defer release()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	stream := dockerapi.NewStream(w, func() {
		if flusher != nil {
			flusher.Flush()
		}
	})
	n, _ := name.ParseReference(ref)
	repo := image.Familiar(n.Context().Name())
	id := q.Get("tag")
	if id == "" {
		id = n.Identifier()
	}
	stream.Status(id, "Pulling from "+strings.TrimPrefix(n.Context().RepositoryStr(), ""), nil)

	is := image.Open(s.root)
	withAuth(is, ac)
	var before string
	if old, err := is.Get(full); err == nil {
		before = old.Digest
	}
	var mu sync.Mutex
	is.Events = func(e image.PullEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Phase {
		case image.PullExists:
			stream.Status(shortDiffID(e.Layer), "Already exists", &jsonstream.Progress{})
		case image.PullWaiting:
			stream.Status(shortDiffID(e.Layer), "Pulling fs layer", &jsonstream.Progress{})
		case image.PullDownloading:
			stream.Status(shortDiffID(e.Layer), "Downloading", &jsonstream.Progress{Current: e.Current, Total: e.Total})
		case image.PullDone:
			stream.Status(shortDiffID(e.Layer), "Pull complete", &jsonstream.Progress{})
		case image.PullDigest:
			stream.Status("", "Digest: "+e.Digest, nil)
		}
	}
	img, err := is.Pull(r.Context(), ref)
	if err == nil {
		// Containers boot runwhp's kernel, which comes with the first pull.
		_, err = vmm.ResolveKernel(r.Context(), s.root, "", image.PullMissing)
	}
	if err != nil {
		stream.Error(err)
		return nil
	}
	shown := image.Familiar(img.Ref)
	if strings.Contains(ref, "@") {
		shown = repo + "@" + img.Digest
	}
	if before == img.Digest {
		stream.Status("", "Status: Image is up to date for "+shown, nil)
	} else {
		stream.Status("", "Status: Downloaded newer image for "+shown, nil)
	}
	s.emit(events.ImageEventType, events.ActionPull, shown, map[string]string{"name": shown})
	return nil
}

func (s *apiServer) importImage(w http.ResponseWriter, r *http.Request, src string) error {
	if src != "-" {
		return badRequest("runwhp imports from the request's body only (fromSrc=-), not %s: download it first", src)
	}
	q := r.URL.Query()
	ref := q.Get("repo")
	if ref != "" && q.Get("tag") != "" {
		ref += ":" + q.Get("tag")
	}
	tarStream, err := image.Decompress(r.Body)
	if err != nil {
		return badRequest("%v", err)
	}
	defer tarStream.Close()
	release, err := gcHold(s.root)
	if err != nil {
		return err
	}
	defer release()
	img, err := image.Open(s.root).Import(tarStream, ref, q["changes"], q.Get("message"), time.Now())
	if err != nil {
		return badRequest("%v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	stream := dockerapi.NewStream(w, nil)
	stream.Status("", img.ID(), nil)
	s.emit(events.ImageEventType, events.ActionImport, img.ID(), nil)
	return nil
}

func gcHold(root string) (func(), error) { return gc.Hold(root) }

// imageGroup is an image as Docker lists it: every record of one ID.
type imageGroup struct {
	id   string
	imgs []*gc.Image
}

// imageGroups groups the store's complete records by image ID, newest
// first.
func imageGroups(u *gc.Usage) []*imageGroup {
	byID := map[string]*imageGroup{}
	var out []*imageGroup
	for _, img := range u.Images {
		if !img.Complete {
			continue
		}
		g := byID[img.ID()]
		if g == nil {
			g = &imageGroup{id: img.ID()}
			byID[img.ID()] = g
			out = append(out, g)
		}
		g.imgs = append(g.imgs, img)
	}
	return out
}

// tagsAndDigests are an image's names and registry digests, as Docker
// lists them.
func tagsAndDigests(recs []*image.Record) (tags, digests []string) {
	tags, digests = []string{}, []string{}
	for _, rec := range recs {
		if rec.Image == nil || image.Untagged(rec.Ref) {
			continue
		}
		repo, _ := image.Display(rec.Ref)
		if t := image.Familiar(rec.Ref); !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
		if rec.Digest != "" && !slices.Contains(digests, repo+"@"+rec.Digest) {
			digests = append(digests, repo+"@"+rec.Digest)
		}
		for _, d := range rec.RepoDigests {
			if !slices.Contains(digests, d) {
				digests = append(digests, d)
			}
		}
	}
	sort.Strings(tags)
	return tags, digests
}

func (g *imageGroup) summary() dimage.Summary {
	recs := make([]*image.Record, len(g.imgs))
	containers := map[string]bool{}
	for i, img := range g.imgs {
		recs[i] = img.Record
		for _, c := range img.Containers {
			containers[c.ID] = true
		}
	}
	tags, digests := tagsAndDigests(recs)
	first := g.imgs[0]
	sum := dimage.Summary{
		ID: g.id, RepoTags: tags, RepoDigests: digests, Created: first.Created().Unix(),
		Size: first.Size, SharedSize: first.Shared, Containers: int64(len(containers)), Labels: map[string]string{},
	}
	if cf, err := first.ConfigFile(); err == nil && cf.Config.Labels != nil {
		sum.Labels = cf.Config.Labels
	}
	return sum
}

func (s *apiServer) getImages(w http.ResponseWriter, r *http.Request) error {
	m, err := dockerapi.DecodeFilters(r.URL.Query().Get("filters"))
	if err != nil {
		return badRequest("%v", err)
	}
	if err := dockerapi.CheckFilterKeys(m, "reference", "dangling", "label", "before", "since", "until"); err != nil {
		return badRequest("%v", err)
	}
	if len(m["until"]) > 0 {
		return badRequest("runwhp's image list takes no until filter yet")
	}
	// A legacy client's filter parameter is a reference.
	if v := r.URL.Query().Get("filter"); v != "" {
		m["reference"] = append(m["reference"], v)
	}
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	groups := imageGroups(u)
	created := func(ref string) (time.Time, error) {
		for _, g := range groups {
			for _, img := range g.imgs {
				if g.id == ref || strings.HasPrefix(strings.TrimPrefix(g.id, "sha256:"), ref) || image.Familiar(img.Ref) == ref ||
					image.Familiar(img.Ref) == ref+":latest" {
					return img.Created(), nil
				}
			}
		}
		return time.Time{}, notFound("No such image: %s", ref)
	}
	var before, since time.Time
	if v := m["before"]; len(v) > 0 {
		if before, err = created(v[0]); err != nil {
			return err
		}
	}
	if v := m["since"]; len(v) > 0 {
		if since, err = created(v[0]); err != nil {
			return err
		}
	}
	out := []dimage.Summary{}
	for _, g := range groups {
		sum := g.summary()
		t := time.Unix(sum.Created, 0)
		switch {
		case !before.IsZero() && !t.Before(before):
			continue
		case !since.IsZero() && !t.After(since):
			continue
		}
		if d := m["dangling"]; len(d) > 0 {
			dangling := len(sum.RepoTags) == 0
			if (d[0] == "true" || d[0] == "1") != dangling {
				continue
			}
		}
		if !filters(m).matchLabels(sum.Labels) {
			continue
		}
		if refs := m["reference"]; len(refs) > 0 {
			ok := false
			for _, pat := range refs {
				for _, tag := range sum.RepoTags {
					ok = ok || matchReference(pat, tag)
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, sum)
	}
	writeJSON(w, out)
	return nil
}

// matchReference matches a reference filter as Docker's does: a glob over
// the familiar name, with or without the tag.
func matchReference(pat, tag string) bool {
	if ok, _ := path.Match(pat, tag); ok {
		return true
	}
	repo := tag
	if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
		repo = tag[:i]
	}
	ok, _ := path.Match(pat, repo)
	return ok
}

// findImageRecords finds the complete records of the image ref names, as
// Docker's daemon words its absence.
func (s *apiServer) findImageRecords(ref string) ([]*image.Record, error) {
	found, err := image.Open(s.root).Find(ref)
	if errors.Is(err, image.ErrNoSuchImage) {
		return nil, notFound("No such image: %s", ref)
	}
	if err != nil {
		return nil, err
	}
	var complete []*image.Record
	for _, rec := range found {
		if rec.Complete {
			complete = append(complete, rec)
		}
	}
	if len(complete) == 0 {
		return nil, notFound("No such image: %s (its layers are gone: pull it again)", ref)
	}
	return complete, nil
}

func (s *apiServer) getImage(w http.ResponseWriter, r *http.Request, ref string) error {
	found, err := s.findImageRecords(ref)
	if err != nil {
		return err
	}
	// Every record of the image, for its names.
	all, _ := image.Open(s.root).Find(found[0].ID())
	tags, digests := tagsAndDigests(all)
	img := found[0].Image
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	resp := dimage.InspectResponse{
		ID: img.ID(), RepoTags: tags, RepoDigests: digests, Architecture: cf.Architecture, Os: cf.OS,
		Variant: cf.Variant, Author: cf.Author, RootFS: dimage.RootFS{Type: cf.RootFS.Type},
	}
	if !cf.Created.IsZero() {
		resp.Created = cf.Created.UTC().Format(time.RFC3339Nano)
	}
	for _, h := range cf.History {
		resp.Comment = h.Comment // the newest step's
	}
	for _, d := range cf.RootFS.DiffIDs {
		resp.RootFS.Layers = append(resp.RootFS.Layers, d.String())
	}
	var raw struct {
		Config dockerspec.DockerOCIImageConfig `json:"config"`
	}
	if err := json.Unmarshal(img.Config, &raw); err == nil {
		resp.Config = &raw.Config
	}
	if u, err := gc.Load(s.root); err == nil {
		for _, gi := range u.Images {
			if strings.EqualFold(gi.Path, found[0].Path) {
				resp.Size = gi.Size
			}
		}
	}
	resp.Metadata.LastTagTime = found[0].Pulled
	writeJSON(w, resp)
	return nil
}

func (s *apiServer) getImageHistory(w http.ResponseWriter, r *http.Request, ref string) error {
	found, err := s.findImageRecords(ref)
	if err != nil {
		return err
	}
	rows, err := history(image.Open(s.root), found[0].Image)
	if err != nil {
		return err
	}
	all, _ := image.Open(s.root).Find(found[0].ID())
	tags, _ := tagsAndDigests(all)
	out := make([]dimage.HistoryResponseItem, 0, len(rows))
	for i, row := range rows {
		item := dimage.HistoryResponseItem{ID: "<missing>", Created: row.Created.Unix(), CreatedBy: row.CreatedBy, Size: row.Size,
			Comment: row.Comment, Tags: nil}
		if i == 0 {
			item.ID, item.Tags = row.ID, tags
		}
		out = append(out, item)
	}
	writeJSON(w, out)
	return nil
}

func (s *apiServer) postImageTag(w http.ResponseWriter, r *http.Request, src string) error {
	q := r.URL.Query()
	repo, tag := q.Get("repo"), q.Get("tag")
	if repo == "" {
		return badRequest("repository name (repo) is required")
	}
	if tag == "" {
		tag = "latest"
	}
	dst := repo + ":" + tag
	target, err := name.NewTag(dst)
	if err != nil {
		return badRequest("invalid reference format: %v", err)
	}
	release, err := gcHold(s.root)
	if err != nil {
		return err
	}
	defer release()
	found, err := s.findImageRecords(src)
	if err != nil {
		return err
	}
	rec := found[0]
	if rec.Ref != target.Name() {
		img := *rec.Image
		img.Ref, img.Digest = target.Name(), ""
		if err := image.Open(s.root).Put(dst, &img); err != nil {
			return err
		}
	}
	s.emit(events.ImageEventType, events.ActionTag, rec.ID(), map[string]string{"name": image.Familiar(target.Name())})
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *apiServer) deleteImage(w http.ResponseWriter, r *http.Request, ref string) error {
	if _, err := s.findImageRecords(ref); err != nil {
		return err
	}
	release, err := gc.Exclusive(s.root, func() {})
	if err != nil {
		return err
	}
	defer release()
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	p, errs := u.RemoveImages([]string{ref}, boolParam(r, "force"))
	if len(errs) > 0 {
		if errors.Is(errs[0], gc.ErrConflict) {
			return conflict("conflict: unable to remove repository reference %q (must force) - %v", ref, errs[0])
		}
		return errs[0]
	}
	rep := u.Apply(p)
	out := []dimage.DeleteResponse{}
	for _, t := range rep.Untagged {
		out = append(out, dimage.DeleteResponse{Untagged: t})
		s.emit(events.ImageEventType, events.ActionUnTag, t, map[string]string{"name": t})
	}
	for _, d := range rep.Deleted {
		out = append(out, dimage.DeleteResponse{Deleted: d})
		s.emit(events.ImageEventType, events.ActionDelete, d, nil)
	}
	if len(rep.Errors) > 0 {
		s.logf("rmi %s: %v", ref, errors.Join(rep.Errors...))
	}
	writeJSON(w, out)
	return nil
}

func (s *apiServer) postImagesPrune(w http.ResponseWriter, r *http.Request) error {
	m, err := dockerapi.DecodeFilters(r.URL.Query().Get("filters"))
	if err != nil {
		return badRequest("%v", err)
	}
	if err := dockerapi.CheckFilterKeys(m, "dangling", "label", "label!", "until"); err != nil {
		return badRequest("%v", err)
	}
	if len(m["until"]) > 0 || len(m["label"]) > 0 || len(m["label!"]) > 0 {
		return badRequest("runwhp's image prune takes only the dangling filter yet")
	}
	all := false
	if d := m["dangling"]; len(d) > 0 && (d[0] == "false" || d[0] == "0") {
		all = true
	}
	release, err := gc.Exclusive(s.root, func() {})
	if err != nil {
		return err
	}
	defer release()
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	p := u.Dangling()
	if all {
		p = u.Unused()
	}
	rep := u.Apply(p)
	out := dimage.PruneReport{ImagesDeleted: []dimage.DeleteResponse{}, SpaceReclaimed: uint64(max(rep.Freed, 0))}
	for _, t := range rep.Untagged {
		out.ImagesDeleted = append(out.ImagesDeleted, dimage.DeleteResponse{Untagged: t})
	}
	for _, d := range rep.Deleted {
		out.ImagesDeleted = append(out.ImagesDeleted, dimage.DeleteResponse{Deleted: d})
	}
	writeJSON(w, out)
	return nil
}

func (s *apiServer) postImagesLoad(w http.ResponseWriter, r *http.Request) error {
	f, err := spool(s.root, r.Body)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	a, err := image.OpenArchive(f)
	if err != nil {
		return badRequest("%v", err)
	}
	release, err := gcHold(s.root)
	if err != nil {
		return err
	}
	defer release()
	loaded, err := image.Open(s.root).Load(a)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	stream := dockerapi.NewStream(w, nil)
	for _, l := range loaded {
		for _, ref := range l.Refs {
			stream.Text("Loaded image: " + image.Familiar(ref) + "\n")
		}
		if len(l.Refs) == 0 {
			stream.Text("Loaded image ID: " + l.ID + "\n")
		}
		s.emit(events.ImageEventType, events.ActionLoad, l.ID, nil)
	}
	if err != nil {
		stream.Error(err)
	}
	return nil
}

// The jobs that boot guests: save, push, commit and build.

func init() {
	apiJobs["save"] = jobSave
	apiJobs["push"] = jobPush
	apiJobs["commit"] = jobCommit
}

func (s *apiServer) getImagesSave(w http.ResponseWriter, r *http.Request) error {
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		return badRequest("no image names given")
	}
	return s.saveImages(w, r, names)
}

func (s *apiServer) saveImages(w http.ResponseWriter, r *http.Request, names []string) error {
	for _, n := range names {
		if _, err := s.findImageRecords(n); err != nil {
			return err
		}
	}
	started := false
	_, err := s.jobs.run(r.Context(), "save", names, nil, writerFunc(func(p []byte) (int, error) {
		if !started {
			w.Header().Set("Content-Type", "application/x-tar")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		return w.Write(p)
	}), nil)
	if err != nil && started {
		s.logf("save %v ended early: %v", names, err)
		return nil
	}
	if err == nil && !started {
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
	}
	return err
}

func jobSave(ctx context.Context, root string, args json.RawMessage, _ io.Reader, out jobSink) (any, error) {
	var names []string
	if err := json.Unmarshal(args, &names); err != nil {
		return nil, err
	}
	release, err := gc.Hold(root)
	if err != nil {
		return nil, err
	}
	defer release()
	is := image.Open(root)
	is.Progress = os.Stderr
	return nil, writeSave(ctx, root, is, names, out)
}

// writeSave writes the images names names to w, as save does.
func writeSave(ctx context.Context, root string, is *image.Store, names []string, w io.Writer) error {
	groups, err := resolveSave(is, names)
	if err != nil {
		return err
	}
	var imgs []image.SaveImage
	for _, g := range groups {
		if err := is.Backfill(ctx, g.recs); err != nil {
			return err
		}
		img := g.recs[0].Image
		raw, err := is.RawConfig(img)
		if err != nil {
			return err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return err
		}
		si := image.SaveImage{Refs: g.refs, Config: raw, ConfigDigest: img.ID()}
		for _, d := range cf.RootFS.DiffIDs {
			si.DiffIDs = append(si.DiffIDs, d.String())
		}
		imgs = append(imgs, si)
	}
	src := vmm.NewLayerServers(ctx, root, is.Layers, image.SaveLayers(imgs), false)
	defer src.Close()
	return image.WriteArchive(w, imgs, is.Layers, src)
}

type pushArgs struct {
	Ref  string               `json:"ref"`
	Auth *registry.AuthConfig `json:"auth,omitempty"`
}

func (s *apiServer) postImagePush(w http.ResponseWriter, r *http.Request, n string) error {
	ac, err := registryAuth(r)
	if err != nil {
		return err
	}
	ref := n
	if tag := r.URL.Query().Get("tag"); tag != "" {
		ref = n + ":" + tag
	}
	if _, err := s.findImageRecords(ref); err != nil {
		if r.URL.Query().Get("tag") == "" {
			return badRequest("runwhp pushes one tag at a time: name it, as in docker push %s:latest", n)
		}
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	stream := dockerapi.NewStream(w, func() {
		if flusher != nil {
			flusher.Flush()
		}
	})
	_, err = s.jobs.run(r.Context(), "push", pushArgs{Ref: ref, Auth: ac}, nil, nil, func(line []byte) error {
		_, err := w.Write(append(line, '\r', '\n'))
		if flusher != nil {
			flusher.Flush()
		}
		return err
	})
	if err != nil {
		stream.Error(err)
		return nil
	}
	s.emit(events.ImageEventType, events.ActionPush, image.Familiar(ref), map[string]string{"name": image.Familiar(ref)})
	return nil
}

// messageStream writes JSON messages through a job's sink.
func messageStream(out jobSink) *dockerapi.Stream {
	return dockerapi.NewStream(lineSink{out}, nil)
}

// lineSink takes a Stream's lines, one message each, without their line
// ends.
type lineSink struct{ out jobSink }

func (l lineSink) Write(p []byte) (int, error) {
	if err := l.out.Message(trimLineEnd(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func trimLineEnd(p []byte) []byte {
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	return p
}

func jobPush(ctx context.Context, root string, args json.RawMessage, _ io.Reader, out jobSink) (any, error) {
	var a pushArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	release, err := gc.Hold(root)
	if err != nil {
		return nil, err
	}
	defer release()
	is := image.Open(root)
	withAuth(is, a.Auth)
	recs, err := pushRecords(is, a.Ref, false)
	if err != nil {
		return nil, err
	}
	stream := messageStream(out)
	repo := image.Familiar(recs[0].tag.Context().Name())
	stream.Status("", "The push refers to repository ["+repo+"]", nil)
	tmp := root + `\tmp`
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if err := is.Backfill(ctx, []*image.Record{rec.rec}); err != nil {
			return nil, err
		}
		raw, err := is.RawConfig(rec.rec.Image)
		if err != nil {
			return nil, err
		}
		layers, err := rec.rec.Layers()
		if err != nil {
			return nil, err
		}
		var order []string
		for _, d := range layers {
			if d != layer.EmptyDiffID {
				order = append(order, d)
			}
			stream.Status(shortDiffID(d), "Preparing", &jsonstream.Progress{})
		}
		src := vmm.NewLayerServers(ctx, root, is.Layers, order, false)
		var mu sync.Mutex
		pushed, err := is.Push(ctx, rec.tag, raw, src, tmp, func(diffID, status string) {
			mu.Lock()
			defer mu.Unlock()
			stream.Status(shortDiffID(diffID), status, &jsonstream.Progress{})
		})
		src.Close()
		if err != nil {
			return nil, err
		}
		stream.Status("", fmt.Sprintf("%s: digest: %s size: %d", rec.tag.TagStr(), pushed.Digest, pushed.Size), nil)
		stream.Aux(map[string]any{"Tag": rec.tag.TagStr(), "Digest": pushed.Digest, "Size": pushed.Size})
		rd := repo + "@" + pushed.Digest
		if !slices.Contains(rec.rec.RepoDigests, rd) {
			rec.rec.RepoDigests = append(rec.rec.RepoDigests, rd)
			if err := is.Put(rec.rec.Ref, rec.rec.Image); err != nil {
				fmt.Fprintf(os.Stderr, "push: recording the digest: %v\n", err)
			}
		}
	}
	return nil, nil
}

type commitArgs struct {
	fileJobArgs
	Repo    string   `json:"repo,omitempty"`
	Changes []string `json:"changes,omitempty"`
	Author  string   `json:"author,omitempty"`
	Comment string   `json:"comment,omitempty"`
	Pause   bool     `json:"pause"`
}

func (s *apiServer) postCommit(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	c, err := s.resolve(q.Get("container"))
	if err != nil {
		return err
	}
	ref := q.Get("repo")
	if ref != "" && q.Get("tag") != "" {
		ref += ":" + q.Get("tag")
	}
	if strings.Contains(ref, "@") {
		return badRequest("a reference with a digest cannot be committed to")
	}
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		return err
	}
	for k, v := range body {
		if v != nil && !isZeroJSON(v) {
			return badRequest("runwhp commits take changes (-c), not a configuration: %s was given", k)
		}
	}
	pause := true
	if v := q.Get("pause"); v != "" {
		pause = boolParam(r, "pause")
	}
	a := commitArgs{fileJobArgs: fileJobArgs{Ref: c.ID}, Repo: ref, Changes: q["changes"], Author: q.Get("author"),
		Comment: q.Get("comment"), Pause: pause}
	// As a file job: here for a container with a VM, in a job for one
	// without.
	local := a
	local.PipeOnly = true
	argData, _ := json.Marshal(local)
	v, err := jobCommit(r.Context(), s.root, argData, nil, localSink{})
	var res json.RawMessage
	switch {
	case errors.Is(err, errNeedsGuest):
		res, err = s.jobs.run(r.Context(), "commit", a, nil, nil, nil)
	case err == nil:
		res, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(append(res, '\n'))
	var id struct{ ID string }
	json.Unmarshal(res, &id)
	s.emitContainer(c, events.ActionCommit, map[string]string{"comment": a.Comment, "imageID": id.ID})
	return nil
}

func isZeroJSON(v any) bool {
	switch x := v.(type) {
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if e != nil && !isZeroJSON(e) {
				return false
			}
		}
		return true
	}
	return false
}

func jobCommit(ctx context.Context, root string, args json.RawMessage, _ io.Reader, _ jobSink) (any, error) {
	var a commitArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	release, err := gc.Hold(root)
	if err != nil {
		return nil, err
	}
	defer release()
	rc, err := openJobRoot(ctx, root, a.fileJobArgs)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	c := rc.c
	is := image.Open(root)
	commit, err := commitOf(is, c, a.Changes, a.Author, a.Comment)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if a.Pause && !rc.booted && c.State.Status == container.Running && !c.State.Paused {
		if err := askPause(root, c, container.OpPause); err != nil {
			return nil, err
		}
		defer askPause(root, c, container.OpUnpause)
	}
	diff, err := rc.Tar(ctx, guestspec.RootDiff)
	if err != nil {
		return nil, err
	}
	defer diff.Close()
	img, err := is.Commit(commit, diff, a.Repo)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Id": img.ID()}, nil
}

func (s *apiServer) postAuth(w http.ResponseWriter, r *http.Request) error {
	var ac registry.AuthConfig
	if err := readJSON(r, &ac); err != nil {
		return err
	}
	server := ac.ServerAddress
	if server == "" || strings.Contains(server, "index.docker.io") {
		server = name.DefaultRegistry
	}
	server = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://"), "/")
	if i := strings.Index(server, "/"); i >= 0 {
		server = server[:i]
	}
	if err := checkLogin(r.Context(), server, ac); err != nil {
		return &apiError{http.StatusUnauthorized, err.Error()}
	}
	writeJSON(w, registry.AuthResponse{Status: "Login Succeeded"})
	return nil
}
