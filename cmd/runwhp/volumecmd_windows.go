//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/volume"
)

const volumeUsage = `Usage:
  runwhp volume create [--opt size=SIZE] [--label K=V]... [NAME]
                                                  make a volume (default size 64g, a limit, not what it takes)
  runwhp volume ls [-q] [-f FILTER]...            list volumes, or those that match name, label,
                                                  driver or dangling (true: no container uses it)
  runwhp volume inspect NAME...                   describe volumes as JSON
  runwhp volume rm NAME...                        remove volumes and everything in them
  runwhp volume prune [-a] [-f]                   remove volumes no container uses

A volume is an ext4 file system in a sparse file in the store's volumes
directory. Mount one with run -v NAME:PATH; a container at a time can use it.
`

// cmdVolume runs runwhp volume. It returns 1 when a command fails and 2 for
// a usage error, as pull does.
func cmdVolume(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, volumeUsage)
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	store := volume.Open(root)
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("volume "+sub, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), volumeUsage) }
	switch sub {
	case "create":
		var opts, labelArgs listFlag
		fs.Var(&opts, "opt", "")
		fs.Var(&opts, "o", "")
		fs.Var(&labelArgs, "label", "")
		if fs.Parse(args) != nil || fs.NArg() > 1 {
			return 2
		}
		size := int64(defaultDiskSize)
		for _, o := range opts {
			k, v, _ := strings.Cut(o, "=")
			if k != "size" {
				errorf("volume create: option %q is not supported; runwhp knows size=SIZE", o)
				return 2
			}
			if size, err = parseDiskSize(v); err != nil {
				errorf("volume create: %v", err)
				return 2
			}
		}
		name := fs.Arg(0)
		if name == "" {
			var b [16]byte
			rand.Read(b[:])
			name = hex.EncodeToString(b[:])
		}
		labels, err := parseLabels(labelArgs, nil)
		if err != nil {
			errorf("volume create: %v", err)
			return 2
		}
		v, err := store.CreateWith(name, size, labels)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		fmt.Fprintln(stdout, v.Name)
	case "ls", "list":
		var quiet bool
		var filterArgs listFlag
		fs.BoolVar(&quiet, "q", false, "")
		fs.BoolVar(&quiet, "quiet", false, "")
		fs.Var(&filterArgs, "f", "")
		fs.Var(&filterArgs, "filter", "")
		if fs.Parse(args) != nil || fs.NArg() > 0 {
			return 2
		}
		f, err := parseFilters(filterArgs, "name", "label", "driver", "dangling")
		if err != nil {
			errorf("%v", err)
			return 2
		}
		all, err := store.List()
		if err != nil {
			errorf("%v", err)
			return 1
		}
		var vs []*volume.Volume
		for _, v := range all {
			if f.matchVolume(v, len(f["dangling"]) > 0 && volumeUsers(root, v.Name) != "") {
				vs = append(vs, v)
			}
		}
		if quiet {
			for _, v := range vs {
				fmt.Fprintln(stdout, v.Name)
			}
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSIZE\tON DISK\tCREATED")
		for _, v := range vs {
			onDisk, _ := v.OnDisk()
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Name, humanSize(v.Size), humanSize(onDisk), v.CreatedAt.Local().Format(time.DateTime))
		}
		tw.Flush()
	case "inspect":
		if fs.Parse(args) != nil || fs.NArg() == 0 {
			fs.Usage()
			return 2
		}
		type info struct {
			Name       string
			CreatedAt  time.Time
			Mountpoint string // the disk image, as close as runwhp has to Docker's directory
			Size       int64
			OnDisk     int64
			InUse      bool
			Labels     map[string]string
		}
		var out []info
		code := 0
		for _, name := range fs.Args() {
			v, err := store.Get(name)
			if err != nil {
				errorf("%v", err)
				code = 1
				continue
			}
			onDisk, _ := v.OnDisk()
			busy, _ := v.InUse()
			out = append(out, info{v.Name, v.CreatedAt, v.Disk(), v.Size, onDisk, busy, withLabels(map[string]string{}, v.Labels)})
		}
		data, _ := json.MarshalIndent(out, "", "    ")
		fmt.Fprintln(stdout, string(data))
		return code
	case "rm", "remove":
		if fs.Parse(args) != nil || fs.NArg() == 0 {
			fs.Usage()
			return 2
		}
		code := 0
		for _, name := range fs.Args() {
			if users := volumeUsers(root, name); users != "" {
				errorf("volume %s is in use by container %s; delete the container first", name, users)
				code = 1
				continue
			}
			if err := store.Remove(name); err != nil {
				errorf("%v", err)
				code = 1
				continue
			}
			fmt.Fprintln(stdout, name)
		}
		return code
	case "prune":
		var all, force bool
		fs.BoolVar(&all, "a", false, "")
		fs.BoolVar(&all, "all", false, "")
		fs.BoolVar(&force, "f", false, "")
		fs.BoolVar(&force, "force", false, "")
		if fs.Parse(args) != nil || fs.NArg() > 0 {
			return 2
		}
		if !all {
			// Docker's prune removes only anonymous volumes unless -a is
			// given. runwhp's live in their containers' writable layers,
			// and go with them, so there are none to remove.
			fmt.Fprintln(stdout, "Total reclaimed space: 0B (runwhp's anonymous volumes go with their containers; -a removes unused named ones)")
			return 0
		}
		if !force && !confirm(stdin, "This will remove every volume no container uses, and all the data in them.") {
			return 0
		}
		// A run makes the volumes it names before it records its
		// container, under the store's lock.
		release, err := exclusive(root)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		defer release()
		u, err := gc.Load(root)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		r := u.Apply(u.UnusedVolumes())
		printSection(stdout, "Deleted Volumes:", r.Volumes)
		fmt.Fprintf(stdout, "Total reclaimed space: %s\n", humanSize(r.Freed))
		if printReport(r) > 0 {
			return 1
		}
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, volumeUsage)
	default:
		fmt.Fprintf(os.Stderr, "runwhp: unknown volume command %q\n\n%s", sub, volumeUsage)
		return 2
	}
	return 0
}

// humanSize writes a byte count in binary units, as sizes are given:
// 64GiB, 416KiB, 12B.
func humanSize(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	var num string
	switch {
	case i == 0:
		return fmt.Sprintf("%dB", n)
	case f >= 100:
		num = fmt.Sprintf("%.0f", f)
	case f >= 10:
		num = fmt.Sprintf("%.1f", f)
	default:
		num = fmt.Sprintf("%.2f", f)
	}
	if strings.Contains(num, ".") {
		num = strings.TrimRight(strings.TrimRight(num, "0"), ".")
	}
	return num + units[i]
}

// volumeUsers names the containers, running or not, that mount the named
// volume, or is empty if none does.
func volumeUsers(root, name string) string {
	cs, err := container.Open(root).Referencing(name)
	if err != nil {
		return ""
	}
	var names []string
	for _, c := range cs {
		if c.Name != "" {
			names = append(names, c.Name)
		} else {
			names = append(names, c.Short())
		}
	}
	return strings.Join(names, ", ")
}
