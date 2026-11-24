//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/image"
)

const imageUsage = `Usage:
  runwhp image ls [-q] [--digests] [--format table|json] [REPOSITORY[:TAG]]   list images (also: runwhp images)
  runwhp image inspect IMAGE...                                             describe images as JSON
  runwhp image rm [-f] IMAGE...                                             remove images (also: runwhp rmi)
  runwhp image prune [-a] [-f]                                              remove dangling images, or with -a unused ones
  runwhp image tag SOURCE TARGET                                            name an image anew (also: runwhp tag)
  runwhp image history [-H=false] [--no-trunc] [-q] [--format table|json] IMAGE   show how an image was built (also: runwhp history)
  runwhp image load [-i FILE] [-q]                                           load images from an archive (also: runwhp load)
  runwhp image import [-c CHANGE]... [-m MESSAGE] FILE|- [REPOSITORY[:TAG]]   make an image from a root file system tar (also: runwhp import)
  runwhp image save [-o FILE] IMAGE...                                       write images to an archive docker load takes (also: runwhp save)

An image is named by its reference, as pulled, or by its ID or a unique
prefix of it. Its layers are shared with other images and with containers
made from it, and go once nothing lists them: a container keeps the layers
it was made from, even once its image is removed or pulled again.
`

// cmdImage runs runwhp image. It returns 1 when a command fails and 2 for
// a usage error, as volume does.
func cmdImage(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, imageUsage)
		return 2
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return cmdImages(args, stdout)
	case "rm", "remove":
		return cmdRmi(args, stdout)
	case "inspect":
		return cmdImageInspect(args, stdout)
	case "tag":
		return cmdTag(args)
	case "history":
		return cmdHistory(args, stdout)
	case "load":
		return cmdLoad(args, stdin, stdout)
	case "import":
		return cmdImport(args, stdin, stdout)
	case "save":
		return cmdSave(args, stdout)
	case "prune":
		return cmdImagePrune(args, stdin, stdout)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, imageUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown image command %q\n\n%s", sub, imageUsage)
	return 2
}

func imageFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), imageUsage) }
	return fs
}

// loadStore reads what the store holds, reporting a failure.
func loadStore() (string, *gc.Usage, bool) {
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return "", nil, false
	}
	u, err := gc.Load(root)
	if err != nil {
		errorf("%v", err)
		return "", nil, false
	}
	return root, u, true
}

// shortID is an image ID as Docker lists it: its first 12 hex digits.
func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(12, len(id))]
}

// imageRow is an image as images --format json prints it.
type imageRow struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Tag        string    `json:"tag"`
	Reference  string    `json:"reference"` // normalized, as pulled
	Digest     string    `json:"digest"`
	Created    time.Time `json:"created,omitzero"`
	Pulled     time.Time `json:"pulled"`
	Size       int64     `json:"size"`
	SharedSize int64     `json:"sharedSize"`
	Containers int       `json:"containers"`
	Complete   bool      `json:"complete"` // every layer is in the store
}

func cmdImages(args []string, stdout io.Writer) int {
	fs := imageFlags("images")
	var quiet, digests bool
	var format string
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.BoolVar(&digests, "digests", false, "")
	fs.StringVar(&format, "format", "table", "")
	if fs.Parse(args) != nil || fs.NArg() > 1 {
		return 2
	}
	if format != "table" && format != "json" {
		errorf("images: unknown format %q; use table or json", format)
		return 2
	}
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	var rows []imageRow
	for _, img := range u.Images {
		repo, tag := image.Display(img.Ref)
		if fs.NArg() == 1 && !matchRepo(fs.Arg(0), repo, tag) {
			continue
		}
		rows = append(rows, imageRow{
			ID: img.ID(), Repository: repo, Tag: tag, Reference: img.Ref, Digest: img.Digest,
			Created: img.Created(), Pulled: img.Pulled, Size: img.Size, SharedSize: img.Shared,
			Containers: len(img.Containers), Complete: img.Complete,
		})
	}
	switch {
	case quiet:
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.ID] {
				seen[r.ID] = true
				fmt.Fprintln(stdout, shortID(r.ID))
			}
		}
	case format == "json":
		if rows == nil {
			rows = []imageRow{}
		}
		printJSON(stdout, rows)
	default:
		now := time.Now()
		tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
		if digests {
			fmt.Fprintln(tw, "REPOSITORY\tTAG\tDIGEST\tIMAGE ID\tCREATED\tSIZE")
		} else {
			fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE")
		}
		for _, r := range rows {
			created := "N/A"
			if !r.Created.IsZero() {
				created = ago(now, r.Created)
			}
			size := humanSize(r.Size)
			if !r.Complete {
				size += " (layers missing)"
			}
			if digests {
				// A tag, a load or an import gives no registry digest.
				digest := r.Digest
				if digest == "" {
					digest = "<none>"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Repository, r.Tag, digest, shortID(r.ID), created, size)
			} else {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Repository, r.Tag, shortID(r.ID), created, size)
			}
		}
		tw.Flush()
	}
	return 0
}

