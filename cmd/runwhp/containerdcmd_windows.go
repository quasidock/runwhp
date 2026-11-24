//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/netstack"
	"github.com/quasidock/runwhp/internal/vmm"
)

// runwhp containerd runs a containerd of the user's own that runs Linux
// containers through runwhp: it serves runwhp's snapshotter beside it, as
// containerd-shim-runwhp-v1 snapshotter, and puts the shim on its PATH.
// Everything it keeps is in one directory:
//
//	config.toml   containerd's configuration, written anew each time
//	root, state   containerd's, made here first: containerd makes them
//	              for Administrators and SYSTEM alone, which locks out an
//	              unelevated containerd
//	snapshotter   runwhp's snapshotter's, on NTFS
//	store         runwhp's store for the shims' containers, apart from
//	              the user's, whose prunes leave them alone
//	containerd.log
const containerdUsage = `Usage: runwhp containerd [--address PIPE] [--dir DIR] [--containerd EXE] [--debug]

Run a containerd of your own, unelevated, that runs Linux containers
through runwhp, until Ctrl+C: nerdctl talks to it at PIPE. Its snapshots,
its own state and the shims' containers are kept in DIR, which must be on
NTFS. Containers outlive it, and it finds them again when it starts.
`

// The plugins an unelevated containerd cannot run, or runwhp has no use
// for: CRI's, and the CimFS snapshotters, whose failing to start, needing
// a privilege, takes the metadata store down with them.
var containerdDisabled = []string{
	"io.containerd.grpc.v1.cri",
	"io.containerd.cri.v1.runtime",
	"io.containerd.cri.v1.images",
	"io.containerd.podsandbox.controller.v1.podsandbox",
	"io.containerd.snapshotter.v1.cimfs",
	"io.containerd.differ.v1.cimfs",
	"io.containerd.snapshotter.v1.blockcim",
	"io.containerd.differ.v1.blockcim",
}

const shimExe = "containerd-shim-runwhp-v1.exe"

// containerdSetup is what config.toml says.
type containerdSetup struct {
	dir, address, snapshotter, kernel string
	debug                             bool
}

// tomlString quotes s as a TOML literal string, which takes backslashes
// as they are.
func tomlString(s string) (string, error) {
	if strings.ContainsAny(s, "'\n\r") {
		return "", fmt.Errorf("%q cannot go in containerd's configuration", s)
	}
	return "'" + s + "'", nil
}

// containerdConfig is containerd's config.toml.
func containerdConfig(c containerdSetup) (string, error) {
	q := map[string]string{}
	for k, v := range map[string]string{
		"root": filepath.Join(c.dir, "root"), "state": filepath.Join(c.dir, "state"),
		"address": c.address, "ttrpc": c.address + ".ttrpc", "snapshotter": c.snapshotter,
		"store": "RUNWHP_ROOT=" + filepath.Join(c.dir, "store"), "kernel": "RUNWHP_KERNEL=" + c.kernel,
	} {
		s, err := tomlString(v)
		if err != nil {
			return "", err
		}
		q[k] = s
	}
	var disabled []string
	for _, p := range containerdDisabled {
		disabled = append(disabled, "'"+p+"'")
	}
	env := q["store"]
	if c.kernel != "" {
		env += ", " + q["kernel"]
	}
	level := "info"
	if c.debug {
		level = "debug"
	}
	return fmt.Sprintf(`# Written by runwhp containerd; changes are lost the next time it starts.
version = 4
root = %s
state = %s
disabled_plugins = [%s]

[debug]
  level = '%s'

[plugins]
  [plugins.'io.containerd.server.v1.grpc']
    address = %s
  [plugins.'io.containerd.server.v1.ttrpc']
    address = %s
  [plugins.'io.containerd.server.v1.debug']
    address = ''
  [plugins.'io.containerd.shim.v1.manager']
    env = [%s]
  [plugins.'io.containerd.transfer.v1.local']
    [[plugins.'io.containerd.transfer.v1.local'.unpack_config]]
      platform = 'linux/amd64'
      snapshotter = 'runwhp'
      differ = 'windows-lcow'

[proxy_plugins]
  [proxy_plugins.runwhp]
    type = 'snapshot'
    address = %s
`, q["root"], q["state"], strings.Join(disabled, ", "), level, q["address"], q["ttrpc"], env, q["snapshotter"]), nil
}

func cmdContainerd(args []string) int {
	fs := flag.NewFlagSet("containerd", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, containerdUsage) }
	address := fs.String("address", `\\.\pipe\containerd-runwhp`, "")
	dir := fs.String("dir", "", "")
	containerdExe := fs.String("containerd", "", "")
	debug := fs.Bool("debug", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if err := runContainerd(*address, *dir, *containerdExe, *debug); err != nil {
		errorf("containerd: %v", err)
		return 1
	}
	return 0
}

