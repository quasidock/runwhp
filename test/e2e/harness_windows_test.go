//go:build windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// defaultImages are the images tests run from the user's store. TestMain
// pulls those missing before any test starts, so that tests running at
// once never pull the same image into that store together.
var defaultImages = []string{alpine, debian, "python:3.12-slim", gitImage}

func TestMain(m *testing.M) {
	if os.Getenv("RUNWHP_E2E") == "1" {
		if err := warmDefaultStore(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	for _, dir := range helperDirs.list() {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// warmDefaultStore pulls the default images the store lacks. It asks no
// registry about the images the store has.
func warmDefaultStore() error {
	exe, err := filepath.Abs(filepath.Join("..", "..", "out", "runwhp.exe"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("%v; run build.ps1 first", err)
	}
	for _, img := range defaultImages {
		if exec.Command(exe, "image", "inspect", img).Run() == nil {
			continue
		}
		if out, err := exec.Command(exe, "pull", img).CombinedOutput(); err != nil {
			return fmt.Errorf("pulling %s: %v\n%s", img, err, out)
		}
	}
	return nil
}

// parallel lets the test run alongside the other parallel tests, which Go
// releases together once the serial ones, those that do not call it, have
// all run. A test runs in parallel unless it measures time or what the
// machine has to give, CPU, memory or bandwidth, or touches what is global
// to the user's store or the machine: its whole list of containers or
// volumes, a fixed port, or Docker's own pipe. RUNWHP_E2E_SERIAL=1 runs
// every test alone, as before iteration 25.
func parallel(t *testing.T) {
	t.Helper()
	runwhp(t)
	if os.Getenv("RUNWHP_E2E_SERIAL") != "1" {
		t.Parallel()
	}
}

// subtest is a named part of a test, for concurrently.
type subtest struct {
	name string
	f    func(t *testing.T)
}

// concurrently runs the subtests at once, in their test's own place among
// the parallel tests. Subtests that called t.Parallel would each wait for
// a place behind every test still waiting for one, and a long one, started
// last, would hold the whole run up. With RUNWHP_E2E_SERIAL=1 they run one
// after another.
func concurrently(t *testing.T, subtests ...subtest) {
	t.Helper()
	if os.Getenv("RUNWHP_E2E_SERIAL") == "1" {
		for _, s := range subtests {
			t.Run(s.name, s.f)
		}
		return
	}
	var wg sync.WaitGroup
	for _, s := range subtests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.Run(s.name, s.f)
		}()
	}
	wg.Wait()
}

// testEnv holds what each test adds to the environment of the runwhps it
// starts, by the test's name, which t.Setenv cannot do for tests that run
// in parallel.
var testEnv struct {
	sync.Mutex
	m map[string][]string
}

// setEnv sets key to value in the environment of the processes the test,
// and its subtests, start through command, until the test ends.
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	name, kv := t.Name(), key+"="+value
	testEnv.Lock()
	if testEnv.m == nil {
		testEnv.m = map[string][]string{}
	}
	testEnv.m[name] = append(testEnv.m[name], kv)
	testEnv.Unlock()
	t.Cleanup(func() {
		testEnv.Lock()
		defer testEnv.Unlock()
		env := testEnv.m[name]
		for i := len(env) - 1; i >= 0; i-- {
			if env[i] == kv {
				testEnv.m[name] = append(env[:i:i], env[i+1:]...)
				break
			}
		}
	})
}

// environ is the environment of the processes the test starts: this
// process's, then what setEnv set for the test's parents, outermost
// first, then for the test itself, the last of each key winning.
func environ(t testing.TB) []string {
	testEnv.Lock()
	defer testEnv.Unlock()
	env := os.Environ()
	parts := strings.Split(t.Name(), "/")
	for i := range parts {
		env = append(env, testEnv.m[strings.Join(parts[:i+1], "/")]...)
	}
	return env
}

// command is runwhp with exactly args, in the test's environment. Callers
// that add to its environment append to cmd.Env.
func command(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(runwhp(t), args...)
	cmd.Env = environ(t)
	return cmd
}

// helperDirs are the directories Linux helpers were built into, which
// TestMain removes.
var helperDirs dirList

type dirList struct {
	sync.Mutex
	dirs []string
}

func (l *dirList) add(dir string) {
	l.Lock()
	l.dirs = append(l.dirs, dir)
	l.Unlock()
}

func (l *dirList) list() []string {
	l.Lock()
	defer l.Unlock()
	return append([]string(nil), l.dirs...)
}

// buildLinux builds the package in ./name for Linux into a directory of its
// own, and gives the directory, to bind into containers.
func buildLinux(name string) (string, error) {
	dir, err := os.MkdirTemp("", "runwhp-"+name+"-")
	if err != nil {
		return "", err
	}
	helperDirs.add(dir)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", filepath.Join(dir, name), "./"+name)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building %s: %v\n%s", name, err, out)
	}
	return dir, nil
}