// matchRepo reports whether an image listed as repo and tag matches the
// argument images was given: a repository, with a tag or without.
func matchRepo(arg, repo, tag string) bool {
	wantRepo, wantTag := arg, ""
	if i := strings.LastIndex(arg, ":"); i > strings.LastIndex(arg, "/") {
		wantRepo, wantTag = arg[:i], arg[i+1:]
	}
	// alpine, library/alpine and docker.io/library/alpine alike.
	if r, err := name.NewRepository(wantRepo); err == nil {
		wantRepo = image.Familiar(r.Name())
	}
	return repo == wantRepo && (wantTag == "" || tag == wantTag)
}

func cmdRmi(args []string, stdout io.Writer) int {
	fs := imageFlags("rmi")
	var force bool
	fs.BoolVar(&force, "f", false, "")
	fs.BoolVar(&force, "force", false, "")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	release, err := exclusive(root)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer release()
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	p, errs := u.RemoveImages(fs.Args(), force)
	r := u.Apply(p)
	for _, ref := range r.Untagged {
		fmt.Fprintf(stdout, "Untagged: %s\n", ref)
	}
	for _, d := range r.Deleted {
		fmt.Fprintf(stdout, "Deleted: %s\n", d)
	}
	for _, err := range errs {
		errorf("%v", err)
	}
	if printReport(r) > 0 || len(errs) > 0 {
		return 1
	}
	return 0
}

// imageInfo is an image as image inspect describes it, in the shape of
// docker image inspect's.
type imageInfo struct {
	Id           string
	RepoTags     []string
	RepoDigests  []string
	Created      time.Time `json:",omitzero"`
	Pulled       time.Time
	Size         int64
	Os           string
	Architecture string
	Config       any
	RootFS       struct {
		Type   string
		Layers []string
	}
	Containers []string // the IDs of the containers made from it
}

func cmdImageInspect(args []string, stdout io.Writer) int {
	fs := imageFlags("image inspect")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	byPath := map[string]*gc.Image{}
	for _, img := range u.Images {
		byPath[strings.ToLower(img.Path)] = img
	}
	out := []imageInfo{}
	code := 0
	for _, arg := range fs.Args() {
		found, err := image.Open(u.Root).Find(arg)
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		info := imageInfo{Id: found[0].ID()}
		for _, rec := range found {
			if image.Untagged(rec.Ref) {
				continue
			}
			repo, _ := image.Display(rec.Ref)
			info.RepoTags = append(info.RepoTags, image.Familiar(rec.Ref))
			// A tag, a load or an import gives no registry digest.
			if rec.Digest != "" {
				info.RepoDigests = append(info.RepoDigests, repo+"@"+rec.Digest)
			}
		}
		img := byPath[strings.ToLower(found[0].Path)]
		if img == nil {
			errorf("image %s changed while it was read", arg)
			code = 1
			continue
		}
		cf, err := img.ConfigFile()
		if err != nil {
			errorf("%v", err)
			code = 1
			continue
		}
		info.Created, info.Pulled, info.Size = img.Created(), img.Pulled, img.Size
		info.Os, info.Architecture, info.Config = cf.OS, cf.Architecture, cf.Config
		info.RootFS.Type = cf.RootFS.Type
		for _, h := range cf.RootFS.DiffIDs {
			info.RootFS.Layers = append(info.RootFS.Layers, h.String())
		}
		for _, rec := range found {
			if other := byPath[strings.ToLower(rec.Path)]; other != nil {
				for _, c := range other.Containers {
					info.Containers = append(info.Containers, c.ID)
				}
			}
		}
		out = append(out, info)
	}
	printJSON(stdout, out)
	return code
}

func cmdImagePrune(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := imageFlags("image prune")
	var all, force bool
	fs.BoolVar(&all, "a", false, "")
	fs.BoolVar(&all, "all", false, "")
	fs.BoolVar(&force, "f", false, "")
	fs.BoolVar(&force, "force", false, "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	warning := "This will remove all dangling images: layers no image or container lists, and images whose layers are gone."
	if all {
		warning = "This will remove all images without at least one container associated to them."
	}
	if !force && !confirm(stdin, warning) {
		return 0
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	release, err := exclusive(root)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer release()
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	p := u.Dangling()
	if all {
		p = u.Unused()
	}
	r := u.Apply(p)
	printSection(stdout, "Deleted Images:", imageLines(r))
	fmt.Fprintf(stdout, "Total reclaimed space: %s\n", humanSize(r.Freed))
	if printReport(r) > 0 {
		return 1
	}
	return 0
}

// imageLines are a prune's removed images and layers, as docker's prunes
// list them.
func imageLines(r *gc.Report) []string {
	var out []string
	for _, ref := range r.Untagged {
		out = append(out, "untagged: "+ref)
	}
	for _, d := range r.Deleted {
		out = append(out, "deleted: "+d)
	}
	return out
}