func runContainerd(address, dir, containerdExe string, debug bool) error {
	if !strings.HasPrefix(address, `\\.\pipe\`) {
		if strings.ContainsAny(address, `\/:`) || address == "" {
			return fmt.Errorf("--address %s: give a named pipe, \\\\.\\pipe\\NAME, or its NAME", address)
		}
		address = `\\.\pipe\` + address
	}
	root, err := storeRoot()
	if err != nil {
		return err
	}
	if dir == "" {
		dir = filepath.Join(root, "containerd")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	shim := filepath.Join(filepath.Dir(exe), shimExe)
	if _, err := os.Stat(shim); err != nil {
		return fmt.Errorf("%s is not beside runwhp.exe: %w", shimExe, err)
	}
	if containerdExe == "" {
		if containerdExe, err = exec.LookPath("containerd.exe"); err != nil {
			return errors.New("containerd.exe is not on PATH; give it with --containerd")
		}
	}
	timeout := 200 * time.Millisecond
	if c, err := winio.DialPipe(address, &timeout); err == nil {
		c.Close()
		return fmt.Errorf("something already serves %s", address)
	}

	// The shims boot this runwhp's kernel, from the user's store, fetched
	// now if it has to be, unless RUNWHP_KERNEL names one.
	kernel := vmm.ExplicitKernel("")
	if kernel == "" {
		if kernel, err = vmm.ResolveKernel(context.Background(), root, "", image.PullMissing); err != nil {
			return err
		}
	}
	if kernel, err = filepath.Abs(kernel); err != nil {
		return err
	}
	for _, d := range []string{"root", "state", "snapshotter", "store"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(address)))
	snapPipe := `\\.\pipe\runwhp-snapshotter-` + hex.EncodeToString(sum[:4])
	config, err := containerdConfig(containerdSetup{dir: dir, address: address, snapshotter: snapPipe, kernel: kernel, debug: debug})
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return err
	}

	// Both run in a job that ends them with this process, and lets the
	// shims containerd starts leave it, so that containers outlive it.
	job, err := newContainerdJob()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	logPath := filepath.Join(dir, "containerd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	var out io.Writer = logFile
	if debug {
		out = io.MultiWriter(logFile, os.Stderr)
	}

	snap := exec.Command(shim, "snapshotter", "--root", filepath.Join(dir, "snapshotter"), "--address", snapPipe)
	snap.Stdout, snap.Stderr = out, out
	if err := startInJob(snap, job); err != nil {
		return fmt.Errorf("starting runwhp's snapshotter: %w", err)
	}
	snapExited := make(chan error, 1)
	go func() { snapExited <- snap.Wait() }()
	if err := waitPipe(snapPipe, 10*time.Second, snapExited); err != nil {
		return fmt.Errorf("runwhp's snapshotter: %w; see %s", err, logPath)
	}

	cd := exec.Command(containerdExe, "--config", configPath)
	cd.Env = append(os.Environ(), "PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cd.Stdout, cd.Stderr = out, out
	if err := startInJob(cd, job); err != nil {
		snap.Process.Kill()
		return fmt.Errorf("starting containerd: %w", err)
	}
	cdExited := make(chan error, 1)
	go func() { cdExited <- cd.Wait() }()
	if err := waitPipe(address, 30*time.Second, cdExited); err != nil {
		snap.Process.Kill()
		return fmt.Errorf("containerd: %w; see %s", err, logPath)
	}

	fmt.Printf(`containerd serves %s, running Linux containers through runwhp, until Ctrl+C.
Its log is %s.

For nerdctl, put these lines in %%APPDATA%%\nerdctl\nerdctl.toml:

  address = '%s'
  snapshotter = 'runwhp'
  host_gateway_ip = '%s'

and run containers with:

  nerdctl run --runtime io.containerd.runwhp.v1 --platform linux/amd64 --network none IMAGE
`, address, logPath, address, netstack.Gateway)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	select {
	case <-sigs:
		cd.Process.Kill()
		<-cdExited
		snap.Process.Kill()
		<-snapExited
		return nil
	case err := <-cdExited:
		snap.Process.Kill()
		<-snapExited
		return fmt.Errorf("containerd exited: %v; see %s", err, logPath)
	case err := <-snapExited:
		cd.Process.Kill()
		<-cdExited
		return fmt.Errorf("runwhp's snapshotter exited: %v; see %s", err, logPath)
	}
}

// newContainerdJob makes a job whose processes end when its last handle,
// this process's, closes, and whose children may leave it.
func newContainerdJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// startInJob starts cmd suspended, puts it in job, and lets it run, so
// that nothing it starts escapes the job unasked.
func startInJob(cmd *exec.Cmd, job windows.Handle) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("putting it in a job: %w", err)
	}
	return resumeProcess(uint32(cmd.Process.Pid))
}

// resumeProcess resumes the threads of a process started suspended.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(th)
		windows.CloseHandle(th)
		if err != nil {
			return err
		}
	}
	return nil
}

// waitPipe waits until the named pipe path can be dialed, or exited says
// its server ended.
func waitPipe(path string, wait time.Duration, exited <-chan error) error {
	timeout := 200 * time.Millisecond
	for deadline := time.Now().Add(wait); ; {
		c, err := winio.DialPipe(path, &timeout)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("exited before it served %s: %v", path, err)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("did not serve %s within %v", path, wait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
