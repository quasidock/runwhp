//go:build windows

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/runwhp/internal/volume"
)

const volumeUsage = `Usage:
  runwhp volume create [--opt size=SIZE] [NAME]   make a volume (default size 64g, a limit, not what it takes)
  runwhp volume ls [-q]                           list volumes
  runwhp volume inspect NAME...                   describe volumes as JSON
  runwhp volume rm NAME...                        remove volumes and everything in them
  runwhp volume prune [-a] [-f]                   remove volumes no running container uses

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
		var opts listFlag
		fs.Var(&opts, "opt", "")
		fs.Var(&opts, "o", "")
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
		v, err := store.Create(name, size)
		if err != nil {
			errorf("%v", err)
			return 1
		}
		fmt.Fprintln(stdout, v.Name)
	case "ls", "list":
		var quiet bool
		fs.BoolVar(&quiet, "q", false, "")
		fs.BoolVar(&quiet, "quiet", false, "")
		if fs.Parse(args) != nil || fs.NArg() > 0 {
			return 2
		}
		vs, err := store.List()
		if err != nil {
			errorf("%v", err)
			return 1
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
			out = append(out, info{v.Name, v.CreatedAt, v.Disk(), v.Size, onDisk, busy})
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
			// given. runwhp's anonymous volumes end with their run, so
			// there are none to remove.
			fmt.Fprintln(stdout, "Total reclaimed space: 0B (runwhp keeps no anonymous volumes; -a removes unused named ones)")
			return 0
		}
		if !force {
			fmt.Fprint(os.Stderr, "WARNING! This will remove every volume no running container uses, and all the data in them.\nAre you sure you want to continue? [y/N] ")
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return 0
			}
		}
		vs, err := store.List()
		if err != nil {
			errorf("%v", err)
			return 1
		}
		var freed int64
		var removed []string
		for _, v := range vs {
			if busy, _ := v.InUse(); busy {
				continue
			}
			onDisk, _ := v.OnDisk()
			if err := store.Remove(v.Name); err != nil {
				if !errors.Is(err, volume.ErrInUse) {
					errorf("%v", err)
				}
				continue
			}
			freed += onDisk
			removed = append(removed, v.Name)
		}
		if len(removed) > 0 {
			fmt.Fprintln(stdout, "Deleted Volumes:")
			for _, name := range removed {
				fmt.Fprintln(stdout, name)
			}
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "Total reclaimed space: %s\n", humanSize(freed))
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
