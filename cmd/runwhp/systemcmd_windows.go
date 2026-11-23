//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/image"
)

const systemUsage = `Usage:
  runwhp system df [-v] [--format table|json]     show the space the store takes, and what a prune would free
  runwhp system prune [-a] [--volumes] [-f]       remove what nothing uses

system prune removes stopped and created containers, dangling images (with
-a, every image no container uses), kernels this runwhp does not boot, and
temporary files. With --volumes it also removes every volume no container
uses, and the data in it: runwhp's anonymous volumes go with their
containers, so it removes named ones, unlike docker system prune --volumes.
`

// cmdSystem runs runwhp system. It returns 1 when a command fails and 2 for
// a usage error.
func cmdSystem(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, systemUsage)
		return 2
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "df":
		return cmdSystemDf(args, stdout)
	case "prune":
		return cmdSystemPrune(args, stdin, stdout)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, systemUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "runwhp: unknown system command %q\n\n%s", sub, systemUsage)
	return 2
}

// dfRow is one line of system df's summary.
type dfRow struct {
	Type        string `json:"type"`
	Total       int    `json:"total"`
	Active      int    `json:"active"`
	Size        int64  `json:"size"`
	Reclaimable int64  `json:"reclaimable"`
}

// summarize works out system df's summary: what each kind of thing in the
// store takes, and what pruning would free of it.
func summarize(u *gc.Usage) []dfRow {
	images := dfRow{Type: "Images", Total: len(u.Images)}
	for _, img := range u.Images {
		if len(img.Containers) > 0 {
			images.Active++
		}
	}
	for _, l := range u.Layers {
		images.Size += l.Size
	}
	for _, l := range u.Unused().Layers {
		images.Reclaimable += l.Size
	}
	containers := dfRow{Type: "Containers", Total: len(u.Containers)}
	for _, c := range u.Containers {
		containers.Size += c.Size
		if gc.Removable(c.Container) {
			containers.Reclaimable += c.Size
		} else {
			containers.Active++
		}
	}
	volumes := dfRow{Type: "Local Volumes", Total: len(u.Volumes)}
	for _, v := range u.Volumes {
		volumes.Size += v.Size
		if len(v.Users) > 0 {
			volumes.Active++
		} else {
			volumes.Reclaimable += v.Size
		}
	}
	kernels := dfRow{Type: "Kernels", Total: len(u.Kernels)}
	for _, k := range u.Kernels {
		kernels.Size += k.Size
		if k.Kept {
			kernels.Active++
		} else {
			kernels.Reclaimable += k.Size
		}
	}
	return []dfRow{images, containers, volumes, kernels}
}

// reclaimable writes a reclaimable size with its share of the whole, as
// docker system df does.
func reclaimable(r dfRow) string {
	pct := 0
	if r.Size > 0 {
		pct = int(100 * r.Reclaimable / r.Size)
	}
	return fmt.Sprintf("%s (%d%%)", humanSize(r.Reclaimable), pct)
}

func cmdSystemDf(args []string, stdout io.Writer) int {
	fs := newFlags("system df", systemUsage)
	var verbose bool
	var format string
	fs.BoolVar(&verbose, "v", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	fs.StringVar(&format, "format", "table", "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	if format != "table" && format != "json" {
		errorf("system df: unknown format %q; use table or json", format)
		return 2
	}
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	rows := summarize(u)
	if format == "json" {
		printJSON(stdout, dfJSON(u, rows))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", r.Type, r.Total, r.Active, humanSize(r.Size), reclaimable(r))
	}
	tw.Flush()
	if verbose {
		dfVerbose(stdout, u)
	}
	return 0
}

// dfVerbose writes system df -v's tables, one for each kind of thing.
func dfVerbose(w io.Writer, u *gc.Usage) {
	now := time.Now()
	fmt.Fprint(w, "\nImages space usage:\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE\tSHARED SIZE\tUNIQUE SIZE\tCONTAINERS")
	for _, img := range u.Images {
		repo, tag := image.Display(img.Ref)
		created := "N/A"
		if c := img.Created(); !c.IsZero() {
			created = ago(now, c)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", repo, tag, shortID(img.ID()), created,
			humanSize(img.Size), humanSize(img.Shared), humanSize(img.Size-img.Shared), len(img.Containers))
	}
	tw.Flush()
	var loose, kept int
	var looseSize, keptSize int64
	for _, l := range u.Layers {
		if len(l.Images) > 0 {
			continue
		}
		if len(l.Containers) > 0 {
			kept++
			keptSize += l.Size
		} else {
			loose++
			looseSize += l.Size
		}
	}
	if loose+kept > 0 {
		fmt.Fprintf(w, "\nLayers no image lists: %d kept by containers, %s; %d dangling, %s\n", kept, humanSize(keptSize), loose, humanSize(looseSize))
	}

	fmt.Fprint(w, "\nContainers space usage:\n\n")
	tw = tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tSIZE\tCREATED\tSTATUS\tNAMES")
	for _, c := range u.Containers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Short(), imageOf(c.Config), commandOf(c.Config, 20),
			humanSize(c.Size), ago(now, c.Config.Created), statusText(c.State, now), c.Name)
	}
	tw.Flush()

	fmt.Fprint(w, "\nLocal Volumes space usage:\n\n")
	tw = tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "VOLUME NAME\tLINKS\tSIZE")
	for _, v := range u.Volumes {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", v.Name, len(v.Users), humanSize(v.Size))
	}
	tw.Flush()

	fmt.Fprint(w, "\nKernels space usage:\n\n")
	tw = tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "DIGEST\tSIZE\tKEPT")
	for _, k := range u.Kernels {
		fmt.Fprintf(tw, "%s\t%s\t%v\n", shortID(k.Digest), humanSize(k.Size), k.Kept)
	}
	tw.Flush()
}

