//go:build windows

package container

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func testRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	os.MkdirAll(base, 0o755)
	dir, err := os.MkdirTemp(base, "container")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newConfig(id string) *Config {
	if id == "" {
		id = NewID()
	}
	return &Config{Version: ConfigVersion, ID: id, Created: time.Now().UTC(), Image: "alpine:3.20", Args: []string{"true"}}
}

func mustCreate(t *testing.T, s *Store, id, name string) *Container {
	t.Helper()
	c, err := s.Create(newConfig(id), name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateGet(t *testing.T) {
	s := Open(testRoot(t))
	cfg := newConfig("")
	cfg.Env = []string{"A=1"}
	c, err := s.Create(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	if c.State.Status != Creating || c.Name != "web" || c.Dir() != filepath.Join(s.root, "containers", cfg.ID) {
		t.Errorf("created %+v", c)
	}
	got, err := s.Get(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "web" || got.Config.Env[0] != "A=1" || got.State.Status != Creating || got.Short() != cfg.ID[:12] {
		t.Errorf("read back %+v", got)
	}
	if _, err := s.Create(newConfig(cfg.ID), ""); err == nil {
		t.Error("created a second container with the same ID")
	}
	if _, err := s.Create(&Config{ID: "nothex"}, ""); err == nil {
		t.Error("created a container with a bad ID")
	}
	if _, err := s.Create(newConfig(""), "-bad"); err == nil {
		t.Error("created a container with a bad name")
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "tmp"))
	if len(entries) != 0 {
		t.Errorf("tmp holds %d entries", len(entries))
	}
}

func TestResolve(t *testing.T) {
	s := Open(testRoot(t))
	a := mustCreate(t, s, "abc1"+strings.Repeat("0", 60), "alpha")
	b := mustCreate(t, s, "abc2"+strings.Repeat("0", 60), "")
	// A name that looks like a prefix of another container's ID.
	c := mustCreate(t, s, "def0"+strings.Repeat("0", 60), "abc2")
	for _, tc := range []struct {
		ref  string
		want *Container
		err  error
	}{
		{a.ID, a, nil},
		{"alpha", a, nil},
		{"ALPHA", a, nil},
		{"abc1", a, nil},
		{"ABC1", a, nil},
		{"abc2", c, nil}, // names win over prefixes
		{"abc20", b, nil},
		{"abc", nil, ErrAmbiguous},
		{"a", nil, ErrAmbiguous},
		{"beta", nil, ErrNotFound},
		{"fff", nil, ErrNotFound},
		{"", nil, ErrNotFound},
		{"x/y", nil, ErrNotFound},
		{strings.Repeat("a", 65), nil, ErrNotFound},
	} {
		got, err := s.Resolve(tc.ref)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("Resolve(%q) = %v, want %v", tc.ref, err, tc.err)
			}
			continue
		}
		if err != nil || got.ID != tc.want.ID {
			t.Errorf("Resolve(%q) = %v, %v; want %s", tc.ref, got, err, tc.want.Short())
		}
	}
}

// TestMadeUpNames checks that a container given no name gets one, as
// Docker's do, by which it is found, and that many get different ones.
func TestMadeUpNames(t *testing.T) {
	s := Open(testRoot(t))
	shape := regexp.MustCompile(`^[a-z]+_[a-z]+[0-9]?$`)
	seen := map[string]bool{}
	for range 40 {
		c, err := s.Create(newConfig(""), "")
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(c.Name) || CheckName(c.Name) != nil || seen[c.Name] {
			t.Errorf("made-up name %q (seen before: %v)", c.Name, seen[c.Name])
		}
		seen[c.Name] = true
		if got, err := s.Resolve(c.Name); err != nil || got.ID != c.ID {
			t.Errorf("Resolve(%q) = %v, %v", c.Name, got, err)
		}
	}
}

func TestNames(t *testing.T) {
	s := Open(testRoot(t))
	a := mustCreate(t, s, "", "web")
	if _, err := s.Create(newConfig(""), "WEB"); !errors.Is(err, ErrNameInUse) {
		t.Errorf("second container named WEB: %v", err)
	}
	b := mustCreate(t, s, "", "db")
	if err := s.Rename(b, "web"); !errors.Is(err, ErrNameInUse) {
		t.Errorf("rename to a name in use: %v", err)
	}
	if err := s.Rename(a, "Web"); err != nil || a.Name != "Web" {
		t.Errorf("rename to another case: %v, %q", err, a.Name)
	}
	if got, _ := s.Resolve("web"); got == nil || got.Name != "Web" {
		t.Errorf("after the case change: %+v", got)
	}
	if err := s.Rename(a, "frontend"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Resolve("frontend"); err != nil || got.ID != a.ID {
		t.Errorf("resolve the new name: %v, %v", got, err)
	}
	// The old name is free again.
	if err := s.Rename(b, "web"); err != nil {
		t.Errorf("take the freed name: %v", err)
	}
	// A name left by a container that is gone is taken over.
	os.WriteFile(filepath.Join(s.root, "names", "ghost"), []byte(NewID()), 0o644)
	if _, err := s.Create(newConfig(""), "ghost"); err != nil {
		t.Errorf("dangling name: %v", err)
	}
	// Removing a container frees its name.
	c, _ := s.Resolve("frontend")
	if err := s.Remove(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "names", "frontend")); !os.IsNotExist(err) {
		t.Errorf("name kept after remove: %v", err)
	}
	if _, err := s.Create(newConfig(""), "frontend"); err != nil {
		t.Errorf("reuse a removed container's name: %v", err)
	}
}

func TestConcurrentNames(t *testing.T) {
	s := Open(testRoot(t))
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Create(newConfig(""), "shared")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
			} else if !errors.Is(err, ErrNameInUse) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d containers got the name", won)
	}
}

