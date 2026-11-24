//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/container"
)

const statsUsage = `Usage: runwhp stats [--no-stream] [--format table|json] [CONTAINER...]

Show what running containers use, every second until Ctrl+C: of their
processors, of their memory, and what their runwhp holds of this machine's.
Without CONTAINER, every running container.

      --no-stream      print once and exit
      --format FORMAT  table (the default), or json: one object per
                       container and sample, a line each
`

// statsSample is one container's statistics at one moment.
type statsSample struct {
	c     *container.Container
	stats *container.Stats
	err   error
}

func cmdStats(args []string, stdout io.Writer) int {
	fs := newFlags("stats", statsUsage)
	var noStream bool
	var format string
	fs.BoolVar(&noStream, "no-stream", false, "")
	fs.StringVar(&format, "format", "table", "")
	if fs.Parse(args) != nil {
		return 2
	}
	if format != "table" && format != "json" {
		errorf("stats: unknown format %q; use table or json", format)
		return 2
	}
	store, ok := openContainers()
	if !ok {
		return 1
	}
	var cs []*container.Container
	if fs.NArg() == 0 {
		all, err := store.List()
		if err != nil {
			errorf("%v", err)
			return 1
		}
		for _, c := range all {
			if c.State.Status == container.Running {
				cs = append(cs, c)
			}
		}
	} else {
		for _, ref := range fs.Args() {
			c, err := store.Resolve(ref)
			if err != nil {
				errorf("%v", err)
				return 1
			}
			if c.State.Status != container.Running {
				errorf("container %s is not running", c.Short())
				return 1
			}
			cs = append(cs, c)
		}
	}
	root, _ := storeRoot()
	console := format == "table" && !noStream && enableVT(os.Stdout)

	prev := sampleAll(root, cs)
	if noStream {
		time.Sleep(500 * time.Millisecond)
	} else {
		time.Sleep(time.Second)
	}
	for {
		cur := sampleAll(root, cs)
		rows := statsRows(prev, cur)
		if console {
			fmt.Fprint(stdout, "\x1b[H\x1b[2J")
		}
		if format == "json" {
			enc := json.NewEncoder(stdout)
			for _, r := range rows {
				enc.Encode(r)
			}
		} else {
			printStatsTable(stdout, rows)
			if !console && !noStream {
				fmt.Fprintln(stdout)
			}
		}
		if noStream {
			return 0
		}
		prev = cur
		time.Sleep(time.Second)
	}
}

// sampleAll asks each container's runwhp for its statistics, all at once.
func sampleAll(root string, cs []*container.Container) []statsSample {
	out := make([]statsSample, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			rep, err := container.Ask(ctx, root, c.ID, container.Request{Op: container.OpStats})
			switch {
			case errors.Is(err, container.ErrNoRunwhp):
				err = fmt.Errorf("container %s is not running", c.Short())
			case err == nil && !rep.OK:
				err = errors.New(rep.Error)
			case err == nil && rep.Stats == nil:
				err = fmt.Errorf("container %s's runwhp is too old to report statistics", c.Short())
			}
			out[i] = statsSample{c: c, stats: rep.Stats, err: err}
		})
	}
	wg.Wait()
	return out
}

// statsRow is one line of runwhp stats, and its JSON form.
type statsRow struct {
	ID             string  `json:"id"`
	Name           string  `json:"name,omitempty"`
	CPUPercent     float64 `json:"cpuPercent"`
	CPUs           int     `json:"cpus"`
	MemUsage       uint64  `json:"memUsage,omitempty"`
	MemLimit       uint64  `json:"memLimit"`
	MemPercent     float64 `json:"memPercent,omitempty"`
	HostCommit     uint64  `json:"hostCommit"`
	HostWorkingSet uint64  `json:"hostWorkingSet"`
	NetRx          uint64  `json:"netRx"`
	NetTx          uint64  `json:"netTx"`
	BlockRead      uint64  `json:"blockRead"`
	BlockWrite     uint64  `json:"blockWrite"`
	PIDs           int     `json:"pids,omitempty"`
	Error          string  `json:"error,omitempty"`

	guest bool // whether init answered, so the guest's numbers are known
}

// statsRows turns two samples of the same containers into rows: CPU % is
// the processor time used between them.
func statsRows(prev, cur []statsSample) []statsRow {
	var rows []statsRow
	for i, s := range cur {
		r := statsRow{ID: s.c.Short(), Name: s.c.Name}
		if s.err != nil {
			r.Error = s.err.Error()
			rows = append(rows, r)
			continue
		}
		st := s.stats
		r.CPUs, r.MemLimit = st.CPUs, st.MemLimit
		r.HostCommit, r.HostWorkingSet = st.HostCommit, st.HostWorkingSet
		r.NetRx, r.NetTx, r.BlockRead, r.BlockWrite = st.NetRx, st.NetTx, st.BlockRead, st.BlockWrite
		if p := prev[i].stats; p != nil && st.Time.After(p.Time) && st.CPUTime >= p.CPUTime {
			r.CPUPercent = 100 * float64(st.CPUTime-p.CPUTime) / float64(st.Time.Sub(p.Time))
		}
		if st.Guest != nil {
			r.guest = true
			r.MemUsage = st.Guest.MemTotal - min(st.Guest.MemAvailable, st.Guest.MemTotal)
			if r.MemLimit > 0 {
				r.MemPercent = 100 * float64(r.MemUsage) / float64(r.MemLimit)
			}
			r.PIDs = st.Guest.Tasks
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func printStatsTable(w io.Writer, rows []statsRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER ID\tNAME\tCPU %\tMEM USAGE / LIMIT\tMEM %\tHOST MEM\tNET I/O\tBLOCK I/O\tPIDS")
	for _, r := range rows {
		if r.Error != "" {
			// Most likely it stopped. JSON says why.
			fmt.Fprintf(tw, "%s\t%s\t--\t--\t--\t--\t--\t--\t--\n", r.ID, r.Name)
			continue
		}
		mem, memPct, pids := "--", "--", "--"
		if r.guest {
			mem = binarySize(r.MemUsage) + " / " + binarySize(r.MemLimit)
			memPct = fmt.Sprintf("%.2f%%", r.MemPercent)
			pids = fmt.Sprint(r.PIDs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.2f%%\t%s\t%s\t%s\t%s / %s\t%s / %s\t%s\n", r.ID, r.Name, r.CPUPercent, mem, memPct,
			binarySize(r.HostCommit), decimalSize(r.NetRx), decimalSize(r.NetTx), decimalSize(r.BlockRead), decimalSize(r.BlockWrite), pids)
	}
	tw.Flush()
}

// binarySize writes memory sizes as Docker does: 1.5GiB, 512MiB.
func binarySize(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f, u := float64(n), 0
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	return trimSize(f, units[u])
}

// decimalSize writes I/O as Docker does: 1.2MB, 340kB.
func decimalSize(n uint64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f, u := float64(n), 0
	for f >= 1000 && u < len(units)-1 {
		f /= 1000
		u++
	}
	return trimSize(f, units[u])
}

func trimSize(f float64, unit string) string {
	s := fmt.Sprintf("%.3g", f)
	if strings.Contains(s, "e") {
		s = fmt.Sprintf("%.0f", f)
	}
	return s + unit
}

// enableVT turns on VT sequences for f, if it is a console, and reports
// whether it is one.
func enableVT(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	return true
}
