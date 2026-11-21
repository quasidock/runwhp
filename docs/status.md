# Status

- **Iteration 7** (2026-11-21): containers write to disk. The writable
  layer lives on a sparse ext4 scratch disk made for each run instead of in
  guest RAM, so a 256 MiB guest writes gigabytes. Named volumes
  (`-v NAME:PATH`, `runwhp volume`) keep data between runs in journaled
  ext4 disk images, filled from the image as Docker fills them, and
  anonymous volumes and `--tmpfs` work too. A Go build cache in a volume
  takes a build of runwhp from 21 s to 3 s, and a run costs about 15 ms
  more, plus 10-20 ms per volume. See [Storage](#storage),
  [Volumes](#volumes) and [Iteration 7 results](#iteration-7-results).
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
- **Not started**: the runc-style lifecycle commands.

 **On AMD Zen, 6.18 reads the FCH at `0xfed80000`** to report why the
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
- **hcsshim's `tar2ext4` cannot make a writable disk.** Every file system it
  writes is marked read-only (`RO_COMPAT_READONLY`), has no journal, uses
  `sparse_super2`, which online resize refuses, and holds only as many
  inodes as its content needs: 16 per group when empty. runwhp formats its
  writable disks itself, as mke2fs would, and `e2fsck -fn` passes them.
- **A new ext4 file system is mostly holes.** With group descriptor
  checksums, groups can be marked as having uninitialized bitmaps and
  zeroed inode tables, and the kernel works them out instead of reading
  them; since holes in a sparse file read as zeros, that is true of every
  inode table. Formatting 64 GiB writes 373 KiB. Without the zeroed flag the
  kernel's lazy init would write 1 GiB of inode tables.
- **`os.File` runs positional I/O one request at a time on Windows.** Its
  `ReadAt` and `WriteAt` hold a lock for the whole call, and a handle opened
  for synchronous I/O makes Windows serialize requests on it as well. With
  an overlapped handle and an event per request, four goroutines read
  cached 4 KiB blocks 1.5 times as fast as one, where `os.File` gets
  1.6 times slower.
- **NTFS gives a sparse file's space back in 64 KiB units, ReFS per
  cluster.** `FSCTL_SET_ZERO_DATA` on a 4 KiB range of a sparse file on NTFS
  frees nothing; on ReFS, with 4 KiB clusters, it frees the range. NTFS also
  allocates 64 KiB at a time, so a new 64 GiB volume, whose metadata is
  scattered over a few dozen places, takes about 3 MiB there. runwhp asks
  the guest to discard in 64 KiB units.
- **Deleting a sparse file takes about 4 ms** on both NTFS and ReFS, even
  when little of it was written: most of what the scratch disk adds to a
  run.
- **Windows deletes a delete-on-close file when the process holding it is
  killed**, and will not rename a directory holding a file open without
  `FILE_SHARE_DELETE`. The first makes a killed runwhp leave no scratch
  disk behind, the second lets `volume rm` move a volume aside whole or
  find it in use, with no lock files.
- **Freezing a journaled ext4 file system leaves it clean** without
  unmounting it: `FIFREEZE` writes back its data, empties its journal and
  clears `needs_recovery`, whoever still has it mounted. Unmounting cannot
  be counted on in a container, which may have copied its mounts into
  namespaces of its own.
- **Handing every disk request to another thread made a single reader 40%
  slower.** The vCPU halts while it waits, and waking it for the
  completion, as well as the thread that does the I/O, cost about 86 µs
  per request: reading the Go toolchain out of an image took 5.0-5.3 s
  instead of 3.4-3.9 s. Workers spinning for the next request made it
  worse. Serving the first request of each notification on the vCPU that
  sent it, after letting go of the device's lock, brought one reader back
  to 3.8-4.1 s and made four readers faster than either.
- **A freeze waits for three RCU grace periods**, one per level it freezes,
  and in an idle guest each takes tens of milliseconds: a volume added
  about 60 ms to a run. Init asks for expedited grace periods just before
  freezing, once the container is gone, and a volume adds 10-20 ms.
- **Debian's images already skip most fsyncs.** Their dpkg is configured
  with `force-unsafe-io`, so overlayfs's `volatile`, which drops the
  writable layer's syncs, gains only about 5% on `apt-get install`.

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
did, took 5.0-5.3 s for one reader; see [Findings](#findings).

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
