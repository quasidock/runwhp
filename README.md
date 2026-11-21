# runwhp - lightweight, rootless container runtime for Windows

`runwhp` is an experimental OCI runtime that runs Linux containers on any Windows
edition, including Home. It drives the Windows Hypervisor Platform (WHP)
directly from an ordinary user process, so it needs no Hyper-V role, no WSL, no
HCS, and no administrator rights at run time.

## Status

`runwhp` is experimental. It runs Linux containers from registry images,
interactively or not, with a network and the host's files, on a kernel of
its own and on several processors.

- **Iteration 6** (2026-11-21): containers get more than one vCPU: the
  host's logical processors, up to 4, unless `--cpus` asks for more, up to
  64. The guest sees them as cores of one processor sharing its cache, so
  its scheduler spreads work over all of them. A cold `go build` of runwhp
  over bind mounts takes 18 s with 16 vCPUs, as long as on Windows itself,
  against 63 s with one, and starting 16 costs about 50 ms. See
  [Usage](#usage) and [Iteration 6 results](#iteration-6-results).
- **Iteration 5** (2026-11-21): `-v` and `--mount` bind host directories and
  files into the container. runwhp serves them itself over virtio-fs, and
  keeps the owners, modes, symlinks and special files the container makes in
  the format WSL uses, so WSL sees them the same way. See
  [Host files](#host-files) and [Iteration 5 results](#iteration-5-results).
- **Iteration 4** (2026-11-20): runwhp boots its own kernel, Linux 6.18 with
  everything it needs built in, fetched once from ghcr.io and pinned by
  digest. WSL is no longer needed, nor is copying a driver out of it, and
  `run alpine:3.20 true` takes under 300 ms instead of over 700. See
  [Kernel](#kernel) and [Iteration 4 results](#iteration-4-results).
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
- **Not started**: named volumes and the runc-style lifecycle commands.

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

runwhp boots a kernel of its own, which its first `run` or `pull` fetches
from `ghcr.io/quasidock/runwhp-kernel` (about 8 MB). WSL is not needed.

## Usage

Build, from the repository root in PowerShell. This produces `out\runwhp.exe`
and the guest init `out\runwhp-init`, which must stay next to it:

```powershell
.\build.ps1
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
.\out\runwhp.exe run -v ${PWD}:/src -w /src alpine:3.20 ls -l
.\out\runwhp.exe run --mount type=bind,source=C:\data,target=/data,readonly alpine:3.20 du -sh /data
.\out\runwhp.exe run --cpus 8 -m 2g -v ${PWD}:/src -w /src golang:1.26 go build ./...
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
| `-m`, `--memory` | `512m` | Guest RAM, `128m` to `3328m` |
| `--cpus` | the host's, up to 4 | vCPUs, a whole number up to the host's logical processors, at most 64. Unlike Docker's `--cpus`, it counts processors rather than setting a quota, so `1.5` is refused. Builds size their jobs by `nproc`, so raise `-m` with it |
| `--hostname` | random | Hostname, also written to `/etc/hostname` and `/etc/hosts` |
| `--network`, `--net` | `user` | `user` gives the container a network through runwhp; `none` leaves it only loopback |
| `-p`, `--publish` | | `[IP:]HOSTPORT:PORT[/tcp]`: listen on the host and forward to the container's TCP port; repeatable. IP defaults to `127.0.0.1` |
| `--add-host` | | `NAME:IP` or `NAME=IP`, added to `/etc/hosts`; repeatable. `NAME:host-gateway` also lets the container reach the host's `127.0.0.1` at that name |
| `-v`, `--volume` | | `HOSTPATH:PATH[:ro]`: bind a host directory or file at `PATH`; repeatable. A missing `HOSTPATH` is made as a directory. See [Host files](#host-files) |
| `--mount` | | `type=bind,source=HOSTPATH,target=PATH[,readonly]`: the same, in Docker's long form; `HOSTPATH` must exist |
| `--storage-opt` | `size=64g` | `size=SIZE`: how large the container's writable layer may grow, `1g` to `16t`. See [Storage](#storage) |
| `--pull` | `missing` | When to pull the image: `missing`, `always` or `never`. `never` also keeps runwhp from fetching its kernel |
| `--kernel` | runwhp's own | bzImage to boot instead; see [Kernel](#kernel) |
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

Images, layers and runwhp's kernel are cached in `%LOCALAPPDATA%\runwhp`, or
in `RUNWHP_ROOT` when it is set. `runwhp pull` fetches the kernel as well as
the image, so a machine prepared with it can run that image offline.
Registry credentials come from Docker's configuration and credential helpers.
Only `linux/amd64` images run. Nothing is ever removed from the cache yet.

In Git Bash, set `MSYS_NO_PATHCONV=1`, or MSYS rewrites container paths such
as `/tmp` into Windows paths before runwhp sees them; then
`-v "$(pwd -W):/src"` binds the current directory. Under mintty, the
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

### Host files

`-v HOSTPATH:PATH` makes a host directory or file appear at `PATH` in the
container. Changes either side makes show on the other.

- `HOSTPATH` is an absolute Windows path (`C:\src`, `C:/src`,
  `\\server\share\dir`), a path starting with a dot (`.`, `.\src`), which is
  relative to the current directory, or Git Bash's `/c/src`. A name without
  a path, such as `data`, would be a named volume, which runwhp does not have
  yet.
- `-v` makes a missing `HOSTPATH` as a directory, as Docker does; `--mount`
  requires it to exist.
- `PATH` must be absolute, and cannot be `/` or lie under `/proc` or `/sys`.
  A missing `PATH` is made in the container; if it leads through a link in
  the image, such as alpine's `/var/run`, the mount goes where the link
  leads. Mounts inside mounts work whatever order they are given in. A file
  bound over one runwhp writes, such as `/etc/hosts`, wins.
- `:ro` makes the mount read-only, and `:rw`, the default, writable. `z`,
  `Z`, `cached`, `delegated` and `consistent` are accepted and ignored.

**Owners and modes.** Windows has no Unix owners or modes. A file that has
never had them shows as belonging to the container's user, the one `-u` or
the image's `USER` names, with mode 755. A file Windows marks read-only
shows without write bits. Everything the container sets is kept: files it
makes belong to the process that made them, and `chmod`, `chown`, symlinks,
FIFOs, sockets and device nodes survive the run. runwhp stores them as WSL
does with its `metadata` mount option, in the extended attributes `$LXUID`,
`$LXGID`, `$LXMOD` and `$LXDEV` and in WSL's reparse points, so WSL shows
the same owners, modes and links. Taking away the owner's write bit also
marks the file read-only for Windows. On a volume without extended
attributes, such as FAT or some network shares, `chmod` and `chown` succeed
and change nothing.

**Links.** A symlink made in the container is a WSL symlink, which Windows
programs do not follow. Windows symlinks and junctions show as symlinks: one
pointing into the same mount shows its container path, and any other its
Windows path, which leads nowhere in the container. runwhp never follows a
link on the host, so no link reaches outside what was mounted. Hard links
work within a mount.

**Names.** Lookups are case-insensitive, as on the host, unless the directory
has been made case-sensitive with `fsutil file setCaseSensitiveInfo`.
Listings show names as stored. Characters Windows forbids in names
(`\ : * ? " < > |`) are stored as WSL stores them, as U+F000 plus the
character.

**Caching.** The container caches names and attributes for a second, and file
contents until a file's size or modification time changes, so a change made
on the host shows within about a second. Host changes raise no inotify
events in the container, so file watchers there must poll.

Removing a file that is open works as on Linux: the name goes at once and
the open file stays readable. A rename replaces the target, even a read-only
one. File locks are kept in the guest and are invisible to host programs.

### Storage

Whatever the container writes outside its mounts goes to its writable layer,
which lives on a scratch disk: a sparse file in the store's `scratch`
directory holding an ext4 file system that runwhp makes for the run and
deletes when the run ends. Windows deletes it even when runwhp is killed,
and the next run removes one the machine stopped under.

- The writable layer is limited by the scratch disk's size, 64 GiB unless
  `--storage-opt size=` says otherwise, and by the host's free space, not by
  `--memory`. A full host disk shows in the container as I/O errors.
- The file takes only what is written. Space the container frees goes back
  to the host during the run: the guest discards the blocks, and runwhp
  punches them out of the file. NTFS does this in 64 KiB units, ReFS in
  clusters.
- `fsync` in the writable layer returns without waiting for the host's
  disk, since nothing there outlives the run. Formatting the disk takes
  about 3 ms, while the kernel loads, and deleting it about 4 ms at exit.
- On a store whose volume has no sparse files, such as FAT or exFAT, runwhp
  says so and keeps the writable layer in guest memory.

### Kernel

runwhp boots unmodified upstream Linux, currently 6.18.64 (a longterm
release), built for it with everything built in and no modules. The
configuration, [`kernel/runwhp.config`](kernel/runwhp.config), applies over
`allnoconfig`, so anything it does not name stays off, and has four groups:

| Group | What |
| --- | --- |
| Booting on runwhp's VMM | Hyper-V clock and APIC enlightenments without VMBus, ACPI (which needs PCI support, though runwhp boots with `pci=off`), virtio-mmio, the COM1 console, an uncompressed initramfs |
| runwhp-init | ext4, overlayfs, tmpfs with xattrs, devtmpfs, virtio block, console and network devices, IPv4 and Unix sockets |
| Containers | Namespaces, cgroup v2 controllers, seccomp, BPF, IPv6, 32-bit binaries, `binfmt_misc`, FUSE, TUN, veth, bridges, nftables and iptables (both over nftables and legacy) |
| Later work | Host file sharing: virtio-fs, which bind mounts now use, and vsock and 9p, which nothing uses yet; the virtio balloon with free page reporting |

CPU vulnerability mitigations stay at upstream's defaults. The kernel is
LZ4-compressed, which decompresses fastest.

**Where it comes from.** CI publishes each build to
`ghcr.io/quasidock/runwhp-kernel` as an OCI artifact, tagged with the
upstream version and a build revision (`6.18.64-1`).
[`internal/kernel/pin.go`](internal/kernel/pin.go) names the build this
runwhp boots by its manifest digest, which stands in for a signature: runwhp
fetches only that digest and checks every byte against it. The first `run`
or `pull` downloads the bzImage, anonymously, into
`kernels\sha256\<digest>\bzImage` in the store, and it is never fetched
again. `--pull never` does not fetch it at all.

**Another kernel.** `--kernel PATH`, or the `RUNWHP_KERNEL` environment
variable, boots any other bzImage. The WSL kernel builds `virtio_blk` as a
module, which runwhp then has to load from its initramfs; copy it out of a WSL
distro and pass it with the development flag `--module`:

```powershell
wsl -e sh -c 'cp /lib/modules/$(uname -r)/kernel/drivers/block/virtio_blk.ko /mnt/c/temp/'
.\out\runwhp.exe run --kernel 'C:\Program Files\WSL\tools\kernel' --module C:\temp\virtio_blk.ko alpine:3.20 uname -r
```

**Building it.** [`kernel/Dockerfile`](kernel/Dockerfile) builds the kernel
from kernel.org's tarball, checked against the checksum in kernel.org's
signed `sha256sums.asc`, in a Debian image pinned by digest whose packages
come from a fixed `snapshot.debian.org` date, with the build's timestamp,
user and host fixed. The same inputs should give the same bzImage. Locally
it needs Docker inside WSL:

```powershell
.\kernel\build.ps1                  # out\kernel\bzImage, in a few minutes
$env:RUNWHP_KERNEL = "$PWD\out\kernel\bzImage"   # boot it instead of the pinned one
```

[`kernel/full.config`](kernel/full.config) records the configuration
`runwhp.config` resolves to, and the build fails if they disagree, so every
consequence of a change, or of a new upstream release, shows in review.
`.\kernel\build.ps1 -Target config` regenerates it. The build also fails if a
setting in `runwhp.config` does not survive, which is how Kconfig reports a
missing dependency or a renamed option: it does not.

**Publishing it.** [`.github/workflows/kernel.yml`](.github/workflows/kernel.yml)
builds the kernel for every pull request that touches it, and publishes it
from `main`. A tag never moves: if it already names a different build, the
job fails, and `KERNEL_REVISION` in the Dockerfile has to go up. Rerunning it
on an unchanged build publishes nothing. To move runwhp to a new build, copy
the tag and digest from the job's summary into `pin.go`. A new package on
ghcr.io starts private, and runwhp pulls anonymously, so the package has to
be made public once in its settings; the job checks for that.

**Source.** Besides the bzImage, the artifact carries the kernel's
`.config`, a tar of `kernel/`, and kernel.org's tarball, so the source of
the binary is published with it, as GPLv2 asks. runwhp downloads only the
bzImage.

### How a run works

1. The image's layers are pulled and each one is converted to an ext4 image
   with `tar2ext4`, with OCI whiteouts turned into overlayfs whiteouts. Layers
   are cached by diffID and verified against it; empty layers are skipped.
2. runwhp loads its kernel from the store, fetching it first if this is the
   first run, and writes an initramfs holding `runwhp-init` and a JSON spec
   of the process. Meanwhile it makes the run's scratch disk: a sparse file
   in the store, formatted as an empty ext4 file system by runwhp itself,
   which writes a few hundred KiB of it.
3. The guest gets the vCPUs `--cpus` asks for, each run by a thread of its
   own. runwhp starts only the first. The others wait until the guest's
   kernel starts them with INIT and SIPI, which the hypervisor's local APIC
   emulation delivers, as it does every interrupt between processors.
   runwhp describes the vCPUs to the guest, in ACPI's MADT and in CPUID, as
   single-threaded cores of one package that share their last-level cache.
   The I/O APIC sends each device's interrupt to the CPU the guest's kernel
   chose for it.
4. The guest gets one multiport virtio console, with ports named `stdout`,
   `stderr`, `stdin` and `control`; a virtio network device, unless
   `--network none`; a virtio-fs device, with `-v` or `--mount`; one
   read-only virtio-blk disk per layer; and the scratch disk. Each disk's
   serial number names it, so init does not depend on probe order. Disk
   requests are served on goroutines, up to 16 at a time per disk, through
   overlapped I/O, and complete in whatever order they finish.
   Each port's output is written to the host by a goroutine of its own, and
   input is delivered to the guest from another, so no vCPU ever waits on
   the host's console or pipes.
5. The network device's far end is a user-mode TCP/IP stack inside runwhp,
   gVisor's netstack. Frames the guest sends are copied out on the thread
   of the vCPU that sent them and processed on other goroutines; frames for
   the guest go into receive buffers it has posted, or wait in a bounded
   backlog. The link's MTU is 65520, so bulk data crosses in few, large
   frames. The stack takes packets for any address, and for each new TCP
   connection or UDP flow it opens a host socket to the same destination
   and copies between the two. Published ports are host listeners whose
   connections the stack opens towards the guest.
6. The virtio-fs device's far end is a FUSE server inside runwhp, which
   serves every mount at once: its root, which only init sees, holds them as
   entries `0`, `1`, and so on. Requests are served on goroutines, up to 16
   at a time, through Windows' native API, and complete in whatever order
   they finish. A read or write carries up to 512 KiB.
7. `runwhp-init` mounts the layers under an overlay whose upper layer is on
   the scratch disk,
   adds `/proc`, `/sys`, a minimal `/dev`, a hostname and `/etc/hosts`,
   configures `eth0` and `/etc/resolv.conf` when there is a network,
   resolves the container's user, mounts the virtio-fs device, tells it who
   owns host files without an owner, and binds each mount onto its target.
   It then moves the overlay over the initramfs as the root, as `switch_root`
   does, which leaves the virtio-fs root out of the container's reach, and
   runs the process in it. The process's stdin, stdout and stderr are
   pipes, or with `-t` a pseudo-terminal from the container's own devpts,
   and init relays them to and from the ports. A port can be open only once
   at a time, so handing the process the ports themselves would break
   `/dev/stdout` and similar paths.
8. The `control` port carries JSON lines both ways: signals and terminal
   sizes from the host, and the exit status from init. When the process
   exits, init kills whatever is left, waits for the last output to reach
   the ports, writes the status, and resets the machine, which ends the VMM.
   It writes back the bind mounts' file system first, but not the scratch
   disk's, which is about to be thrown away.

### Code layout

| Path | Contents |
| --- | --- |
| [`cmd/runwhp`](cmd/runwhp) | Command line |
| [`internal/whp`](internal/whp) | Bindings for `WinHvPlatform.dll` and `WinHvEmulation.dll` |
| [`internal/vm`](internal/vm) | Partition setup, vCPU loops, processor topology, serial port, RTC stub, I/O APIC |
| [`internal/boot`](internal/boot) | Kernel loader, page tables, ACPI tables, initramfs builder. Builds on any OS |
| [`internal/virtio`](internal/virtio) | virtio-mmio transport, block device, multiport console, network device, file system device. Builds on any OS |
| [`internal/netstack`](internal/netstack) | The host end of the container's network: gVisor's netstack, TCP and UDP forwarding, DNS, published ports. Builds on any OS |
| [`internal/fuse`](internal/fuse) | The FUSE protocol as virtio-fs speaks it, served from a file system interface. Builds on any OS |
| [`internal/disk`](internal/disk) | The files behind virtual disks: overlapped I/O from many goroutines, sparse files, freeing ranges, exclusive and delete-on-close opens |
| [`internal/ext4`](internal/ext4) | Makes empty ext4 file systems as mke2fs would, and reads back whether one was left clean. Builds on any OS |
| [`internal/hostfs`](internal/hostfs) | The host end of bind mounts: Windows files served as that file system, with WSL's metadata |
| [`internal/image`](internal/image), [`internal/layer`](internal/layer) | Registry pulls, image records, layer conversion and cache |
| [`internal/kernel`](internal/kernel) | The kernel artifact's format, the pinned build, and fetching it into the store |
| [`kernel`](kernel) | The kernel's configuration and build; [`kernel/push`](kernel/push) publishes it |
| [`internal/guestspec`](internal/guestspec) | The contract between host and guest |
| [`internal/passwd`](internal/passwd) | User and group resolution |
| [`guest/init`](guest/init) | `runwhp-init`, PID 1 in the guest |
| [`test/e2e`](test/e2e) | End-to-end tests against WHP |

`go test ./...` runs the unit tests anywhere. The end-to-end tests need a
build, WHP and network access, and run with `RUNWHP_E2E=1`:

```powershell
$env:RUNWHP_E2E = '1'; go test -count=1 ./test/e2e
```

They boot whatever runwhp would: the pinned kernel, or the one
`RUNWHP_KERNEL` names. `RUNWHP_E2E_RUN_FLAGS` adds flags, separated by
semicolons, to every `runwhp run`, such as `--kernel` and `--module` for the
WSL kernel; tests of runwhp's own kernel are then skipped.

## Proposed design

One small virtual machine per container, with the VMM process acting as the
container's supervisor, with no daemon.

| Concern | Approach | State |
| --- | --- | --- |
| Hypervisor | WHP from Go through `WinHvPlatform.dll`, no cgo | Done |
| Boot | Direct kernel boot (Linux 64-bit boot protocol), no firmware | Done |
| Clock and timers | Hyper-V enlightenments supply TSC and APIC timer frequencies | Done |
| Interrupts | Hypervisor's local APIC plus an emulated 64-pin I/O APIC | Done, edge-triggered only |
| Processors | One thread per vCPU; the guest starts the others through the hypervisor's local APIC; topology described in CPUID | Done, up to 64 |
| Device discovery | Hardware-reduced ACPI with a generated DSDT | Done |
| Root filesystem | One read-only ext4 image per layer on virtio-blk, overlayfs in the guest, writable scratch disk | Done; the scratch disk is a sparse file, formatted for each run |
| Layer conversion | `tar2ext4`, cached per layer digest | Done |
| Container I/O | Multiport virtio console with named ports for stdout, stderr, stdin and control; pipes or a pty in the guest | Done, including `-i`, `-t`, resize and signals |
| Guest agent | Tiny init as PID 1 that mounts the overlay and execs the process, instead of hcsshim's GCS plus runc | Done (`runwhp-init`) |
| Bind mounts and volumes | virtio-fs, with a FUSE server in Go serving Windows files and keeping Unix metadata as WSL does | Bind mounts done; named volumes not started |
| Networking | User-mode network stack in the VMM (gVisor netstack); published ports are loopback listeners | Done, IPv4 only, TCP ports only |
| Command line | runc-compatible `create`, `start`, `state`, `kill`, `delete`, `exec` | `run` and `pull` only |
| Kernel | Our own build with virtio drivers built in, shipped as a signed OCI artifact | Done: Linux 6.18 LTS on ghcr.io, pinned by digest rather than signed |

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

## Iteration 4 results

Both columns are measured the same way, with the same `runwhp.exe`, on the
test machine, over five runs from Git Bash, so compare them with each other
rather than with earlier tables. The WSL kernel booted with `--kernel` and
`--module`.

| Measurement | WSL kernel 6.6 | runwhp kernel 6.18 |
| --- | --- | --- |
| `run alpine:3.20 true` | 710-719 ms | 269-285 ms |
| `run alpine:3.20 true` with `--network none` | 709-720 ms | 265-271 ms |
| `run python:3.12-slim python3 -c 'print(1)'` (4 layers) | 761-785 ms | 324-329 ms, and 427 ms once |
| Kernel uptime when `/init` starts | 0.42 s | 0.18 s |
| bzImage | 16.0 MB, gzip | 7.7 MB, LZ4 |
| Initramfs | 2494 KiB, with `virtio_blk.ko` | 2433 KiB |
| `MemTotal` and `MemAvailable` with `-m 512m` | 461 and 426 MiB | 486 and 459 MiB |

On an empty store, the first `run alpine:3.20 true` took 5.5-6.4 s, pulling
the image and fetching the kernel; a run that fetched only the kernel took
2.7-2.8 s.

The build is reproducible: a local build, a second one without Docker's
cache, and the one CI published all produced the same bzImage, and the
local and published artifacts have the same manifest digest.

The same configuration with each compression:

| Compression | bzImage | `run alpine:3.20 true` |
| --- | --- | --- |
| LZ4 | 7.7 MB | 276-296 ms |
| zstd | 5.4 MB | 303-320 ms |
| gzip | 6.0 MB | 321-342 ms |

The new tests cover:

- the kernel artifact: a manifest that depends only on its content, and
  fetching the pinned build once, refusing to without permission, rejecting
  the wrong artifact type, a missing or oversized bzImage, one that is not a
  kernel and one whose bytes do not match, and three first runs at once
- which kernel boots: `--kernel`, then `RUNWHP_KERNEL`, then the store, whatever
  the pull policy, and `--module`
- end to end on runwhp's kernel: its release string; namespaces, cgroup v2,
  overlayfs, 9p, virtio-fs, FUSE, `binfmt_misc`, seccomp, vsock, IPv6 on the
  loopback and options in `/proc/config.gz`; `unshare` of a user and PID
  namespace; `#!` scripts; an opaque directory on the writable layer; and a
  wall clock within 5 s of the host's
- end to end from an empty store: the kernel is fetched, `--pull never`
  refuses to fetch it, and `runwhp pull` brings it back

The whole suite passes on runwhp's kernel and, through
`RUNWHP_E2E_RUN_FLAGS`, on the WSL kernel.

## Iteration 5 results

Measured on the test machine with runwhp's kernel, on its NTFS system volume
(`C:`, where the temporary directory is) and a ReFS volume (`E:`).

A bind mount costs almost nothing at start, over five runs from Git Bash:

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true` | 278-280 ms |
| the same with one `-v` | 279-286 ms |
| the same with five `-v` | 282-287 ms |

Moving 200 MB, timed inside the guest with `-m 1g`, over three runs. Reads
follow a `drop_caches`, so they come from the host. The overlay, a tmpfs in
guest RAM, is there for scale:

| Measurement | NTFS | ReFS | Overlay |
| --- | --- | --- | --- |
| `dd` in 1 MiB blocks, then `fsync` | 0.96-1.09 s | 0.97-1.03 s | 0.46-0.47 s |
| `dd` reading it back | 0.21 s | 0.18-0.21 s | 0.01-0.02 s |
| `head -c`, which writes about 820 bytes at a time | 34.7-34.9 s | 32.3-32.9 s | 1.24-1.26 s |

Large reads and writes go at 200 MB/s to 1 GB/s. Small writes are slow
because each `write()` is a request to the host, and a request costs about
100-150 µs: three or four VM exits for the notification and the interrupt,
which the MMIO fast path in [Known gaps](#known-gaps) would cut, and the
Windows call. Writing 20 MB from `dd` took 0.12 s in 64 KiB blocks and
0.8 s in 4 KiB blocks. FUSE's writeback cache would gather small writes
together, but at a cost to host edits; see [Open questions](#open-questions).

A tree of 10,000 small files in 100 directories, one run each. "New
container" is the first run of a command after a fresh boot, "again" the
second in the same container. WSL2's `/mnt/c`, a 9p mount of the same NTFS
volume, is there for comparison:

| Measurement | NTFS | ReFS | WSL2 `/mnt/c` | Overlay |
| --- | --- | --- | --- | --- |
| `tar -x` from the overlay | 40.8 s | 35.9 s | 35.5 s | 0.37 s |
| `find`, new container | 0.36 s | 0.49 s | 0.35 s | 0.02 s |
| `ls -lR` | 0.33 s | 0.43 s | 27.5 s | 0.03 s |
| `git add -A` and `git commit` | 69 s | 53 s | 184 s | 0.9 s |
| `git status`, new container | 0.84-0.94 s | 0.94-1.05 s | 3.96-3.98 s | 0.02-0.03 s |
| `git status` again | 0.13-0.21 s | 0.17-0.33 s | 4.0 s | 0.03 s |
| `rm -rf` | 7.7-16.6 s over three runs | 14.3 s | 26.2 s | |

Reading a tree is fast: the guest caches what it learns, and listings carry
attributes. Making files is slow. `tar -x` made 12 requests per file, and
the host spent 20 of its 41 seconds in Windows calls: creating a file took
0.5 ms, each of the three changes to its owner, mode and times 0.26 ms, and
closing it after a write 0.14 ms. `git add` spent 15 s making 10,000 hard links at 1.5 ms each;
outside the VM, `CreateHardLink` alone takes 0.6-0.7 ms per link on NTFS and
0.24 ms on ReFS.

Statting files with `NtQueryInformationByName` instead of opening them,
8 µs instead of 38 µs per file on NTFS, halved `find` and `ls -lR`.

The new tests cover:

- the virtio-fs device against a simulated driver: its tag, requests and
  replies split over several buffers, requests completing out of order,
  FORGET without a reply, a reset with requests in flight, draining, and
  shared memory regions reading as absent
- the FUSE server against a fake file system: INIT with older and newer
  kernels, entries and negative entries, errors, forgets, reads across
  page-sized buffers, READDIRPLUS counting only the entries that fit, and
  SETATTR
- the host file system, on NTFS and, through `RUNWHP_TEST_DIR`, on ReFS:
  - lookups, case-insensitive ones included, and listings
  - creating, writing, truncating and setting times
  - metadata surviving into a new server, and the read-only attribute
  - removing open and read-only files
  - renames over a file, over an empty directory and to another case
  - hard links
  - WSL symlinks, FIFOs, sockets and device nodes
  - junctions not followed, and their targets translated
  - read-only mounts, file mounts, and forgetting nodes
- parsing `-v` and `--mount`
- end to end:
  - reading and writing both ways, with a 20 MB checksum each way
  - `:ro` and `readonly`
  - file mounts, including over `/etc/hosts`, which stays untouched on the host
  - nested mounts given in either order
  - owners with and without `-u`, and `git` as a non-root user
  - `chmod`, `chown`, a symlink and a FIFO surviving into the next run
  - renames, and removing an open file
  - a host change showing within a second
  - a junction to outside the mount not followed
  - a mount point through an image's link, and a working directory in a mount
  - missing sources and named volumes

The whole suite passes on runwhp's kernel and on the WSL kernel.

## Iteration 6 results

Measured on the test machine, which has 8 cores and 16 threads, with
runwhp's kernel, in one session from Git Bash. Times inside the guest come
from `/proc/uptime`.

More processors cost little at start, over five runs:

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true` with `--cpus 1` | 263-268 ms, and 327 ms once |
| the same with `--cpus 2` | 263-281 ms |
| the same with `--cpus 4`, the default here | 271-284 ms |
| the same with `--cpus 8` | 290-295 ms |
| the same with `--cpus 16` | 312-330 ms |

Work that runs in parallel scales with them, over three runs each. "16
hashes" is sixteen jobs that each hash 512 MB from `/dev/zero` through a
pipe. The build is a cold `GOOS=windows go build ./cmd/runwhp` in
`golang:1.26` with `-m 3g`, which reads runwhp's source and the host's Go
module cache through read-only bind mounts and keeps its build cache on the
overlay:

| `--cpus` | 16 hashes | `go build` |
| --- | --- | --- |
| 1 | 65.6-65.8 s | 62.1-65.1 s |
| 2 | 33.9-36.2 s | |
| 4 | 18.6-19.1 s | 25.8-31.0 s |
| 8 | 11.2-11.4 s | |
| 16 | 10.6-10.7 s | 17.6-18.7 s |

The same build takes 17.5-18.9 s on Windows itself, and 47.0-47.8 s in
WSL2's Docker with 16 vCPUs, reading the same files through `/mnt`.
Hashing gains little past 8 vCPUs: by then the machine's 8 cores are all
busy, and the second thread of a core adds little.

`git status` in a new container, on a tree of 10,000 files on NTFS like
iteration 5's, checks the files from several threads. It took 1.11-1.15 s
with 1 vCPU, 0.80-0.84 s with 4, and 0.95-0.96 s with 16.

Before runwhp described the topology in CPUID, the 15 secondary CPUs of a
16-vCPU guest took 1.24 s to come up; they now take 12 ms. See
[Findings](#findings).

The new tests cover:

- the MADT for 1 to 64 processors, and the CPUID topology decoded as Linux
  decodes it
- the rules for stopping the guest, and the I/O APIC's routing to each
  CPU's local APIC, from concurrent goroutines and under the race detector
- parsing `--cpus`
- end to end:
  - the number of processors and the topology the guest sees, with and
    without `--cpus`, and refused values
  - a process on each CPU, and four busy loops spread over four CPUs
  - an exit code from the last CPU, and a reboot asked for from another
  - Ctrl+Break before the secondary CPUs have started
  - a CPU taken offline and brought back
  - four writers to a bind mount, four downloads from the host and four
    writers to stdout, all at once

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel.

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
  container guest does not need. Iteration 4 replaced it.
- **Linux 6.18 needs PCI support for ACPI.** Built without `CONFIG_PCI`,
  ACPICA fails to load the tables ("AE_BAD_PARAMETER, During Region
  initialization"), no device appears, and init never reports back. With PCI
  built in, `pci=off` keeps the kernel from probing a bus that is not there.
- **6.18 still reads the CMOS clock** under hardware-reduced ACPI, without an
  RTC driver, so the VMM's stub still sets the wall clock.
- **On AMD Zen, 6.18 reads the FCH at `0xfed80000`** to report why the
  machine last reset. The VMM ignores the unbacked page and the kernel
  carries on.
- **A chrooted process cannot create user namespaces.** The kernel refuses
  `unshare(CLONE_NEWUSER)` when the caller's root is not its mount
  namespace's root, and `pivot_root` fails the same way, so container tools
  inside the container broke. runwhp-init now makes the overlay the real
  root, as `switch_root` does, instead of starting the process in a chroot.
- **LZ4 boots fastest**, even though it makes the largest kernel; see
  [Iteration 4 results](#iteration-4-results).
- **A missing virtio shared memory region must read as length -1.** The WSL
  kernel, built with FUSE DAX, asks a virtio-fs device for its DAX window.
  The transport answered 0, so the kernel tried to reserve an empty region at
  address 0, failed with EBUSY, and dropped the device. runwhp's kernel has
  no DAX and never asked.
- **NT checks the alignment of what it fills in.** A Go byte array has none,
  and `NtQueryInformationFile` then fails with
  `STATUS_DATATYPE_MISALIGNMENT`, which at first looked like a file system
  without WSL's metadata. Every buffer runwhp hands to NT is now 8-byte
  aligned.
- **ReFS file ids are 128 bits.** The 64-bit id a stat reports is a packed
  form, which matches what `FILE_ID_BOTH_DIR_INFO` listings report, but not
  the 128-bit ids of `FILE_ID_EXTD_DIR_INFO` listings, whose low halves are
  small and repeat across directories.
- **An unelevated process can do what WSL does.** It can set WSL's reparse
  points and extended attributes, and delete and rename with POSIX
  semantics, on NTFS and on ReFS alike.
- **`NtQueryInformationByName` stats without opening.** On NTFS it is five
  times faster than an open, a query and a close. It does not follow a link,
  and with `FileStatLxInformation` it returns WSL's metadata too.
- **WHP starts secondary processors itself.** With the local APIC emulated
  by the hypervisor, every vCPU but the first starts out suspended: it
  waits inside `WHvRunVirtualProcessor` until the guest sends it INIT and
  SIPI, then starts in the guest's real-mode trampoline, all without an
  exit to the VMM. runwhp sets `StartupSuspend` in
  `WHvRegisterInternalActivityState` all the same, rather than rely on
  that. Taking a CPU offline and back works the same way. Each vCPU's APIC
  id is its index.
- **WHP leaves the processor topology to the VMM.** It answers CPUID's
  topology leaves (0xB, the core count in 0x80000008, and the sharing of
  the L3 cache) as if every vCPU were a package of its own. Linux then gave
  each CPU a last-level cache to itself, and each secondary CPU calibrated
  its delay loop from scratch, because it had no sibling to copy it from:
  `calibrate_delay_direct()` failed under the hypervisor, and 15 CPUs took
  1.24 s to come up. `WHvPartitionPropertyCodeCpuidResultList2` now
  describes one package of single-threaded cores sharing the L3; it takes
  per-VP entries only once the VPs exist, and fails `WHvSetupPartition`
  with `HV_STATUS_INVALID_VP_INDEX` before. 16 CPUs come up in 12 ms.
- **Hyper-V's paravirtual TLB flushes and IPIs bring nothing measurable
  here.** Asked for `TbFlushHypercalls`, `SyntheticClusterIpi` and
  `ExtendedProcessorMasks`, WHP recommends them to the guest, and Linux
  switches to hypercalls for remote TLB flushes and IPIs. But the build and
  the hashes above ran no faster, compared run for run, so runwhp leaves
  them off.
- **Halted processors never exit.** With the hypervisor's local APIC,
  `hlt` does not exit to the VMM even with interrupts disabled, so the VMM
  cannot tell a guest that halted for good, as after `halt -f`, from an
  idle one.

## Known gaps

- There is no detaching or reattaching, and no way to send signals other
  than SIGINT and SIGTERM.
- Nothing written outside a bind mount survives the run: the scratch disk
  is deleted when it ends.
- There are no named volumes and no `--tmpfs`.
- Every `write()` to a bind mount is a request to the host, about 130 µs each
  on the test machine, so a program writing in small pieces is slow there;
  see [Iteration 5 results](#iteration-5-results). Making files is slow too,
  at 3-4 ms each, mostly in Windows itself.
- Host changes in bind mounts raise no inotify events in the container.
  Extended attributes cannot be set there, and file locks are not shared with
  host programs.
- The kernel's first fetch needs ghcr.io, unless `--kernel` names one. A new
  kernel build reaches runwhp only when someone moves the pin in
  `internal/kernel/pin.go`, and the pinned digest is the only check on it:
  builds are not signed.
- An image can have at most 56 non-empty layers, one more with
  `--network none`, and one fewer with bind mounts: each device, the
  scratch disk included, takes an I/O APIC line.
- The network is IPv4 only and carries no ICMP. Only TCP ports can be
  published, each with an explicit host port, and there is no `-P`, `--dns`
  or `--dns-search`.
- The network device has neither notification suppression (EVENT_IDX) nor
  checksum and segmentation offloads, so sending from the container is
  limited to about 90 MB/s on the test machine.
- The cache only grows: there is no `images` or `rmi` yet.
- Interrupts are edge-triggered only; level-triggered lines and EOI tracking
  are not implemented.
- A guest that stops without resetting, as `halt -f` and `poweroff -f` make
  it, runs on until Ctrl+C. The hypervisor keeps its halted processors to
  itself, and runwhp has no ACPI power-off for it to use.
- `--cpus` is a count, not Docker's quota, and there is no `--cpuset-cpus`.
- Each device has one virtqueue, which every vCPU shares.
- Each MMIO access takes several round trips through `WinHvEmulation.dll`;
  a fast path for common instructions would cut the per-write output cost.
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
  MSI-X would also give each device a queue and an interrupt per vCPU.
- What protection does the guest kernel owe a container from itself? The VM
  is the boundary with the host, and the container is the guest's only
  tenant, so KASLR is off; CPU mitigations are on, at a cost to system
  calls that has not been measured.
- Should bind mounts use FUSE's writeback cache? The guest would gather
  small writes into large ones before sending them to the host, but the
  host would see writes late, and a host program changing a file the
  container has open could have its change overwritten.
