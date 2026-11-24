# Status

- **Iteration 13** (2026-11-24): containers restart, and their logs are
  bounded and tell the time. `--restart` takes Docker's policies: the
  runwhp running a container boots it again when its process ends, after
  Docker's delays, unless it was stopped or killed, and a foreground run
  hands its container over to a runwhp in the background to do so. Logs
  record when each line was written, rotate at 20 MB keeping five files
  unless `--log-opt` says otherwise, and every kept container has one,
  foreground runs included; `logs` takes `-t`, `-n`, `--since` and
  `--until`. The container's process no longer starts before init has
  set the guest's clock, and a kept container whose runwhp died starting
  it again is no longer swept away. See [Containers](../README.md#containers)
  and [Iteration 13 results](#iteration-13-results).
- **Iteration 12** (2026-11-24): the store no longer only grows.
  `images` lists what is cached, with Docker's image IDs. `rmi` removes
  images, and `image prune`, `container prune` and `system prune` remove
  what nothing uses: layers no image or container lists, images no
  container was made from, stopped and created containers, old kernels
  and, with `--volumes`, volumes. `system df` shows what each takes and
  what a prune would free. Pulls and runs hold a lock on the store while
  they resolve images, so no prune removes layers they are about to use,
  and runs cost no more. The test machine's store had 141 images left by
  the end-to-end tests, which no longer leave any. See
  [Cleaning up](../README.md#cleaning-up) and
  [Iteration 12 results](#iteration-12-results).
- **Iteration 11** (2026-11-23): containers that run for a long time
  can be rejoined and keep good time. `attach` shows a running
  container's output and, for one in the background run with `-i`, takes
  input, with Docker's Ctrl+P Ctrl+Q to detach; `start -a` attaches from
  the first byte, and `wait` prints how containers ended. The guest's
  clock is set from the host's as init starts, every minute, and when
  Windows resumes: it was 0-1 s behind from boot, and is now within
  1 ms. The container can no longer reset, halt or power off its VM, as
  `reboot -f` and `halt -f` did, and the ACPI tables offer a power-off.
  See [Containers](../README.md#containers) and
  [Iteration 11 results](#iteration-11-results).
- **Iteration 10** (2026-11-23): I/O got faster. runwhp decodes the
  guest's accesses to device registers itself instead of through
  `WinHvEmulation.dll`, serves a bind mount's request on the vCPU that sent
  it, and suppresses needless notifications and interrupts with
  `EVENT_IDX`; the network device offers checksum offload. A small write
  to a bind mount takes about 90-110 µs instead of 120-200, and with
  `-v SRC:DST:delegated` the guest caches a bind mount's writes, which made
  writing 200 MB in small pieces take 1.2 s instead of 22-28. A bug that
  dropped frames for the guest, which made downloads slow and sometimes
  stall, is fixed. See [Delegated mounts](../README.md#delegated-mounts)
  and [Iteration 10 results](#iteration-10-results).
- **Iteration 9** (2026-11-23): containers get as much memory as they
  need and give back what they free. Guest RAM goes on above 4 GiB, up to
  the machine's, and defaults to half of it. It is committed 2 MiB at a
  time as the guest first touches it, so a guest takes from Windows only
  what it uses, and a virtio balloon with free page reporting gives back
  what the guest frees; an idle guest gives back its page cache too.
  `runwhp stats` shows what running containers use. Runs got faster:
  `run alpine:3.20 true` takes about 165 ms at 512m, and about 225 ms with
  half the machine's memory, against about 265 ms in iteration 8. See
  [Memory](../README.md#memory) and [Iteration 9 results](#iteration-9-results).
- **Iteration 8** (2026-11-22): containers outlive the command that made
  them. Every run keeps its container, with its writable layer, unless
  `--rm`; `run -d` and `create` run one in the background, in a runwhp of
  its own that survives closing the terminal; and runc's verbs, `start`,
  `kill`, `state`, `list`, `delete` and `exec`, with `stop`, `restart`,
  `rename` and `logs`, work on them by ID, prefix or name. A stopped
  container starts again in a new VM on its journaled writable layer.
  `exec` runs further commands over a new virtio socket device. See
  [Containers](../README.md#containers) and
  [Iteration 8 results](#iteration-8-results).
- **Iteration 7** (2026-11-21): containers write to disk. The writable
  layer lives on a sparse ext4 scratch disk made for each run instead of in
  guest RAM, so a 256 MiB guest writes gigabytes. Named volumes
  (`-v NAME:PATH`, `runwhp volume`) keep data between runs in journaled
  ext4 disk images, filled from the image as Docker fills them, and
  anonymous volumes and `--tmpfs` work too. A Go build cache in a volume
  takes a build of runwhp from 21 s to 3 s, and a run costs about 15 ms
  more, plus 10-20 ms per volume. See [Storage](../README.md#storage),
  [Volumes](../README.md#volumes) and [Iteration 7 results](#iteration-7-results).
- **Iteration 6** (2026-11-21): containers get more than one vCPU: the
  host's logical processors, up to 4, unless `--cpus` asks for more, up to
  64. The guest sees them as cores of one processor sharing its cache, so
  its scheduler spreads work over all of them. A cold `go build` of runwhp
  over bind mounts takes 18 s with 16 vCPUs, as long as on Windows itself,
  against 63 s with one, and starting 16 costs about 50 ms. See
  [Usage](../README.md#usage) and [Iteration 6 results](#iteration-6-results).
- **Iteration 5** (2026-11-21): `-v` and `--mount` bind host directories and
  files into the container. runwhp serves them itself over virtio-fs, and
  keeps the owners, modes, symlinks and special files the container makes in
  the format WSL uses, so WSL sees them the same way. See
  [Host files](../README.md#host-files) and [Iteration 5 results](#iteration-5-results).
- **Iteration 4** (2026-11-20): runwhp boots its own kernel, Linux 6.18 with
  everything it needs built in, fetched once from ghcr.io and pinned by
  digest. WSL is no longer needed, nor is copying a driver out of it, and
  `run alpine:3.20 true` takes under 300 ms instead of over 700. See
  [Kernel](../README.md#kernel) and [Iteration 4 results](#iteration-4-results).
- **Iteration 3** (2026-11-20): containers have a network. Outbound TCP and
  UDP leave through the host's own sockets, names resolve through the host's
  resolver, `-p` publishes container ports on the host, and
  `--add-host NAME:host-gateway` reaches services on the host's loopback. It
  needs no elevation and no new kernel module. See [Networking](../README.md#networking)
  and [Iteration 3 results](#iteration-3-results).
- **Iteration 2** (2026-11-20): `-i` passes stdin to the container with a real
  end of file, `-t` gives it a pseudo-terminal that follows the console's size,
  and Ctrl+C reaches the container as SIGINT instead of killing the VM.
  Container output no longer runs on the vCPU thread, so a slow reader stalls
  only its own stream. See [Usage](../README.md#usage) and
  [Iteration 2 results](#iteration-2-results).
- **Iteration 1** (2026-11-20): `runwhp run IMAGE [COMMAND...]` pulls an image,
  converts its layers to ext4, boots one VM, runs the entrypoint and command
  with the image's environment, working directory and user, sends stdout and
  stderr to the host separately, and exits with the container's exit code.
  See [Usage](../README.md#usage) and [Iteration 1 results](#iteration-1-results).
- **Proven by a spike** (2026-11-17): an unelevated Go program boots a Linux
  kernel on WHP. See [Boot spike](../README.md#boot-spike).
- **Proposed, not yet agreed**: the rest of the architecture in
  [Proposed design](../README.md#proposed-design).
- **Not started**: OCI bundles, for containerd or nerdctl to drive runwhp
  with; see [Open questions](../README.md#open-questions).

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
which the MMIO fast path in [Known gaps](gaps.md) would cut, and the
Windows call. Writing 20 MB from `dd` took 0.12 s in 64 KiB blocks and
0.8 s in 4 KiB blocks. FUSE's writeback cache would gather small writes
together, but at a cost to host edits; see [Open questions](../README.md#open-questions).

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
[Findings](findings.md).

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

## Iteration 7 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 6" is that iteration's build, run in turn
with this one.

Disks cost a little at start, over four or five runs:

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true`, iteration 6 | 244-255 ms, and 307 ms once |
| the same, iteration 7 | 262-271 ms |
| with one volume | 285-295 ms |
| with five volumes | 316-342 ms |

Of the scratch disk's cost, deleting it takes about 4 ms; it is formatted in
3 ms while the kernel loads. A volume costs mounting a journaled file system
and freezing it at the end.

Moving 200 MB, timed inside the guest with `-m 1g` as in iteration 5, over
three runs. Reads follow a `drop_caches`, which empties the guest's cache
but not a tmpfs:

| Measurement | Writable layer | Volume | Iteration 6's tmpfs |
| --- | --- | --- | --- |
| `dd` in 1 MiB blocks, then `fsync` | 0.53-0.54 s | 0.69-0.71 s | 0.47-0.48 s |
| `dd` reading it back | 0.08-0.09 s | 0.08-0.09 s | 0.02 s |
| `head -c`, about 820 bytes a write, then `sync` | 1.29-1.32 s | 1.39-1.42 s | 1.22-1.27 s |

A bind mount took 0.96-1.09 s, 0.18-0.21 s and 34.7 s for the same in
iteration 5.

The tree of 10,000 small files in 100 directories from iteration 5, in
`golang:1.26` with `--cpus 4`, two runs each:

| Measurement | Writable layer | Volume | Iteration 6's tmpfs |
| --- | --- | --- | --- |
| `tar -x` | 0.33 s | 0.24-0.25 s | 0.24-0.25 s |
| `find` | 0.01 s | 0.01 s | 0.01 s |
| `ls -lR` | 0.03 s | 0.02 s | 0.03 s |
| `git add -A` and `git commit` | 0.87-0.96 s | 0.69-0.73 s | 0.71-0.74 s |
| `git status`, twice | 0.01-0.02 s | 0.01-0.02 s | 0.01-0.02 s |

The writable layer is overlayfs over ext4, which makes files more slowly
than overlayfs over tmpfs; a volume is plain ext4. A bind mount took 36-41 s
to extract the same tree in iteration 5.

Reading the Go toolchain, about 16,000 files and 270 MB, out of
`golang:1.26`'s layers with caches dropped and `--cpus 4`, six runs each:

| Readers | Iteration 6 | Iteration 7 |
| --- | --- | --- |
| one | 3.45-4.12 s | 3.82-4.11 s, and 4.75 s once |
| four | 1.40-1.53 s | 1.33-1.39 s |

Handing every request to a worker thread, as this iteration's first version
did, took 5.0-5.3 s for one reader; see [Findings](findings.md).

A cold `GOOS=windows go build ./cmd/runwhp` in `golang:1.26` with `-m 3g`,
as in iteration 6, reading the source and module cache through read-only
bind mounts, two runs each:

| `--cpus` | Iteration 6 | Iteration 7 |
| --- | --- | --- |
| 4 | 27.1-27.4 s | 27.9-28.8 s |
| 16 | 19.8 s | 20.8-21.4 s |

The build cache is now on the scratch disk instead of in RAM, which costs
3-7%. In a volume it outlives the run: the next build took 3.1-3.2 s.

Also measured:

- Installing `python3` with `apt-get` in `debian:12`, its packages already
  downloaded, took 4.90-5.14 s on the writable layer, 5.15-5.25 s without
  `volatile` and 4.78-4.80 s on iteration 6's tmpfs.
- Truncating a 1 GiB file in the writable layer gave the host its space
  back within 3 s. Without `discard`, the host kept it until the run ended.
- A run that leaves 1 GiB of the writable layer unwritten when it ends no
  longer writes it out first. It was worth only 50-90 ms here, since the
  guest had written most of it back by then.
- `postgres:17` initialized its database on a new volume, which it refuses
  to do in a directory holding `lost+found`, and found it again on the next
  run.

The new tests cover:

- the disk files, on NTFS and, through `RUNWHP_TEST_DIR`, on ReFS: sparse
  creation, concurrent reads and writes, freeing ranges, exclusive and
  shared opens, and scratch disks deleted on close and when their process
  is killed
- the block device against a simulated driver: requests served by the
  kicking thread and by workers, out of order, past and across the end of
  the disk, read errors, a header changed after it was checked, reset with
  requests in flight, draining, discard and write-zeroes, and the features
  and configuration of each kind of disk
- the formatter, from 64 MiB to 1 TiB with and without a journal: every
  structure read back as the kernel would, the checksums, the bitmaps of
  uninitialized groups, and `e2fsck -fn` where it is installed
- copying a directory into a volume, as root in a Linux container: owners,
  modes, times, set-user-ID bits, symlinks, hard links, FIFOs, device nodes
  and extended attributes, and sockets skipped
- volume names, making a volume from several processes at once, and
  removing one in use
- parsing `-v`, `--mount`, `--tmpfs` and `--storage-opt`
- end to end:
  - 1 GiB written and read back in a 256 MiB guest, the writable layer's
    size, ENOSPC past it, and fsync not reaching the host
  - scratch disks gone after a run, a second Ctrl+Break and a killed
    runwhp, and one left behind swept away
  - data kept across runs, a volume filled from an image with every kind
    of file, `nocopy`, a volume under a bind mount filled from the image,
    `:ro`, one volume at two paths
  - a volume in use refused to a second container and to `volume rm`
  - a volume clean after a run, and after a second Ctrl+C in the middle of
    writes, replayed and clean after the next
  - anonymous volumes, under a bind mount and from the image
  - tmpfs mounts: `noexec`, `size`, `exec` and `tmpfs-mode`
  - all four kinds of mount nested
  - too many volumes for the I/O APIC
  - `volume create`, `ls`, `inspect`, `rm` and `prune`

## Iteration 8 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 7" is that iteration's build, run in turn
with this one.

Keeping a record costs a little at start, over six runs:

| Measurement | Result |
| --- | --- |
| `run alpine:3.20 true`, iteration 7 | 262-279 ms |
| the same with `--rm` | 275-290 ms |
| the same, kept | 301-315 ms |

Making a record, taking its lock, serving its pipe and writing its state
take 5-10 ms on NTFS, and the vsock device about 5 ms to boot. A kept
container also freezes its writable layer before the VM resets, as a volume
is frozen, which is most of the rest.

The lifecycle commands, over five runs, or three for the slower ones:

| Measurement | Result |
| --- | --- |
| `create` | 270-277 ms |
| `start` of a created container | 43-49 ms |
| `run -d`, until it returns | 271-285 ms |
| `stop` of a container that heeds SIGTERM | 131-138 ms |
| `stop -t 1` of one that ignores it | 1.085-1.090 s |
| `start` of a stopped container | 261-275 ms |
| `restart` | 353-380 ms |
| `delete -f` of a running container | 162-261 ms |
| `start` after its runwhp was killed, replaying the journal | 348-375 ms, against 258-269 ms after a clean stop |
| `list` with 100 containers | 58-68 ms |

`exec`, over eight runs for the first and three for the others:

| Measurement | Result |
| --- | --- |
| `exec ID true` | 43-47 ms |
| `exec` writing 200 MB to stdout, sent to the null device | 0.91-1.53 s |
| `exec -i` reading 200 MB from a file | 1.71-1.76 s |
| a foreground `run --rm` writing 200 MB to stdout, to a file, boot included | 1.15-2.23 s |
| a container in the background writing 200 MB to its log, until it has stopped | 1.29-1.82 s |

Output from `exec` goes about as fast as a foreground run's, through the
vsock device instead of a console port.

The new tests cover:

- the container records, on NTFS and, through `RUNWHP_TEST_DIR`, on ReFS:
  - making one, finding one by ID, name or prefix, and prefixes that match
    more than one
  - names, case-insensitive, taken from goroutines at once, renamed, and
    left behind by a container that is gone
  - the state replaced while it is being read
  - the lock held by another process, and a container whose runwhp died
  - `list` removing what dead runwhps left, and the volumes containers use
- the log's framing, frames cut short, and a damaged log
- signals by name and number
- the vsock device against a simulated driver, under the race detector:
  connecting, refused and unanswered; data both ways; flow control both
  ways, asking for credit, and a guest sending past its credit; small
  receive buffers; stray packets; closing; connections taking turns; reset
  and draining
- init's reaper, in WSL Docker under the race detector: processes that
  exit at once, killed ones, and sixteen at once
- the writable layer: a scratch one, a kept one, and a kept one opened again
- end to end:
  - kept containers: `list`, `state`, `delete`, names, renames, prefixes,
    a writable layer clean after a run, and fsync reaching the host only
    for a kept one
  - `/etc/hostname`, `/etc/hosts` and `/etc/resolv.conf` bound: edited in
    place but not replaced, over a volume on `/etc`, and a mount of the
    user's own winning
  - `volume rm` refusing a volume a stopped container uses
  - `--rm` leaving nothing after a run, a hard stop or a killed runwhp
  - `run -d` with output to the log, to a file after the ID, with `-t`,
    and with piped stdin; `logs -f`
  - `create`, `start`, `kill` of a created container, signals by name and
    number, `kill --all`, `stop` with SIGTERM heeded and ignored, `restart`
    on the same layer and hostname, `delete -f`, a killed runwhp recorded
    with 255 and its journal replayed, three `start`s at once, `kill` of a
    foreground run, a container in the background outliving the pseudo
    console its `run -d` ran in, and published ports bound again on restart
  - `exec`: streams, exit codes, `-e`, `-w` and `-u`, missing directories
    and users, refused in a created or stopped container, 10 MB of stdin,
    `-d`, four at once with 20 MB each, killed with the container, into a
    foreground run, Ctrl+Break, and `-it` on a pseudo console with a resize

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel with its vsock modules passed with `--module`.

## Iteration 9 results

Measured on the test machine, which has 15.3 GiB of RAM, with runwhp's
kernel, from Git Bash, with the store on NTFS (`C:`). "Iteration 8" is that
iteration's build, run in turn with this one. Half the machine's memory,
the new default, is 7852 MiB here.

Runs got faster, over seven runs:

| Measurement | Result |
| --- | --- |
| `run --rm alpine:3.20 true`, iteration 8 (512 MiB) | 255-272 ms |
| the same, iteration 9, `-m 512m` | 159-168 ms |
| the same, iteration 9, default memory (7852 MiB) | 216-233 ms |

What a guest takes from Windows to run `true`, against all of its memory
in iteration 8:

| Memory | Committed at most | Memory faults |
| --- | --- | --- |
| 512 MiB | 62 MiB | 25 |
| 2 GiB | 92 MiB | 39 |
| 4 GiB | 138 MiB | 62 |
| 7852 MiB | 206 MiB | 95 |
| 15 GiB | 336 MiB | 160 |

Most of it is the guest kernel's page descriptors, 1/64 of its RAM; each
GiB also adds about 8 ms to a run. Before the guest's touches mapped its
memory, committing and mapping 8 GiB up front took 140 ms, and the guest
kernel then spent 250 ms describing its pages, 7.6 µs per 4 KiB page
for the hypervisor to fill in its page tables; populating each chunk as
it is mapped took that to 22 ms. See [Findings](findings.md).

Giving memory back, watched from the host as the commit of the
container's runwhp process:

| Measurement | Result |
| --- | --- |
| a 4 GiB guest fills 3 GiB of tmpfs and deletes it | commit rose from 198 to 3289 MiB, and was back to 227 MiB 14 s after the delete |
| the same, `TestMemoryGivenBack`'s 1.5 GiB in a 3 GiB guest | 176 MiB, 1724 MiB full, 379 MiB 8.3 s after |
| 1.5 GiB of page cache in a 4 GiB guest, idle with `RUNWHP_RECLAIM_IDLE=5` | 1814 MiB, then 920 MiB 40 s later, a tenth of the cache every 5 s |
| a 4-vCPU `go build` of runwhp in a 4 GiB guest | 230 MiB before, 1190 MiB at its peak, 870 MiB once its page cache was dropped |
| a detached `postgres:17`, default memory, idle | 467 MiB a minute after it started, 462 MiB after seven idle minutes |

A cold `GOOS=windows go build ./cmd/runwhp` in `golang:1.26` with
`--cpus 16`, reading the source and module cache through read-only bind
mounts, as in iteration 6, two runs each:

| Build | Time | Committed at most |
| --- | --- | --- |
| iteration 8, `-m 3g` | 22.9 s, and 46.5 s the first time | 3 GiB |
| iteration 9, default memory, no `-m` | 18.5-22.5 s | 1.26 GiB |

Reading the Go toolchain out of `golang:1.26`'s layers, `tar` of
`/usr/local/go` with caches dropped, `--cpus 4 -m 1g`, three runs each:

| Build | Time |
| --- | --- |
| iteration 8 | 4.15-4.18 s, and 7.10 s the first time |
| iteration 9 | 2.96-3.15 s |

The guest fills its page cache from chunks populated whole rather than a
page at a time.

The balloon and init's cgroup mount each first cost about 15 ms a run,
waiting for an RCU grace period: the balloon's driver as the kernel shuts
it down, the mount as init makes it. Init now expedites grace periods for
both, as it did for freezing volumes, and they cost nothing measurable.

The idle postgres gave back little: its guest used 145 MiB, of which
138 MiB was page cache, below the reclaimer's floor of 2% of its RAM,
157 MiB here. The rest of what its runwhp held was the guest kernel's page
descriptors, about 123 MiB, runwhp itself, and memory `initdb` had freed in
blocks too small to report.

`runwhp stats --no-stream` for two containers takes about 0.55 s, most of
it the half second between its two samples.

The new tests cover:

- guest RAM in two ranges: spans in either and across the hole, one map
  per chunk from 16 goroutines at once, faults inside and outside RAM and
  across chunks, commit failing, and chunks given back whole and in part
  while others are faulted in, under the race detector
- the loader: e820 with and without RAM above 4 GiB, and an 8 GiB guest's
  initramfs below 2 GiB
- the balloon against a simulated driver: reports given back by address
  without slicing what they name, chains running past RAM, the idle
  inflate and deflate queues, a driver without reporting, and reset
- the reclaimer's decisions, `/proc/meminfo` and `/proc/stat` parsing,
  and counting tasks without the kernel's threads, on Linux
- `-m` and its default, `RUNWHP_RECLAIM_IDLE`, the stats table, CPU %
  from two samples, sizes, and a late answer from init not taken for the
  next request
- end to end:
  - `-m 6g`: `MemTotal`, System RAM above 4 GiB, and 512 MiB of random
    data through a tmpfs in the Normal zone, the RAM above 4 GiB
  - the default memory, recorded in `state`, and `-m` above the machine's
  - a guest committing only what it touches
  - 1.5 GiB filled and freed, and back to Windows within a minute
  - an idle guest's page cache given back, and kept with
    `RUNWHP_RECLAIM_IDLE=0`
  - `stats`: two busy loops on 2 vCPUs at 150-210%, an idle container's
    PIDs and memory, all running containers, the table, network bytes, a
    stopped container refused, and a foreground run

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel with its vsock modules passed with `--module`. WSL's kernel
builds in the balloon and free page reporting, so it gives memory back
too; it takes 64 MiB or so more of its own than runwhp's.

## Iteration 10 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 9" is that iteration's build, run in turn
with this one.

Runs got a little faster, as the boot's 2,800 device accesses no longer go
through the emulator, over seven runs:

| Measurement | Iteration 9 | Iteration 10 |
| --- | --- | --- |
| `run --rm alpine:3.20 true`, `-m 512m` | 191-215 ms | 177-191 ms |
| the same, default memory | 266-291 ms | 245-275 ms |

Where a small request's time goes, for 20 MB written to a bind mount on
NTFS in 820-byte pieces, 25,600 requests, before this iteration's changes
and after them:

| Per request | Before | After |
| --- | --- | --- |
| Wall time | about 190 µs | about 85 µs |
| MMIO exits | 3.1, through the emulator, 17.8 µs each | 3.1, decoded by runwhp, 2.3 µs each besides the request |
| Windows' `WriteFile` | about 18 µs | 10-12 µs |
| Served by | a worker, while the vCPU halted | the vCPU that sent it |

Bind mounts, timed inside the guest with `-m 1g`, over three runs. "Small
writes" is 200 MB from `head -c`, about 820 bytes a write, then `sync`; the
others end with an `fsync`:

| Measurement | Iteration 9 | Iteration 10 | Iteration 10, delegated |
| --- | --- | --- | --- |
| NTFS, 200 MB in 1 MiB writes | 0.53 s | 0.44-0.50 s | 0.95-1.06 s |
| NTFS, 20 MB in 4 KiB writes | 0.90-1.00 s | 0.34-0.57 s | 0.10-0.12 s |
| NTFS, small writes | 30.2-51.9 s | 22.0-28.2 s | 1.17-1.18 s |
| ReFS, 200 MB in 1 MiB writes | 0.52-0.64 s | 0.45-0.50 s | 1.03-1.09 s |
| ReFS, 20 MB in 4 KiB writes | 0.53-1.06 s | 0.22-0.49 s | 0.10-0.11 s |
| ReFS, small writes | 50.0-51.1 s | 25.3-27.9 s | 1.18-1.19 s |

A delegated mount takes large writes followed by an `fsync` at about half
the speed: the guest copies them into its page cache first, and writes
them back in 512 KiB pieces when the `fsync` comes.

The tree of 10,000 small files in 100 directories from iteration 5, on
NTFS, in `golang:1.26` with `--cpus 4`, one run each:

| Measurement | Iteration 9 | Iteration 10 | Iteration 10, delegated |
| --- | --- | --- | --- |
| `tar -x` | 37.7 s | 35.9 s | 35.2 s |
| `git add -A` and `git commit` | 76.1 s | 63.3 s | 72.7 s |
| `git status`, new container | 0.75 s | 0.41 s | 0.44 s |
| `git status` again | 0.26 s | 0.19 s | 0.19 s |
| `rm -rf` | 18.9 s | 15.6 s | 19.5 s |

Making files is still slow: Windows takes most of the 3.5 ms each costs,
and the writeback cache does not gather creates, renames or removals.

Moving 200 MB, timed inside the guest, over four runs. Iteration 9 dropped
some frames for the guest, as [Findings](findings.md) explains, which made
downloads slow, and slower still each time the host's stack backed off:

| Measurement | Iteration 9 | Iteration 10 |
| --- | --- | --- |
| to a host server through `host-gateway` (`dd \| nc`) | 0.43-0.74 s | 0.36-0.53 s |
| from it (`wget`) | 0.50-6.39 s | 0.58-0.84 s |

The same upload took 30-45% fewer interrupts with `EVENT_IDX`, and a
download about 60% fewer.

Output, over four runs:

| Measurement | Iteration 9 | Iteration 10 |
| --- | --- | --- |
| 200 MB to stdout, to a file, boot included | 1.08-2.53 s | 1.01-1.84 s |
| `exec` writing 200 MB to stdout, to a file | 1.06-1.09 s | 0.97-0.98 s |

Console output takes about 1.0 s or about 1.8 s, run to run, in both.

Reading the Go toolchain out of `golang:1.26`'s layers, `tar` of
`/usr/local/go` with caches dropped, `--cpus 4 -m 1g`, three runs each in
turn; each pair ran faster than the one before, as the host's cache warmed:

| Build | Time |
| --- | --- |
| iteration 9 | 2.85 s, 2.78 s, 1.73 s |
| iteration 10 | 2.58 s, 2.21 s, 1.29 s |

A cold `GOOS=windows go build ./cmd/runwhp` in `golang:1.26` with
`--cpus 16`, reading the source and module cache through read-only bind
mounts, as in iteration 6, two runs each: 19.9 s with iteration 9, and
17.2-17.9 s with iteration 10.

The new tests cover:

- the instruction decoder: every mov form Linux uses for device
  registers, with each kind of memory operand, the forms it leaves to the
  emulator, and opcodes past prefixes
- `EVENT_IDX` against a simulated driver: interrupts held back until
  `used_event`, notifications asked for only once the device has run out,
  a chain added while the device asks, 70,000 chains across the 16-bit
  wrap, the no-interrupt flag without it, and reset; and every device's
  tests again with a driver that takes it, under the race detector
- the network device: a frame over several receive buffers returned at
  once, the checksum features, frames marked valid, and the driver's
  choice across a reset
- the stack: a guest leaving its checksums out, and frames for it with
  checksums or without, as the guest takes them
- virtio-fs: the first request served by the kicking goroutine and the
  rest by workers, a request the handler holds slow sent to a worker, and
  reset and draining with a request served either way
- hostfs, on NTFS and, through `RUNWHP_TEST_DIR`, on ReFS: files marked
  offline are slow, and write-only files readable with the writeback cache
- FUSE: the writeback cache taken only when asked for and offered, and
  requests about slow nodes kept off the vCPU
- parsing `:delegated` and `consistency=`, shares numbered per file
  system, and the device budget with both
- end to end: nearly every device access served without the emulator;
  delegated mounts: 20 MB in small writes and its checksum, appends,
  renames and removals reaching the host after the run, a file closed or
  synced reaching it while the container runs, and a consistent, a
  delegated and a read-only mount in one container

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel with its vsock modules passed with `--module`.

## Iteration 11 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 10" is that iteration's build, run in turn
with this one.

Runs cost the same, over seven runs:

| Measurement | Iteration 10 | Iteration 11 |
| --- | --- | --- |
| `run --rm alpine:3.20 true`, `-m 512m` | 173-184 ms | 175-189 ms |
| the same, default memory | 239-258 ms | 243-251 ms |

The guest's wall clock against the host's, read with busybox's
`adjtimex` from an `exec`, whose 80-110 ms bounds each reading:

| Measurement | Iteration 10 | Iteration 11 |
| --- | --- | --- |
| at boot, over eight runs | 0.10 s ahead to 0.94 s behind | within 50 ms, the test's bound |
| a detached container, every 5 minutes for 45 minutes | 0.10-0.21 s behind, unchanged | -0.05 to +0.06 s, unchanged |
| the same, after Windows stepped its own clock back 1.286 s | 1.07-1.17 s ahead, for the next 85 minutes | -0.05 to +0.06 s |
| the host's time as init receives it, 120 messages, idle and busy | | 0.01-0.47 ms ahead of the guest's |

The guest's clock does not drift from the host's, but Windows corrects its
own: it stepped it back 1.286 s against time.windows.com while both ran.
Otherwise, the offset a guest booted with in iteration 10 stayed with it
for its life. Modern Standby, the only sleep this machine has, never
paused either guest; see [Findings](findings.md).

Attaching, over three runs. `start -a` boots the container, attaches,
then starts it; its 200 MB go to the null device, and to the log, which
`logs` then prints:

| Measurement | Result |
| --- | --- |
| `start -a` of a container writing 200 MB to stdout, boot included | 0.91-1.11 s |
| `logs` of the same 200 MB | 0.20-0.21 s |
| `start -a -i` of `wc -c`, 200 MB of input from a file, boot included | 1.86-1.98 s |
| `wait` for a container that sleeps 1 s | 1.035-1.040 s |

Iteration 10 took 1.01-1.84 s to write 200 MB to stdout in the foreground,
boot included, and iteration 8 1.71-1.76 s for 200 MB through `exec -i`.

The new tests cover:

- the ACPI tables: both sleep registers, `\_S5` ahead of `\_SB`, and the
  sleep control register's values
- frames both ways through the log's framing: each kind, empty ones, a
  write larger than a frame, one too large refused, one cut short
- the output hub: two attached runwhps in order, one attached midway, one
  that stops reading cut off at 4 MiB while the output and the other go
  on, and one that detaches
- input from attached runwhps: in order, ended by the first only when the
  container takes input once
- the detach keys: split across reads, a Ctrl+P that is not followed by
  Ctrl+Q, held keys at the end of the input, and parsing `--detach-keys`
- the control queue taking the time as the console reads the message,
  `RUNWHP_CLOCK_SYNC`, and init's choice to step, slew or leave the clock
- end to end:
  - `reboot -f`, `halt -f` and `poweroff -f` failing with EPERM in the
    container and in an `exec`, which lack CAP_SYS_BOOT, and init
    surviving `kill -TERM 1`, `reboot`, `halt` and `poweroff`
  - the kernel taking the ACPI power-off
  - a reset from a CPU other than the first, now init's own
  - the guest's clock within 50 ms of the host's at boot, put back after
    the container sets it back by years, and left alone with
    `RUNWHP_CLOCK_SYNC=0`
  - `wait`: two containers, one stopped already, one run with `--rm`, a
    missing one, and one whose runwhp is killed meanwhile
  - `attach`: two at once, a stopped container refused, a container run
    in the foreground, input from two attaches in turn to a `run -d -i`
    container, a pseudo console with keys, resizes, Ctrl+P Ctrl+Q and the
    exit code
  - `start -a` of a created container and again once stopped, and
    `start -a -i` with input ending the container's

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel with its vsock modules passed with `--module`.

## Iteration 12 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 11" is that iteration's build, run in turn
with this one.

Runs cost the same, though a run now holds the store's lock while it
resolves its image, over seven runs:

| Measurement | Iteration 11 | Iteration 12 |
| --- | --- | --- |
| `run --rm alpine:3.20 true`, `-m 512m` | 191-206 ms | 194-207 ms |
| the same, default memory | 253-268 ms | 254-267 ms |

Taking the lock and letting it go, file opening included, takes about
0.1-0.14 ms on NTFS and 21 µs on ReFS.

The new commands, on a store of their own holding three images and 100
stopped containers, over five runs, with `list` for scale, which reads
the same containers:

| Measurement | Result |
| --- | --- |
| `list` | 90-96 ms |
| `images` | 98-106 ms |
| `system df` | 100-110 ms |
| `system df -v` | 99-108 ms |
| `rmi python:3.12-slim`, 4 layers | 115 ms |
| `container prune` of the 100 | 572 ms |
| `image prune -a` of the other two images, once the containers were gone | 73 ms |

The 100 containers' writable layers took 338 MiB, about 3.4 MiB each: a
new journaled ext4 file system, sparse, and what `true` left in it.

The test machine's own store held 151 image records, 2.25 GiB of layers
and one kernel, and no containers. 141 of the records, all but ten, were
images two end-to-end tests had served from registries of their own, one
record per run; their layers were mostly alpine's. `rmi` of the 141 took
305 ms and freed seven small layers no other image listed. The store
then held the ten images it had pulled from Docker Hub, all of which
`system df` reports as reclaimable, since no container uses them.

Fixed along the way:

- Two runs pulling one image at once wrote its record to the same
  temporary file, and readers did not let it be replaced; one of twelve
  concurrent runs in the prune race test failed on it. See
  [Findings](findings.md).
- Input that a second attached runwhp sent after a `create -i`
  container's input had ended could still reach the container, about
  half the time.
- `start -a` with its output going to a file printed it twice, once
  through the attach and once from the runwhp it started.

The new tests cover:

- image records: their IDs, recorded and worked out for older records,
  listing past records being written and junk, finding by reference and
  by ID prefix, ambiguous prefixes, the repository and tag Docker shows,
  and one reference recorded from eight goroutines while others read it
- layers: listing, removing, and one a guest holds open, on NTFS and,
  through `RUNWHP_TEST_DIR`, on ReFS; kernels: listing and removing
- records replaced while a reader has them open, and from eight
  goroutines at once
- package gc, on NTFS and ReFS, over a store made by hand: which images
  and containers list each layer, a container whose image was pulled
  again, `--layer` files in the store, shared and unique sizes, what
  dangles and what is unused, `rmi`'s rules, a layer held open, the kept
  kernels, unused volumes, and the lock, shared and exclusive
- `images`, `image inspect`, `rmi`, `image prune`, `container prune`,
  `system df` and `system prune` on stores made by hand, and their
  arguments
- the sweep of image records a pull stopped writing
- end to end, in stores of their own, pulling from a registry in the test
  process:
  - `images`, with the registry's configuration digests as IDs, its
    filters and JSON, and `image inspect` by reference and ID prefix
  - `rmi`, refused for an image a stopped container uses, `-f`, the
    container starting without its image, and its layers pruned once it
    is gone
  - `image prune -a` declined, then keeping the image a running container
    uses
  - pruning in a loop while an image is pulled and while twelve runs pull
    one with `--pull always`, which fails without the lock
  - `container prune` of a stopped and a created container, whose runwhp
    is gone after, beside a running one
  - `system df` counting a container's 50 MB, and its `-v` and JSON
  - `system prune -a --volumes` keeping a running container's image and
    volume and the kernel, and removing the rest
  - `start -a` with its output going to files

The whole suite passes with 4 and 16 vCPUs on runwhp's kernel, and on the
WSL kernel with its vsock modules passed with `--module`. With one vCPU
it passes but for `TestKernelClock`, which fails now and then, as it did
with iteration 11's build: the container can read the clock before init
has set it at boot. See [Known gaps](gaps.md).

## Iteration 13 results

Measured on the test machine with runwhp's kernel, from Git Bash, with the
store on NTFS (`C:`). "Iteration 12" is that iteration's build, run in turn
with this one.

Runs cost the same, though init now waits for the host's time before it
starts the process, over seven runs:

| Measurement | Iteration 12 | Iteration 13 |
| --- | --- | --- |
| `run --rm alpine:3.20 true`, `-m 512m` | 191-198 ms, and 333 ms once | 186-199 ms |
| the same, default memory | 262-266 ms | 256-268 ms |

The guest's clock as the container's process first reads it, with one
vCPU, from `adjtimex` in a foreground run, against the host's before and
after the run, with 50 ms of slack:

| Build | Outside the bound |
| --- | --- |
| iteration 12 | 1 of 50 runs |
| iteration 13 | 0 of 100 runs |

Logs, over four or five runs each. A kept foreground run now also writes
its output to its log, and a log in the background stamps each frame and
rotates:

| Measurement | Iteration 12 | Iteration 13 |
| --- | --- | --- |
| kept foreground run writing 200 MB to stdout, to a file, `-m 1g` | 0.98-1.74 s | 1.08-1.21 s |
| `run -d` writing 200 MB to its log, until it has stopped | 1.03-1.54 s | 1.12-1.52 s |

Console output takes either about 1.0 s or about 1.75 s, run to run, as
before, so the log costs nothing measurable here. The 200 MB left four
files of 20 MiB and the start of a fifth, the oldest dropped.

Reading a full log, five files and 89 MB of numbered lines, the default
limit, three runs each:

| Measurement | Result |
| --- | --- |
| `logs` | 0.46-0.48 s |
| `logs -t` | 0.56-0.57 s |
| `logs -n 10`, which reads the log through twice | 0.70-0.73 s |
| `logs --since 1s`, which prints nothing | 0.45-0.50 s |

Before its output was buffered, `logs` of the same took 3.9-4.0 s, and
`logs -t` 12.4-13.2 s; see [Findings](findings.md).

Restarting: a container in a 512 MiB guest that exits 1 at once, with
`--restart on-failure:20` and `RUNWHP_RESTART_BACKOFF=10ms`, started again
every 121-134 ms, the time between its runs' first lines, so booting it
again takes about 115 ms beyond the delay. Docker's own delays start at
100 ms. Booted 50 times in one runwhp, the runwhp held 206 handles and
112 MiB of commit after 10 restarts, and 212 handles and 94 MiB after 50.

Fixed along the way:

- A kept container whose runwhp died while booting it again, as `start`
  does, was taken by `list` for a record never finished, and removed
  with its writable layer. It is now recorded as stopped, with 255.
- The console's readers of the control port and of attached input
  outlived their VM, waiting for input meant for the next; with restarts,
  they would have taken it.

The new tests cover:

- log files: the format's round trip, rotation at the size given with the
  files' generations, a lower `max-file` dropping files, a log of
  iteration 12 rotated aside, a frame cut short dropped, a damaged log,
  and a write that fails dropped once and reported once, on NTFS and,
  through `RUNWHP_TEST_DIR`, on ReFS
- reading logs: lines over frames and frames of several lines, partial
  lines of both streams, `-t`, `--tail` over files, `--since` and
  `--until` by each line's first byte, following through rotations, and
  a reader that starts, or falls behind into, a file starting in the
  middle of a line
- `--log-driver`, `--log-opt`, `--restart`, `--since` and `--until`
  times, `--tail`, `runwhp.log` kept to two files, and the foreground tee
- restart policies, Docker's delays, and the supervisor's decisions
  without a VM: on-failure's count, a process that never started, a kill,
  a signal to a foreground run, a stop while waiting, `wait` through the
  next run, and `start` leaving a restarting container be
- the store: a dead `restarting` record keeps its exit code, a dead
  container started again is kept, and a lock handed to another process
- the console's control port and attached input ending with their boot,
  and input left for the next
- init waiting for the host's time
- end to end:
  - logs: a million lines rotated at 1 MiB, `logs -f` seeing every one,
    `system df` counting the files, the default 20 MB rotation, a log of
    iteration 12, a foreground run's log, `--log-driver none`, refused
    options, `-t` times within the run, `--since` and `--until` in each
    form, `-n` with a partial last line, and `-f -n 0`
  - restarts: `on-failure:2`, `always` through `stop`, `start` and `kill`,
    a trapped `kill -s HUP`, a container waiting to start again refusing
    `exec`, `attach` and `delete` and left by `container prune`, `wait`
    and `stop` while it waits, `delete -f`, a port taken between runs, a
    runwhp killed while it waits, 50 boots in one runwhp, a foreground run
    handed over, and one interrupted and not

The whole suite passes with 1, 4 and 16 vCPUs on runwhp's kernel, and on
the WSL kernel with its vsock modules passed with `--module`.
