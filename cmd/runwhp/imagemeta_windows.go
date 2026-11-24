//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/layer"
)

// hold takes the store's lock shared, for a command that adds records.
func hold() (string, func(), bool) {
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return "", nil, false
	}
	release, err := gc.Hold(root)
	if err != nil {
		errorf("%v", err)
		return "", nil, false
	}
	return root, release, true
}

// findImage finds the image arg names, by reference or ID, among complete
// records: one whose layers are all in the store.
func findImage(is *image.Store, arg string) (*image.Record, error) {
	found, err := is.Find(arg)
	if errors.Is(err, image.ErrNoSuchImage) {
		return nil, fmt.Errorf("no such image: %s", arg)
	} else if err != nil {
		return nil, err
	}
	for _, rec := range found {
		if rec.Complete {
			return rec, nil
		}
	}
	return nil, fmt.Errorf("image %s has lost layers: pull it again", arg)
}

// cmdTag runs runwhp tag SOURCE TARGET: it records the image SOURCE names
// under TARGET too, as Docker tags it, replacing whatever TARGET named. The
// new record has no registry digest: it was not pulled by that name.
func cmdTag(args []string) int {
	fs := imageFlags("tag")
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		return 2
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	target, err := name.ParseReference(dst)
	if err != nil {
		errorf("tag: %q is not a valid repository/tag: %v", dst, err)
		return 1
	}
	if _, ok := target.(name.Tag); !ok {
		errorf("tag: refusing to create a tag with a digest reference")
		return 1
	}
	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	is := image.Open(root)
	rec, err := findImage(is, src)
	if err != nil {
		errorf("tag: %v", err)
		return 1
	}
	if rec.Ref == target.Name() {
		return 0
	}
	img := *rec.Image
	img.Ref, img.Digest = target.Name(), ""
	if err := is.Put(dst, &img); err != nil {
		errorf("tag: %v", err)
		return 1
	}
	return 0
}

const historyUsage = `Usage: runwhp history [-H=false] [--no-trunc] [-q] [--format table|json] IMAGE

Show how an image was built, newest step first, from its configuration.
SIZE is what each step's layer holds: the size of its files, as Docker
gives it, or for a layer pulled before runwhp kept its tar's metadata, its
ext4 image's.

  -H, --human     sizes and times for people (default true); -H=false
                  gives bytes and RFC 3339 times
  --no-trunc      do not cut CREATED BY short, and give full IDs
  -q, --quiet     print only the IDs: the image's, then <missing>
  --format json   one JSON object per line, as Docker prints them
`

// historyRow is one step of an image's history.
type historyRow struct {
	ID        string
	Created   time.Time
	CreatedBy string
	Size      int64
	Comment   string
}

func cmdHistory(args []string, stdout io.Writer) int {
	fs := newFlags("history", historyUsage)
	var human, noTrunc, quiet bool
	var format string
	fs.BoolVar(&human, "H", true, "")
	fs.BoolVar(&human, "human", true, "")
	fs.BoolVar(&noTrunc, "no-trunc", false, "")
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.StringVar(&format, "format", "table", "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if format != "table" && format != "json" {
		errorf("history: unknown format %q; use table or json", format)
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	is := image.Open(root)
	rec, err := findImage(is, fs.Arg(0))
	if err != nil {
		errorf("history: %v", err)
		return 1
	}
	rows, err := history(is, rec.Image)
	if err != nil {
		errorf("history: %v", err)
		return 1
	}
	id := func(r historyRow) string {
		if r.ID == "" {
			return "<missing>"
		}
		if noTrunc {
			return r.ID
		}
		return shortID(r.ID)
	}
	createdBy := func(r historyRow) string {
		s := strings.ReplaceAll(r.CreatedBy, "\t", " ")
		if !noTrunc && len([]rune(s)) > 45 {
			s = string([]rune(s)[:44]) + "…"
		}
		return s
	}
	size := func(r historyRow) string {
		if human {
			return humanSize(r.Size)
		}
		return fmt.Sprint(r.Size)
	}
	now := time.Now()
	created := func(r historyRow) string {
		switch {
		case r.Created.IsZero():
			return "N/A"
		case human:
			return ago(now, r.Created)
		}
		return r.Created.Local().Format(time.RFC3339)
	}
	switch {
	case quiet:
		for _, r := range rows {
			fmt.Fprintln(stdout, id(r))
		}
	case format == "json":
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		for _, r := range rows {
			at := ""
			if !r.Created.IsZero() {
				at = r.Created.Local().Format(time.RFC3339)
			}
			enc.Encode(struct {
				Comment, CreatedAt, CreatedBy, CreatedSince, ID, Size string
			}{r.Comment, at, createdBy(r), created(r), id(r), size(r)})
		}
	default:
		tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
		when := "CREATED"
		if !human {
			when = "CREATED AT"
		}
		fmt.Fprintf(tw, "IMAGE\t%s\tCREATED BY\tSIZE\tCOMMENT\n", when)
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id(r), created(r), createdBy(r), size(r), r.Comment)
		}
		tw.Flush()
	}
	return 0
}

// history lists an image's steps, newest first, giving each step that
// made a layer that layer's size. An image whose configuration records no
// history gets a step per layer.
func history(is *image.Store, img *image.Image) ([]historyRow, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	layerSize := func(i int) int64 {
		if i >= len(cf.RootFS.DiffIDs) {
			return 0
		}
		d := cf.RootFS.DiffIDs[i].String()
		if d == layer.EmptyDiffID {
			return 0
		}
		if n, err := is.Layers.DiffSize(d); err == nil {
			return n
		}
		var n int64
		if p, err := is.Layers.Path(d); err == nil {
			n = fileSize(p)
		}
		return n
	}
	var rows []historyRow
	if len(cf.History) == 0 {
		for i := range cf.RootFS.DiffIDs {
			rows = append(rows, historyRow{Size: layerSize(i)})
		}
		if len(rows) == 0 {
			rows = append(rows, historyRow{})
		}
		rows[len(rows)-1].Created = cf.Created.Time
	} else {
		next := 0
		for _, h := range cf.History {
			r := historyRow{Created: h.Created.Time, CreatedBy: h.CreatedBy, Comment: h.Comment}
			if !h.EmptyLayer {
				r.Size = layerSize(next)
				next++
			}
			rows = append(rows, r)
		}
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	rows[0].ID = img.ID()
	return rows, nil
}

// fileSize is a file's size, or 0 if it is not there.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
