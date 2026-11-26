//go:build windows && !compose

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

// composeBuild says this is runwhp-compose.exe; see composecmd_windows.go.
const composeBuild = false

// composeExe is the program next to runwhp.exe that runs compose.
const composeExe = "runwhp-compose.exe"

// cmdCompose runs runwhp compose in runwhp-compose.exe, as Docker's CLI runs
// a plugin, and exits with its exit code. Ctrl+C reaches it as it reaches
// this process, which leaves it to the child.
func cmdCompose(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	path := filepath.Join(filepath.Dir(exe), composeExe)
	cmd := exec.Command(path, append([]string{"compose"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	case errors.Is(err, os.ErrNotExist):
		errorf("compose needs %s next to runwhp.exe, as build.ps1 builds it", composeExe)
		return 1
	}
	errorf("running %s: %v", composeExe, err)
	return 1
}