// dfJSON is system df's JSON: the summary and every row of -v's tables.
func dfJSON(u *gc.Usage, rows []dfRow) any {
	type img struct {
		ID         string `json:"id"`
		Reference  string `json:"reference"`
		Size       int64  `json:"size"`
		SharedSize int64  `json:"sharedSize"`
		Containers int    `json:"containers"`
	}
	type ctr struct {
		ID     string `json:"id"`
		Name   string `json:"name,omitempty"`
		Image  string `json:"image"`
		Status string `json:"status"`
		Size   int64  `json:"size"`
	}
	type vol struct {
		Name  string `json:"name"`
		Links int    `json:"links"`
		Size  int64  `json:"size"`
	}
	type ker struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
		Kept   bool   `json:"kept"`
	}
	out := struct {
		Summary    []dfRow `json:"summary"`
		Images     []img   `json:"images"`
		Containers []ctr   `json:"containers"`
		Volumes    []vol   `json:"volumes"`
		Kernels    []ker   `json:"kernels"`
	}{Summary: rows, Images: []img{}, Containers: []ctr{}, Volumes: []vol{}, Kernels: []ker{}}
	for _, i := range u.Images {
		out.Images = append(out.Images, img{i.ID(), i.Ref, i.Size, i.Shared, len(i.Containers)})
	}
	for _, c := range u.Containers {
		out.Containers = append(out.Containers, ctr{c.ID, c.Name, imageOf(c.Config), string(c.State.Status), c.Size})
	}
	for _, v := range u.Volumes {
		out.Volumes = append(out.Volumes, vol{v.Name, len(v.Users), v.Size})
	}
	for _, k := range u.Kernels {
		out.Kernels = append(out.Kernels, ker{k.Digest, k.Size, k.Kept})
	}
	return out
}

func cmdSystemPrune(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := newFlags("system prune", systemUsage)
	var all, volumes, force bool
	fs.BoolVar(&all, "a", false, "")
	fs.BoolVar(&all, "all", false, "")
	fs.BoolVar(&volumes, "volumes", false, "")
	fs.BoolVar(&force, "f", false, "")
	fs.BoolVar(&force, "force", false, "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	warning := []string{"This will remove:", "  - all stopped containers, and created ones, stopping their VMs"}
	if all {
		warning = append(warning, "  - all images without at least one container associated to them")
	} else {
		warning = append(warning, "  - all dangling images")
	}
	warning = append(warning, "  - all kernels other than the one this runwhp boots")
	if volumes {
		warning = append(warning, "  - all volumes not used by at least one container, and the data in them")
	}
	warning = append(warning, "  - temporary files runs left behind")
	if !force && !confirm(stdin, strings.Join(warning, "\n")) {
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

	// Containers first, so that what they used can go too.
	ids, freed, errs := pruneContainers(root)
	_, u, ok := loadStore()
	if !ok {
		return 1
	}
	p := u.Dangling()
	if all {
		p = u.Unused()
	}
	p.Kernels = u.OldKernels().Kernels
	if volumes {
		p.Volumes = u.UnusedVolumes().Volumes
	}
	r := u.Apply(p)
	sweepStore(root, func(string, ...any) {})

	printSection(stdout, "Deleted Containers:", ids)
	printSection(stdout, "Deleted Images:", imageLines(r))
	printSection(stdout, "Deleted Kernels:", r.Kernels)
	printSection(stdout, "Deleted Volumes:", r.Volumes)
	fmt.Fprintf(stdout, "Total reclaimed space: %s\n", humanSize(freed+r.Freed))
	for _, err := range errs {
		errorf("%v", err)
	}
	if printReport(r) > 0 || len(errs) > 0 {
		return 1
	}
	return 0
}
