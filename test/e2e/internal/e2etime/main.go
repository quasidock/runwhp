// Command e2etime summarises end-to-end runs from go test's JSON (go tool
// test2json -t): each run's wall time, its serial phase, the top-level
// tests that ran one after another, and its parallel phase, the tests
// released together after them, its failures, and its slowest tests.
// Given -base, a run's times are compared with that run's.
//
//	go run ./test/e2e/internal/e2etime [-n 25] [-base base.json] run.json...
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type event struct {
	Time    time.Time
	Action  string
	Test    string
	Elapsed float64
}

// run is one test binary's events, summarised.
type run struct {
	start, end   time.Time
	firstCont    time.Time // when the first parallel top-level test resumed
	tests        map[string]float64
	failed       []string
	passed       int
	skipped      int
	parallel     map[string]bool
	unfinished   []string
	packageFails bool
}

func read(path string) (*run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &run{tests: map[string]float64{}, parallel: map[string]bool{}}
	started := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // a line that is not an event: a build error, say
		}
		if !e.Time.IsZero() {
			if r.start.IsZero() || e.Time.Before(r.start) {
				r.start = e.Time
			}
			if e.Time.After(r.end) {
				r.end = e.Time
			}
		}
		top := e.Test != "" && !strings.Contains(e.Test, "/")
		switch {
		case e.Test == "" && e.Action == "fail":
			r.packageFails = true
		case !top:
		case e.Action == "run":
			started[e.Test] = true
		case e.Action == "cont":
			r.parallel[e.Test] = true
			if r.firstCont.IsZero() {
				r.firstCont = e.Time
			}
		case e.Action == "pass", e.Action == "fail", e.Action == "skip":
			delete(started, e.Test)
			r.tests[e.Test] = e.Elapsed
			switch e.Action {
			case "pass":
				r.passed++
			case "fail":
				r.failed = append(r.failed, e.Test)
			default:
				r.skipped++
			}
		}
	}
	for t := range started {
		r.unfinished = append(r.unfinished, t)
	}
	sort.Strings(r.unfinished)
	return r, sc.Err()
}

func (r *run) wall() time.Duration { return r.end.Sub(r.start) }

// phases splits the wall time at the first parallel test's release.
func (r *run) phases() (serial, parallel time.Duration) {
	if r.firstCont.IsZero() {
		return r.wall(), 0
	}
	return r.firstCont.Sub(r.start), r.end.Sub(r.firstCont)
}

func secs(d time.Duration) string { return fmt.Sprintf("%.0f s", d.Seconds()) }

func report(w io.Writer, path string, r, base *run, n int) {
	serial, par := r.phases()
	status := "ok"
	if len(r.failed) > 0 || r.packageFails || len(r.unfinished) > 0 {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%s: %s, %s (serial %s, parallel %s); %d passed, %d failed, %d skipped",
		path, status, secs(r.wall()), secs(serial), secs(par), r.passed, len(r.failed), r.skipped)
	if base != nil {
		bs, bp := base.phases()
		fmt.Fprintf(w, "; base %s (serial %s, parallel %s)", secs(base.wall()), secs(bs), secs(bp))
	}
	fmt.Fprintln(w)
	for _, t := range r.failed {
		fmt.Fprintf(w, "  FAIL %s (%.1f s)\n", t, r.tests[t])
	}
	for _, t := range r.unfinished {
		fmt.Fprintf(w, "  UNFINISHED %s\n", t)
	}
	names := make([]string, 0, len(r.tests))
	for t := range r.tests {
		names = append(names, t)
	}
	sort.Slice(names, func(i, j int) bool { return r.tests[names[i]] > r.tests[names[j]] })
	if n > len(names) {
		n = len(names)
	}
	var sum float64
	for _, t := range names {
		sum += r.tests[t]
	}
	fmt.Fprintf(w, "  tests' own times add up to %.0f s; slowest %d:\n", sum, n)
	for _, t := range names[:n] {
		mode := "serial  "
		if r.parallel[t] {
			mode = "parallel"
		}
		fmt.Fprintf(w, "  %7.1f s  %s  %s", r.tests[t], mode, t)
		if base != nil {
			if b, ok := base.tests[t]; ok {
				fmt.Fprintf(w, "  (base %.1f s)", b)
			}
		}
		fmt.Fprintln(w)
	}
}

func main() {
	n := flag.Int("n", 25, "how many of the slowest tests to list")
	basePath := flag.String("base", "", "a run to compare each run with")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: e2etime [-n N] [-base base.json] run.json...")
		os.Exit(2)
	}
	var base *run
	if *basePath != "" {
		var err error
		if base, err = read(*basePath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for _, p := range flag.Args() {
		r, err := read(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report(os.Stdout, p, r, base, *n)
	}
}