func TestStateWhileRead(t *testing.T) {
	s := Open(testRoot(t))
	c := mustCreate(t, s, "", "")
	l, err := s.Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// A reader holds state.json open while it is replaced.
	f, err := OpenShared(filepath.Join(c.Dir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < 20; i++ {
		if err := l.WriteState(State{Status: Running, Pid: i}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Get(c.ID)
	if got.State.Status != Running || got.State.Pid != 19 {
		t.Errorf("state %+v", got.State)
	}
}

// holdLock is the child process of the lock tests: it locks the container
// RUNWHP_CONTAINER_HOLD names and waits to be killed.
func holdLock() bool {
	spec := os.Getenv("RUNWHP_CONTAINER_HOLD")
	if spec == "" {
		return false
	}
	root, id, _ := strings.Cut(spec, "|")
	s := Open(root)
	c, err := s.Get(id)
	if err == nil {
		var l *Lock
		if l, err = s.Lock(c); err == nil {
			err = l.WriteState(State{Status: Running, Pid: os.Getpid()})
		}
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("holding")
	time.Sleep(time.Minute)
	os.Exit(0)
	return true
}

func startHolder(t *testing.T, s *Store, c *Container) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockedByAnotherProcess$")
	cmd.Env = append(os.Environ(), "RUNWHP_CONTAINER_HOLD="+s.root+"|"+c.ID)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if !strings.HasPrefix(line, "holding") {
		cmd.Process.Kill()
		t.Fatalf("child said %q", line)
	}
	return cmd
}

func TestLockedByAnotherProcess(t *testing.T) {
	if holdLock() {
		return
	}
	s := Open(testRoot(t))
	c := mustCreate(t, s, "", "busy")
	cmd := startHolder(t, s, c)
	defer cmd.Process.Kill()

	got, err := s.Resolve("busy")
	if err != nil || got.State.Status != Running || !got.Locked() {
		t.Errorf("held container: %+v, %v", got, err)
	}
	if _, err := s.Lock(got); !errors.Is(err, ErrRunning) {
		t.Errorf("second lock: %v", err)
	}
	if err := s.Remove(got); !errors.Is(err, ErrRunning) {
		t.Errorf("remove while held: %v", err)
	}
	// Renaming does not need the lock.
	if err := s.Rename(got, "busier"); err != nil {
		t.Errorf("rename while held: %v", err)
	}

	// Once its runwhp is gone, the container is recorded as stopped.
	cmd.Process.Kill()
	cmd.Wait()
	got, err = s.Resolve("busier")
	if err != nil || got.State.Status != Stopped || got.State.ExitCode != 255 || got.State.Error == "" || got.State.Pid != 0 || got.State.FinishedAt.IsZero() {
		t.Errorf("after the holder died: %+v, %v", got.State, err)
	}
	if again, _ := s.Get(got.ID); again.State.Status != Stopped {
		t.Errorf("repair not recorded: %+v", again.State)
	}
	if err := s.Remove(got); err != nil {
		t.Errorf("remove after: %v", err)
	}
	if _, err := s.Resolve(got.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolve after remove: %v", err)
	}
}

// TestRemoveWaitsForFiles removes a container one of whose files a process
// still has open, without FILE_SHARE_DELETE, for a while after it gave the
// container's lock up, as an exiting runwhp does its log: Remove waits for
// it rather than failing.
func TestRemoveWaitsForFiles(t *testing.T) {
	s := Open(testRoot(t))
	c := mustCreate(t, s, "", "")
	f, err := os.OpenFile(filepath.Join(c.Dir(), "held"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		f.Close()
	}()
	if err := s.Remove(c); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(c.Dir()); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
}

func TestListSweeps(t *testing.T) {
	s := Open(testRoot(t))
	keep := mustCreate(t, s, "", "keep")
	l, _ := s.Lock(keep)
	l.WriteState(State{Status: Stopped, ExitCode: 3})
	l.Close()

	// A --rm container whose runwhp died.
	cfg := newConfig("")
	cfg.AutoRemove = true
	rm, _ := s.Create(cfg, "rm")
	l, _ = s.Lock(rm)
	l.WriteState(State{Status: Running})
	l.Close()

	// A record whose runwhp died while making it, and one being made now.
	old := newConfig("")
	old.Created = time.Now().Add(-2 * creatingGrace)
	s.Create(old, "old")
	fresh := mustCreate(t, s, "", "fresh")

	// A container that had stopped, whose runwhp died starting it again.
	again := newConfig("")
	again.Created = time.Now().Add(-3 * creatingGrace)
	re, _ := s.Create(again, "again")
	l, _ = s.Lock(re)
	l.WriteState(State{Status: Creating, Pid: 1, FinishedAt: time.Now().Add(-time.Hour).UTC(), ExitCode: 2})
	l.Close()

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "fresh,keep,again" {
		t.Errorf("listed %v", names)
	}
	if list[0].State.Status != Creating || list[1].State.ExitCode != 3 {
		t.Errorf("states %+v %+v", list[0].State, list[1].State)
	}
	if st := list[2].State; st.Status != Stopped || st.ExitCode != 255 || st.Error == "" {
		t.Errorf("started again: %+v", st)
	}
	for _, name := range []string{"rm", "old"} {
		if _, err := s.Resolve(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s still there: %v", name, err)
		}
	}
	_ = fresh
}

func TestReferencing(t *testing.T) {
	s := Open(testRoot(t))
	cfg := newConfig("")
	cfg.Volumes = []Volume{{Name: "PgData"}}
	s.Create(cfg, "db")
	mustCreate(t, s, "", "web")
	got, err := s.Referencing("pgdata")
	if err != nil || len(got) != 1 || got[0].Name != "db" {
		t.Errorf("Referencing: %v, %v", got, err)
	}
	if got, _ := s.Referencing("other"); len(got) != 0 {
		t.Errorf("Referencing(other) = %v", got)
	}
}

// TestRestartingDied checks a container whose runwhp died while it waited
// to start it again: it is stopped, with the exit it had.
func TestRestartingDied(t *testing.T) {
	s := Open(testRoot(t))
	c := mustCreate(t, s, "", "")
	l, _ := s.Lock(c)
	l.WriteState(State{Status: Restarting, Pid: 1, ExitCode: 3, FinishedAt: time.Now().UTC(), RestartCount: 2})
	l.Close()
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v, %v", list, err)
	}
	if st := list[0].State; st.Status != Stopped || st.ExitCode != 3 || st.RestartCount != 2 || st.Error == "" {
		t.Errorf("state %+v", st)
	}
}

// TestAdoptLock hands a container's lock to another process: it stays
// locked once the first lets its handle go, and is free once the second
// does.
func TestAdoptLock(t *testing.T) {
	if os.Getenv("RUNWHP_ADOPT_LOCK") != "" {
		s := Open(os.Getenv("RUNWHP_ADOPT_ROOT"))
		c, err := s.Get(os.Getenv("RUNWHP_ADOPT_ID"))
		if err != nil {
			fmt.Println("get:", err)
			os.Exit(3)
		}
		h, _ := strconv.ParseUint(os.Getenv("RUNWHP_ADOPT_LOCK"), 10, 64)
		l, err := s.AdoptLock(c, windows.Handle(h))
		if err != nil {
			fmt.Println("adopt:", err)
			os.Exit(4)
		}
		fmt.Println("adopted")
		bufio.NewReader(os.Stdin).ReadString('\n')
		l.Close()
		os.Exit(0)
	}
	root := testRoot(t)
	s := Open(root)
	c := mustCreate(t, s, "", "")
	other := mustCreate(t, s, "", "")
	l, err := s.Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	h, err := l.Inheritable()
	if err != nil {
		t.Fatal(err)
	}
	// Another container's lock is refused.
	ol, _ := s.Lock(other)
	if _, err := s.AdoptLock(c, ol.h); err == nil {
		t.Error("adopted another container's lock")
	}
	ol.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestAdoptLock$")
	cmd.Env = append(os.Environ(), "RUNWHP_ADOPT_LOCK="+strconv.FormatUint(uint64(h), 10),
		"RUNWHP_ADOPT_ROOT="+root, "RUNWHP_ADOPT_ID="+c.ID)
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(h)}}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "adopted\n" {
		in.Close()
		cmd.Wait()
		t.Fatalf("child: %q", line)
	}
	l.Close()
	if !c.Locked() {
		t.Error("unlocked once the first process let it go")
	}
	if _, err := s.lock(c, false); err == nil {
		t.Error("a third took the lock")
	}
	in.Close()
	cmd.Wait()
	if c.Locked() {
		t.Error("still locked once the second let it go")
	}
}

func TestRWPath(t *testing.T) {
	s := Open(testRoot(t))
	c, err := s.Create(&Config{ID: NewID(), Args: []string{"true"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.RW() != filepath.Join(c.Dir(), "rw.ext4") {
		t.Errorf("RW = %s", c.RW())
	}
	elsewhere := filepath.Join(t.TempDir(), "snapshot", "rw.ext4")
	os.MkdirAll(filepath.Dir(elsewhere), 0o755)
	os.WriteFile(elsewhere, []byte("layer"), 0o644)
	d, err := s.Create(&Config{ID: NewID(), Args: []string{"true"}, RWPath: elsewhere,
		Containerd: &ContainerdTask{Namespace: "default", ID: "web"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RW() != elsewhere || got.Config.Containerd == nil || got.Config.Containerd.ID != "web" {
		t.Errorf("RW = %s, containerd %+v", got.RW(), got.Config.Containerd)
	}
	if err := s.Remove(got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("removing the container removed its writable layer elsewhere: %v", err)
	}
}
