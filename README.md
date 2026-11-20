# runwhp - minimalistic container runtime for Windows

`runwhp` is an experimental OCI runtime that runs Linux containers on any Windows
edition, including Home. It drives the Windows Hypervisor Platform (WHP)
directly from an ordinary user process, so it needs no Hyper-V role, no WSL, no
HCS, and no administrator rights at run time.

Quasidock itself runs native Windows programs and does not run Linux
containers. `runwhp` is a separate subproject for software that has no Windows
build.

## Status

`runwhp` is experimental. It runs Linux containers from registry images,
interactively or not, with a network, but without access to host files yet.

- **Iteration 3** (2026-11-20): containers have a network. Outbound TCP and
  UDP leave through the host's own sockets, names resolve through the host's
  resolver, `-p` publishes container ports on the host, and
  `--add-host NAME:host-gateway` reaches services on the host's loopback. It
  needs no elevation and no new kernel module. See [Networking](#networking)
  and [Iteration 3 results](#iteration-3-results).
- **Iteration 2** (2026-11-20): `-i` passes stdin to the container with a real
  end of file, `-t` gives it a pseudo-terminal that follows the console's size,
  and Ctrl+C reaches the container as SIGINT instead of killing the VM.
  Container output no longer runs on the vCPU thread, so a slow reader stalls
  only its own stream. See [Usage](#usage) and
  [Iteration 2 results](#iteration-2-results).
- **Iteration 1** (2026-11-20): `runwhp run IMAGE [COMMAND...]` pulls an image,
  converts its layers to ext4, boots one VM, runs the entrypoint and command
  with the image's environment, working directory and user, sends stdout and
  stderr to the host separately, and exits with the container's exit code.
  See [Usage](#usage) and [Iteration 1 results](#iteration-1-results).
- **Proven by a spike** (2026-11-17): an unelevated Go program boots a Linux
  kernel on WHP. See [Boot spike](#boot-spike).
- **Proposed, not yet agreed**: the rest of the architecture in
  [Proposed design](#proposed-design).
- **Not started**: host file sharing, more than one vCPU, the runc-style
  lifecycle commands, and a kernel of our own.

## Why not runhcs, HCS, or WSL

The first idea was a fork of `runhcs`. That is no longer the plan.

| Option | What it needs | Why it was set aside |
| --- | --- | --- |
| Fork `runhcs` | HCS, plus the Containers and Hyper-V features | Does not work out of the box on Home. Most of its code is Windows-container and HCS plumbing; the Linux path is a thin layer on top. Upstream's supported path is the containerd shim, not the `runhcs` CLI. |
| Call HCS directly, as WSL2 does | "Virtual Machine Platform" feature | HCS is present on Home, but creating a compute system is expected to need administrator rights or Hyper-V Administrators membership on every call. This was not tested here. |
| Run inside WSL2 | WSL installed | Works on Home and is what Docker Desktop and Podman do, but makes WSL a hard dependency. |
| Software emulation (QEMU TCG) | Nothing | Several times slower, and a large GPL dependency. Possible later fallback. |
| **WHP with our own VMM** | Windows hypervisor running | Works from a standard user process. More code to write, but all of it is ours. |

The reusable parts of hcsshim do not need a fork: `ext4/tar2ext4` is an
importable Go package that turns an OCI layer tar into an ext4 image without
administrator rights.

## Requirements

- x86-64 Windows with the Windows hypervisor running and WHP usable.
- No elevation at run time.

What was observed on the test machine (Windows 11 Home build 26300, AMD Ryzen 7
5700U, WSL2 already installed):

- `WHvGetCapability` reports the hypervisor present, and
  `WHvCreatePartition`, `WHvSetupPartition`, and `WHvCreateVirtualProcessor`
  all succeed from an unelevated process.
- The HCS and HNS services (`vmcompute`, `hns`) are running. `vmms.exe`, the
  Hyper-V management service, is absent.

Not yet known: whether a clean Home install needs the "Windows Hypervisor
Platform" optional feature enabled first. Enabling a feature takes
administrator rights and a reboot, once. The test machine cannot answer this
because WSL2 had already turned the hypervisor on.

Until runwhp has its own kernel, it boots the WSL kernel, so WSL must be
installed. The WSL kernel builds `virtio_blk` as a module, and that module has
to be copied out of a WSL distro once per kernel version (see below).

## Usage

Build, from the repository root in PowerShell. This produces `out\runwhp.exe`
and the guest init `out\runwhp-init`, which must stay next to it:

```powershell
.\build.ps1
```

Once per WSL kernel version, copy the block driver into runwhp's store:

```powershell
wsl -e sh hack/wsl-modules.sh "$env:LOCALAPPDATA\runwhp\modules"
```

Then:

```powershell
.\out\runwhp.exe run alpine:3.20 echo hello
.\out\runwhp.exe run -u nobody -e FOO=bar -w /tmp alpine:3.20 sh -c 'id; echo $FOO; pwd'
.\out\runwhp.exe run python:3.12-slim python3 -c 'print(1)'
.\out\runwhp.exe run -it alpine:3.20 sh
Get-Content data.csv | .\out\runwhp.exe run -i alpine:3.20 wc -l
.\out\runwhp.exe run alpine:3.20 sh -c 'apk add -q curl && curl -sI https://example.com'
.\out\runwhp.exe run -p 8080:80 nginx:alpine
.\out\runwhp.exe run --add-host host.docker.internal:host-gateway alpine:3.20 wget -qO- http://host.docker.internal:3000/
.\out\runwhp.exe pull debian:12
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-i`, `--interactive` | off | Pass stdin to the container. Without it, the container reads `/dev/null` |
| `-t`, `--tty` | off | Run the container on a pseudo-terminal of the console's size, with stderr merged into stdout. With `-i`, stdin must be a console, and keys go to the container raw. `-it` works as a shorthand |
| `--entrypoint` | image's | Replace the entrypoint; `""` removes it. Also drops the image's command, as in Docker |
| `-e`, `--env` | | `KEY=VALUE`, or `KEY` to copy it from the host; repeatable |
| `-w`, `--workdir` | image's, else `/` | Working directory; created if missing |
| `-u`, `--user` | image's, else root | `name`, `uid`, `name:group` or `uid:gid`, resolved against the image's `/etc/passwd` and `/etc/group` |
| `-m`, `--memory` | `512m` | Guest RAM, `128m` to `3328m`. The writable layer is a tmpfs inside it |
| `--hostname` | random | Hostname, also written to `/etc/hostname` and `/etc/hosts` |
| `--network`, `--net` | `user` | `user` gives the container a network through runwhp; `none` leaves it only loopback |
| `-p`, `--publish` | | `[IP:]HOSTPORT:PORT[/tcp]`: listen on the host and forward to the container's TCP port; repeatable. IP defaults to `127.0.0.1` |
| `--add-host` | | `NAME:IP` or `NAME=IP`, added to `/etc/hosts`; repeatable. `NAME:host-gateway` also lets the container reach the host's `127.0.0.1` at that name |
| `--pull` | `missing` | `missing`, `always` or `never` |
| `--kernel` | `C:\Program Files\WSL\tools\kernel` | bzImage to boot |
| `--debug` | off | Kernel console, init's steps and VMM statistics on stderr |

Exit codes follow Docker: the container's own code, 128+N if a signal N killed
it, 125 if runwhp failed, 126 if the command cannot be run, 127 if it does not
exist, and 130 if runwhp stopped the VM after Ctrl+C. When runwhp fails after
the guest booted, it prints the last kernel console lines.

Ctrl+C and Ctrl+Break send SIGINT to the container's process group, as a
terminal would. A container that does not handle it exits with 130; one that
traps it decides for itself. A second Ctrl+C stops the VM at once. With
`-it`, Ctrl+C is a key like any other, and the container's terminal turns it
into SIGINT. Closing the console window, logging off or shutting down sends
SIGTERM to the container's main process and stops the VM 10 seconds later.
While the guest is still booting, Ctrl+C stops the VM at once.

With `-i` and a console but no `-t`, the console edits each line before
sending it, ends lines with `\n`, and Ctrl+Z at the start of a line ends the
input, as usual on Windows.

Images and layers are cached in `%LOCALAPPDATA%\runwhp`, or in `RUNWHP_ROOT`
when it is set. Registry credentials come from Docker's configuration and
credential helpers. Only `linux/amd64` images run. Nothing is ever removed from
the cache yet.

In Git Bash, set `MSYS_NO_PATHCONV=1`, or MSYS rewrites container paths such
as `/tmp` into Windows paths before runwhp sees them. Under mintty, the
default Git Bash window, stdin is a pipe rather than a console, so `-it`
fails with "the input device is not a TTY"; prefix the command with
`winpty`. Pipes from MSYS programs into native ones are slow (about 8 MB/s
on the test machine), and stdin through `-i` is no exception.

### Networking

Unless run with `--network none`, the container has one interface, `eth0`,
on a private network that exists only inside runwhp:

| Address | What it is |
| --- | --- |
| `10.0.2.15/24` | The container |
| `10.0.2.2` | The gateway, which is runwhp itself. With `--add-host NAME:host-gateway`, connections to it reach the host's `127.0.0.1`; without it they are refused |
| `10.0.2.3` | DNS, answered by runwhp |

runwhp ends the container's TCP connections and UDP flows itself and opens
the same connections from the host, as any program on the host would. The
container reaches whatever the host reaches, LAN and VPN included, and nothing
in the host's network configuration changes. Names resolve through Windows's
own resolver, so the hosts file, VPN DNS and per-domain DNS servers apply as
they do on the host. The network is IPv4 only: AAAA queries get an empty
answer, which sends programs straight to IPv4.

The host's loopback is closed to the container unless `host-gateway` opens
it. A service that listens on all of the host's interfaces can still be
reached at the host's LAN address, as with Docker.

`-p 8080:80` listens on the host's `127.0.0.1:8080` and forwards each
connection to port 80 in the container, which sees it come from `10.0.2.2`.
`-p 0.0.0.0:8080:80` listens on every interface; Windows Firewall then asks,
once, whether to allow it, and allowing it on public networks needs an
administrator. A connection that arrives before anything in the container
listens on the port is accepted and then closed, as with Docker.

`ping` gets answers only from the gateway: runwhp carries TCP and UDP, not
ICMP.

### How a run works

1. The image's layers are pulled and each one is converted to an ext4 image
   with `tar2ext4`, with OCI whiteouts turned into overlayfs whiteouts. Layers
   are cached by diffID and verified against it; empty layers are skipped.
2. runwhp writes an initramfs holding `runwhp-init`, the `virtio_blk` module,
   and a JSON spec of the process.
3. The guest gets one multiport virtio console, with ports named `stdout`,
   `stderr`, `stdin` and `control`; a virtio network device, unless
   `--network none`; and one read-only virtio-blk disk per layer. Each disk's
   serial number names its layer, so init does not depend on probe order.
   Each port's output is written to the host by a goroutine of its own, and
   input is delivered to the guest from another, so the vCPU never waits on
   the host's console or pipes.
4. The network device's far end is a user-mode TCP/IP stack inside runwhp,
   gVisor's netstack. Frames the guest sends are copied out on the vCPU
   thread and processed on other goroutines; frames for the guest go into
   receive buffers it has posted, or wait in a bounded backlog. The link's
   MTU is 65520, so bulk data crosses in few, large frames. The stack takes
   packets for any address, and for each new TCP connection or UDP flow it
   opens a host socket to the same destination and copies between the two.
   Published ports are host listeners whose connections the stack opens
   towards the guest.
5. `runwhp-init` mounts the layers under an overlay with a tmpfs upper layer,
   adds `/proc`, `/sys`, a minimal `/dev`, a hostname and `/etc/hosts`,
   configures `eth0` and `/etc/resolv.conf` when there is a network, and runs
   the process chrooted into it. The process's stdin, stdout and stderr are
   pipes, or with `-t` a pseudo-terminal from the container's own devpts,
   and init relays them to and from the ports. A port can be open only once
   at a time, so handing the process the ports themselves would break
   `/dev/stdout` and similar paths.
6. The `control` port carries JSON lines both ways: signals and terminal
   sizes from the host, and the exit status from init. When the process
   exits, init kills whatever is left, waits for the last output to reach
   the ports, writes the status, and resets the machine, which ends the VMM.

### Code layout

| Path | Contents |
| --- | --- |
| [`cmd/runwhp`](cmd/runwhp) | Command line |
| [`internal/whp`](internal/whp) | Bindings for `WinHvPlatform.dll` and `WinHvEmulation.dll` |
| [`internal/vm`](internal/vm) | Partition setup, vCPU loop, serial port, RTC stub, I/O APIC |
| [`internal/boot`](internal/boot) | Kernel loader, page tables, ACPI tables, initramfs builder. Builds on any OS |
| [`internal/virtio`](internal/virtio) | virtio-mmio transport, block device, multiport console, network device. Builds on any OS |
| [`internal/netstack`](internal/netstack) | The host end of the container's network: gVisor's netstack, TCP and UDP forwarding, DNS, published ports. Builds on any OS |
| [`internal/image`](internal/image), [`internal/layer`](internal/layer) | Registry pulls, image records, layer conversion and cache |
| [`internal/guestspec`](internal/guestspec) | The contract between host and guest |
| [`internal/passwd`](internal/passwd) | User and group resolution |
| [`guest/init`](guest/init) | `runwhp-init`, PID 1 in the guest |
| [`test/e2e`](test/e2e) | End-to-end tests against WHP |

`go test ./...` runs the unit tests anywhere. The end-to-end tests need a
build, WHP, the module and network access, and run with `RUNWHP_E2E=1`:

```powershell
$env:RUNWHP_E2E = '1'; go test -count=1 ./test/e2e
```

## Proposed design

One small virtual machine per container, with the VMM process acting as the
container's supervisor. This matches Quasidock's existing model of one
supervisor process and one Job Object per container, with no daemon.

| Concern | Approach | State |
| --- | --- | --- |
| Hypervisor | WHP from Go through `WinHvPlatform.dll`, no cgo | Done |
| Boot | Direct kernel boot (Linux 64-bit boot protocol), no firmware | Done |
| Clock and timers | Hyper-V enlightenments supply TSC and APIC timer frequencies | Done |
| Interrupts | Hypervisor's local APIC plus an emulated 64-pin I/O APIC | Done, edge-triggered only |
| Device discovery | Hardware-reduced ACPI with a generated DSDT | Done |
| Root filesystem | One read-only ext4 image per layer on virtio-blk, overlayfs in the guest, writable scratch disk | Done, but the upper layer is a tmpfs, not a scratch disk |
| Layer conversion | `tar2ext4`, cached per layer digest | Done |
| Container I/O | Multiport virtio console with named ports for stdout, stderr, stdin and control; pipes or a pty in the guest | Done, including `-i`, `-t`, resize and signals |
| Guest agent | Tiny init as PID 1 that mounts the overlay and execs the process, instead of hcsshim's GCS plus runc | Done (`runwhp-init`) |
| Bind mounts and volumes | 9p server in Go on the host, carried over vsock | Not started |
| Networking | User-mode network stack in the VMM (gVisor netstack); published ports are loopback listeners | Done, IPv4 only, TCP ports only |
| Command line | runc-compatible `create`, `start`, `state`, `kill`, `delete`, `exec` | `run` and `pull` only |
| Kernel | Our own build with virtio drivers built in, shipped as a signed OCI artifact | Not started; the WSL kernel stands in |

### Relationship to Quasidock

`runwhp` is meant to stand alone, with no dependency on Quasidock's store or
recipes. Quasidock could later call it for images that have no native Windows
build, or when [`quasidock convert`](dockerfile-conversion.md) rejects a
Dockerfile. Whether Quasidock should do that is undecided.

### Out of scope for now

Linux GUI applications, GPU access, ARM64 hosts, and running many containers in
one shared VM.

## Iteration 1 results

Measured on the test machine, unelevated, from process start to exit,
including VM teardown, with the image already cached.

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true` | 723-729 ms over three runs |
| `run python:3.12-slim python3 -c 'print(1)'` (4 layers) | 770 ms |
| First run of `python:3.12-slim`, including pull and conversion | 14.5 s |
| 200 MB to stdout in 32 KiB writes, redirected to a file | 1.25 s in total |
| 200 MB to stdout from `head -c`, which writes in small pieces | 21 s |
| 200 MB in 32 KiB writes, piped into `wc` in Git Bash | 32.5 s |

The end-to-end tests cover image defaults and overrides, separate streams,
exit codes 3, 126, 127, 130 and 137, users, three parallel runs sharing layer
files, a real multi-layer image, and whiteouts and opaque directories across
layers.

Output costs a few VM exits per guest `write()` (the notification and the
interrupt that follows it), about 50-100 µs. Large writes therefore run at hundreds of MB/s
and many small writes are slow. Host-side writes happen on the vCPU thread, so
a slow reader, such as an MSYS pipe, stalls the guest.

## Iteration 2 results

Measured on the test machine as for iteration 1, after iteration 2's changes.

| Measurement | Iteration 1 | Iteration 2 |
| --- | --- | --- |
| `run alpine:3.20 true` | 723-729 ms | 707-709 ms over three runs |
| `run python:3.12-slim python3 -c 'print(1)'` (4 layers) | 770 ms | 762 ms |
| 200 MB to stdout in 32 KiB writes, redirected to a file | 1.25 s | 1.16-1.21 s |
| 200 MB to stdout from `head -c`, which writes in small pieces | 21 s | 3.0 s |
| 200 MB in 32 KiB writes, piped into `wc` in Git Bash | 32.5 s | 1.2 s |
| 200 MB to stdin with `-i`, redirected from a file | | 4.4-4.5 s |
| 200 MB to stdin with `-i`, piped from `cat` in Git Bash | | 24.5 s; a plain Go program reading the same pipe takes 23.8 s |

Small writes got faster because init now reads the process's output from a
pipe in pieces of up to 64 KiB, so many small writes reach the port as few
large ones. The Git Bash pipe no longer holds up the guest, because output is
written to the host off the vCPU thread.

The new end-to-end tests cover:

- piped stdin, empty stdin and 10 MB of stdin with its checksum
- stdin that arrives while the guest is idle, which shows that an interrupt
  raised from another thread wakes the halted vCPU
- stderr flowing while stdout is not being read
- reopening `/dev/stdout`, `/dev/stderr` and `/dev/stdin`
- Ctrl+Break against a container that does not handle SIGINT, one that traps
  it, and one that ignores it until the second Ctrl+Break
- `-it` on a Windows pseudo console: `tty`, the size and a resize, `TERM`,
  Ctrl+C as a key, and the exit code
- Ctrl+C during a line-mode `-i` read

## Iteration 3 results

Measured on the test machine. Boot times are from process start to exit, as
before, but taken with a different timer than the earlier tables, so compare
them with each other rather than with iteration 2's. Transfer times leave out
booting: they are timed inside the guest with `time`, or for published ports
on the host's end of the connection.

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true` with the network | 655-664 ms over three runs |
| `run alpine:3.20 true` with `--network none` | 654-658 ms over three runs |
| 200 MB from a host server into the container through `host-gateway` (`wget`) | 0.61-1.14 s over five runs |
| 200 MB from the container to a host server through `host-gateway` (`dd \| nc`) | 2.26-2.31 s |
| 200 MB into the container through `-p` | 0.75-0.80 s |
| 200 MB out of the container through `-p` | 2.28-2.44 s |
| 100 sequential `wget` runs against a host server | 0.27-0.28 s |
| Cold DNS lookups through the host's resolver | 50-180 ms each |
| `apk add curl` from the internet, including boot | 2.1-3.0 s |

The network costs nothing measurable at boot. Sending from the container is
about three times slower than receiving because it is bound by VM exits.
Sending 200 MB took about 16,000 frames each way: the stack acknowledges
almost every segment, and busybox `nc` writes small pieces. That came to
81,000 MMIO exits and 32,000 interrupts. Receiving the same amount took 6,500
frames of about 32 KiB, which the guest acknowledges in batches. Notification
suppression (`VIRTIO_F_EVENT_IDX`) is the usual cure and is not implemented
yet.

gVisor's `go` branch, the one Go programs import, has required Go 1.27 since
October 2026. runwhp pins its last commit that builds with Go 1.26
(2026-10-08), so it keeps building with the same toolchain.

The new tests cover:

- the virtio network device against a simulated driver: headers stripped
  from chains of several descriptors, frames split over mergeable receive
  buffers, frames held until the guest posts buffers and dropped beyond the
  backlog, and reset
- the stack against a second gVisor stack standing in for the guest: TCP
  both ways with half-close, a refused connection, which destinations are
  allowed, `host-gateway`, UDP including a 30 KB datagram, DNS over UDP and
  TCP for each record type, truncation and EDNS, published ports, a busy
  host port, and closing with connections open
- end to end: a host server reached through `host-gateway` and refused
  without it, 20 MB downloaded with its checksum, DNS and TLS to the
  internet, `--network none`, `-p` in both directions with a 20 MB checksum,
  and a busy host port

## Boot spike

The spike lives in [`spike/`](spike/) and is kept as a reference; the runtime
in `internal/` grew out of it. It is about 1,800 lines of Go, guest init
included, with no dependencies beyond the standard library.

| File | Contents |
| --- | --- |
| [`whp_windows.go`](spike/whp_windows.go) | Bindings for `WinHvPlatform.dll` and `WinHvEmulation.dll` |
| [`boot_windows.go`](spike/boot_windows.go) | Kernel loader, page tables, ACPI tables, initramfs builder |
| [`devices_windows.go`](spike/devices_windows.go) | MMIO emulation glue, I/O APIC, virtio-mmio block device |
| [`main_windows.go`](spike/main_windows.go) | Partition setup, vCPU run loop, serial port, RTC stub |
| [`init/main_linux.go`](spike/init/main_linux.go) | Guest PID 1: module loading and the tests below |
| [`fixtures.sh`](spike/fixtures.sh) | Builds the test disk image and copies the block driver, inside WSL |

It boots the WSL2 kernel already on the machine
(`C:\Program Files\WSL\tools\kernel`, 6.6.114.1) as a stand-in for a kernel of
our own.

### How the guest is put together

- **Partition**: one vCPU, local APIC emulated by the hypervisor in xAPIC mode,
  unimplemented MSRs read as zero.
- **Enlightenments**: the Hyper-V synthetic features for hypervisor presence,
  the reference counter and TSC page, hypercall and VP-index registers,
  interrupt-control registers, and the frequency MSRs.
- **Memory**: one `VirtualAlloc` block mapped at guest address 0.
- **Boot**: the bzImage payload is copied to its preferred address and entered
  at `startup_64` in long mode, with identity page tables for the first 4 GiB
  and `boot_params` filled in by the loader.
- **ACPI**: RSDP, XSDT, FADT, MADT, and a DSDT. The FADT declares
  hardware-reduced ACPI and a reset register. The MADT lists one local APIC and
  one I/O APIC. The DSDT is hand-assembled bytecode declaring the serial port
  and each virtio device with its memory window and interrupt.
- **Devices**: a 16550A serial port at `0x3f8` (output only), a CMOS clock
  stub, an I/O APIC, and virtio-mmio block devices. Memory-mapped accesses are
  decoded by Windows' own instruction emulator, which calls back into Go.

| Guest address | Use |
| --- | --- |
| `0x1000` | GDT |
| `0x7000` | `boot_params` |
| `0x9000`-`0xefff` | Page tables |
| `0x20000` | Kernel command line |
| `0xe0000` | ACPI tables |
| `0x1000000` | Kernel |
| Top of RAM | Initramfs |
| `0xd0000000` + `0x1000` per device | virtio-mmio windows, interrupts 5 and up |
| `0xfec00000` | I/O APIC |
| `0xfee00000` | Local APIC |

### What the guest does

The initramfs holds a statically linked Go program as `/init`. It:

1. Prints kernel, CPU, memory, and clock source, and checks that a 250 ms sleep
   takes 250 ms.
2. Loads `virtio_blk.ko` from the initramfs.
3. Mounts the first disk read-only as an ext4 image layer, mounts an overlay
   with a tmpfs upper directory, and runs a process chrooted into it. That
   process reads a file from the layer and writes a new one.
4. Writes 128 MiB of random data to the second disk with `O_DIRECT`, reads it
   back, and compares SHA-256 digests.
5. Resets the machine through the ACPI reset register, which stops the VMM.

### Build and run

From the repository root, in PowerShell:

```powershell
go build -o out/spike/runwhp-spike.exe ./spike

$env:GOOS = 'linux'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags="-s -w" -o out/spike/init ./spike/init
$env:GOOS = $null; $env:CGO_ENABLED = $null
```

Kernel and userspace only:

```powershell
.\out\spike\runwhp-spike.exe -init out\spike\init
```

With disks. The fixture script runs inside WSL and needs `e2fsprogs`. Run it
from the repository root; WSL starts in the same directory, so relative paths
work:

```powershell
wsl -e sh spike/fixtures.sh out/spike
$f = [IO.File]::Create("$PWD\out\spike\scratch.img"); $f.SetLength(256MB); $f.Close()

.\out\spike\runwhp-spike.exe -init out\spike\init -module out\spike\virtio_blk.ko `
    -disk out\spike\layer.ext4:ro -disk out\spike\scratch.img
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-kernel` | `C:\Program Files\WSL\tools\kernel` | bzImage to boot |
| `-init` | none | Static Linux executable to run as `/init` |
| `-module` | none | Kernel module copied to `/modules` for `/init` to load; repeatable |
| `-disk` | none | Raw image attached as a virtio block device; `:ro` suffix for read-only; repeatable |
| `-mem` | `512` | Guest RAM in MiB |
| `-cmdline` | `console=ttyS0 earlyprintk=serial,ttyS0,115200 nokaslr panic=-1 reboot=acpi` | Kernel command line |
| `-timeout` | `30s` | Stop the guest after this long |
| `-hv` | `true` | Expose Hyper-V enlightenments |

The guest console goes to stdout. Lines prefixed `[vmm]` on stderr are
milestones and exit statistics from the VMM.

### Results

Measured on the test machine, from process start, unelevated.

| Measurement | Result |
| --- | --- |
| Partition created, memory mapped, kernel loaded | 25 ms |
| First kernel console line | about 90 ms |
| Go init running, quiet console (`loglevel=5` or lower) | 557-585 ms over eight runs |
| Go init running, full boot log on serial | about 750 ms |
| Go init running, 2 GiB guest, quiet console | about 650 ms |
| Kernel-reported uptime at init, quiet console | 0.23 s |
| 250 ms sleep measured in the guest | 250-251 ms |
| Disk write, 128 MiB | 210-317 MiB/s over five runs |
| Disk read, 128 MiB | 313-677 MiB/s over five runs |
| Full run with both disk tests | 1.55-1.78 s |

All five full runs passed: the overlay behaved as expected and the digests
matched every time. The disk rates include hashing in the guest and are served
synchronously on the vCPU thread, so they are a lower bound.

Roughly 300 ms of the time to userspace passes before the kernel's own clock
starts, which covers decompressing the kernel and early memory setup.

## Findings

- **Hyper-V enlightenments are required.** With `-hv=false` the kernel cannot
  calibrate its clock, because there is no PIT, HPET, or ACPI timer, and it
  never reaches userspace. With them on, Linux reads the TSC and APIC timer
  frequencies from the hypervisor and needs no legacy timer hardware at all.
- **Devices have to be described through ACPI.** The WSL kernel is built
  without MP-table parsing and without command-line virtio-mmio devices. A
  small ACPI bytecode builder is enough; the alternative is virtio over PCI.
- **The I/O APIC is ours to emulate.** WHP emulates only the local APIC.
  Edge-triggered delivery through `WHvRequestInterrupt` was sufficient for the
  serial port and virtio-blk.
- **`WinHvEmulation.dll` removes the need for an x86 decoder.** Its callbacks
  work from Go through `syscall.NewCallback`. Each memory-mapped access costs
  several round trips, so a real runtime would want a fast path for the common
  instructions.
- **Linux 6.6 reads the CMOS clock even when ACPI says there is none.** Without
  a stub it retries for over a second during boot.
- **Hardware-reduced ACPI without EFI reboots through BIOS real mode.** The
  kernel then executes whatever is in low memory. Passing `reboot=acpi` makes
  it use the reset register instead.
- **The WSL kernel is the wrong kernel to ship.** `virtio_blk` and the virtio
  vsock transport are modules, and the configuration carries much that a
  container guest does not need.

## Known gaps

- There is no detaching or reattaching, and no way to send signals other
  than SIGINT and SIGTERM.
- The writable layer is a tmpfs in guest RAM. Writes are limited by `--memory`,
  and nothing written survives the run.
- Booting needs the WSL kernel, plus `virtio_blk.ko` copied out of a WSL
  distro whose kernel version matches.
- An image can have at most 57 non-empty layers, or 58 with
  `--network none`: each device takes an I/O APIC line.
- The network is IPv4 only and carries no ICMP. Only TCP ports can be
  published, each with an explicit host port, and there is no `-P`, `--dns`
  or `--dns-search`.
- The network device has neither notification suppression (EVENT_IDX) nor
  checksum and segmentation offloads, so sending from the container is
  limited to about 90 MB/s on the test machine.
- The cache only grows: there is no `images` or `rmi` yet.
- Interrupts are edge-triggered only; level-triggered lines and EOI tracking
  are not implemented.
- One vCPU. Starting further processors is untested.
- Disk emulation blocks the vCPU. Each MMIO access also takes several round
  trips through `WinHvEmulation.dll`; a fast path for common instructions
  would cut the per-write output cost.
- Guest memory is committed up front and never returned to Windows.
- The guest clock starts at whatever the RTC stub reports and is not kept in
  step with the host afterwards.
- String port I/O is not handled and stops the VMM with an error.
- It is unknown whether hosted Windows CI runners expose WHP, so the
  end-to-end tests run only by hand.

## Open questions

- Is `runwhp` a general OCI runtime that containerd or nerdctl can drive, or a
  backend used only by Quasidock? This decides how much of the runc command
  line is worth implementing.
- Is a one-time administrator step acceptable on machines where the hypervisor
  is off, or must there be a fallback that needs none?
- virtio-mmio with generated ACPI, as spiked, or virtio over PCI with MSI-X,
  which avoids both the I/O APIC and the bytecode but needs PCI emulation?
- How should host directories behave when shared into the guest, given NTFS
  case-insensitivity and the lack of Unix permissions and symlinks?
