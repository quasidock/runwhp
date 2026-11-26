package dockerapi

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/guestspec"
)

// ErrPsArgs is the error for ps options Top does not take.
var ErrPsArgs = errors.New("runwhp lists processes as ps -ef or ps aux shows them; other ps options are not supported")

// Top renders a container's processes as ps with args would, as docker
// top shows them: "" and "-ef" give ps -ef's columns, "aux" ps aux's.
// Times are shown in loc.
func Top(p *guestspec.Processes, args string, loc *time.Location) (titles []string, rows [][]string, err error) {
	procs := append([]guestspec.Process(nil), p.Procs...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	switch strings.TrimSpace(args) {
	case "", "-ef", "-e -f", "-fe":
		titles = []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
		for _, pr := range procs {
			rows = append(rows, []string{
				user(pr), strconv.Itoa(pr.PID), strconv.Itoa(pr.PPID),
				strconv.Itoa(int(cpuPercent(pr, now))), startTime(pr.Start, now, loc), ttyName(pr.TTY),
				cpuTimeLong(pr.CPU), command(pr),
			})
		}
	case "aux", "-aux":
		titles = []string{"USER", "PID", "%CPU", "%MEM", "VSZ", "RSS", "TTY", "STAT", "START", "TIME", "COMMAND"}
		for _, pr := range procs {
			mem := 0.0
			if p.MemTotal > 0 {
				mem = float64(pr.RSS) * 100 / float64(p.MemTotal)
			}
			rows = append(rows, []string{
				user(pr), strconv.Itoa(pr.PID), fmt.Sprintf("%.1f", cpuPercent(pr, now)), fmt.Sprintf("%.1f", mem),
				strconv.FormatUint(pr.VSZ/1024, 10), strconv.FormatUint(pr.RSS/1024, 10), ttyName(pr.TTY),
				stat(pr), startTime(pr.Start, now, loc), cpuTimeShort(pr.CPU), command(pr),
			})
		}
	default:
		return nil, nil, fmt.Errorf("ps %s: %w", args, ErrPsArgs)
	}
	if rows == nil {
		rows = [][]string{}
	}
	return titles, rows, nil
}

func user(p guestspec.Process) string {
	if p.User != "" {
		return p.User
	}
	return strconv.FormatUint(uint64(p.UID), 10)
}

// cpuPercent is the share of one processor the process has used since it
// started, as ps's C and %CPU are.
func cpuPercent(p guestspec.Process, now time.Time) float64 {
	elapsed := now.Sub(p.Start)
	if elapsed <= 0 {
		return 0
	}
	return float64(p.CPU) * 100 / float64(elapsed)
}

// startTime is ps's STIME and START: the time of day within the last 24
// hours, the month and day within the year, else the year.
func startTime(t, now time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "?"
	}
	t, now = t.In(loc), now.In(loc)
	switch {
	case now.Sub(t) < 24*time.Hour:
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan02")
	default:
		return strconv.Itoa(t.Year())
	}
}

// cpuTimeLong is ps -ef's TIME: [DD-]HH:MM:SS.
func cpuTimeLong(d time.Duration) string {
	s := int64(d / time.Second)
	days, h, m, sec := s/86400, s/3600%24, s/60%60, s%60
	if days > 0 {
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}

// cpuTimeShort is ps aux's TIME: M:SS.
func cpuTimeShort(d time.Duration) string {
	s := int64(d / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// ttyName names a terminal by its device number, as ps does; "?" for none.
func ttyName(dev uint32) string {
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & 0xfff00)
	switch {
	case dev == 0:
		return "?"
	case major >= 136 && major <= 143:
		return fmt.Sprintf("pts/%d", minor+(major-136)*256)
	case major == 4 && minor < 64:
		return fmt.Sprintf("tty%d", minor)
	case major == 4:
		return fmt.Sprintf("ttyS%d", minor-64)
	case major == 5 && minor == 1:
		return "console"
	}
	return "?"
}

// stat is ps aux's STAT: the state, with < for a raised priority, N for a
// lowered one, s for a session leader, l for many threads and + for the
// terminal's foreground group.
func stat(p guestspec.Process) string {
	s := p.State
	switch {
	case p.Nice < 0:
		s += "<"
	case p.Nice > 0:
		s += "N"
	}
	if p.PID == p.SID {
		s += "s"
	}
	if p.Threads > 1 {
		s += "l"
	}
	if p.TPGID > 0 && p.TPGID == p.PGID {
		s += "+"
	}
	return s
}

// command is the process's arguments, or its name in brackets without
// any, as ps shows a process that has none.
func command(p guestspec.Process) string {
	if len(p.Args) == 0 {
		return "[" + p.Comm + "]"
	}
	return strings.Join(p.Args, " ")
}
