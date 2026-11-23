//go:build windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/quasidock/runwhp/internal/gc"
)

// confirm prints a prune's warning and asks to go on, as docker's prunes
// do, reading the answer from stdin.
func confirm(stdin io.Reader, warning string) bool {
	fmt.Fprint(os.Stderr, "WARNING! "+warning+"\nAre you sure you want to continue? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// exclusive takes the store's lock for a prune or a removal, saying so if
// it has to wait for a pull or a run.
func exclusive(root string) (func(), error) {
	return gc.Exclusive(root, func() {
		errorf("waiting for a pull or run to finish resolving its image")
	})
}

// printReport reports what a removal left in use, and its errors. It
// returns the number of errors.
func printReport(r *gc.Report) int {
	for _, d := range r.InUse {
		errorf("%s is in use; left it", d)
	}
	for _, err := range r.Errors {
		errorf("%v", err)
	}
	return len(r.Errors)
}

// printSection writes a titled list, as docker's prunes do, if it is not
// empty.
func printSection(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, title)
	for _, s := range items {
		fmt.Fprintln(w, s)
	}
	fmt.Fprintln(w)
}
