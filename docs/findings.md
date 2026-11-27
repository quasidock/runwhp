# Findings

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
  [Iteration 4 results](status.md#iteration-4-results).
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
- **Windows will not replace a file someone has open, unless they share
  delete and the rename has POSIX semantics.** `os.Rename`, `MoveFileEx`
  underneath, fails with access denied over a file another process has
  open, whether or not that process shares delete. A rename through
  `FileRenameInfoEx` with `POSIX_SEMANTICS` and `REPLACE_IF_EXISTS`
  succeeds if the reader opened with `FILE_SHARE_DELETE`, which `os.Open`
  does not ask for, and fails with a sharing violation otherwise; both on
  NTFS and on ReFS. runwhp reads container records sharing delete, and
  replaces them with a POSIX rename.
- **A reservation is only as good as what it points at.** Container names
  are files in the store's `names` directory, made with exclusive
  creation. With the name taken before its container's record was in
  place, another runwhp found a name pointing at nothing and took it as
  abandoned: on ReFS, two of eight concurrent creates got the same name.
  runwhp now reserves a name once the record exists, and writes the name's
  file aside and hard-links it into place, so that no one reads it empty.
- **Leaving a job object needs the job's leave.** A process started with
  `CREATE_BREAKAWAY_FROM_JOB` from a job without `BREAKAWAY_OK` fails to
  start with access denied, and without the flag it dies when a
  kill-on-close job is closed. With `BREAKAWAY_OK` the flag is enough, and
  with `SILENT_BREAKAWAY_OK` every child leaves the job unasked. runwhp
  asks to break away, and starts the background runwhp inside the job,
  with a warning, when it may not. Starting it, until it reports on an
  inherited pipe, takes about 12 ms.
- **go-winio's dial sleeps 10 ms when the pipe is busy.** `DialPipe`
  retries `ERROR_PIPE_BUSY` every 10 ms, and a listener makes its next
  instance only once the previous one is accepted, so a dial right after
  another can cost 10 ms. Against a server that is ready, a dial and one
  request take about 100 µs, and a request on an open connection 25 µs.
- **Closing a pseudo console twice corrupts the heap.** A second
  `ClosePseudoConsole` of the same handle ended the process with
  `STATUS_HEAP_CORRUPTION`.
- **Keeping a record is cheaper on ReFS, removing one cheaper on NTFS.**
  Making a container's record, a few small files written in the store's
  `tmp` directory and renamed into place, takes 3-5 ms on NTFS and 1-3 ms
  on ReFS; replacing its state 1-3 ms and 0.5-1 ms; removing it 2-3 ms and
  6-8 ms.
- **Linux takes a virtio socket device described in ACPI like any other.**
  The guest's virtio-mmio driver binds its vsock transport to a device
  with ID 19 and a `guest_cid` in its configuration, with no feature bits
  offered: stream sockets are then assumed. WSL's kernel builds that
  transport as modules, `vmw_vsock_virtio_transport_common.ko` and
  `vmw_vsock_virtio_transport.ko`, and their names sort against their
  order of loading, so init tries modules that failed again for as long as
  others load.
- **The hypervisor fills in its page tables one page at a time.** A guest's
  first touch of each 4 KiB page of RAM traps into the hypervisor, which
  then maps it, at about 7.6 µs a page on the test machine, whether or not
  the host already has the page. Linux writes a page descriptor for every
  page of RAM as it boots, 1/64 of it, so an 8 GiB guest spent 250 ms on
  that alone. `WHvAdviseGpaRange` with its populate advice fills in a
  range at once: about 0.2 ms for 2 MiB, mostly zeroing it, and the same
  boot step took 22 ms. runwhp populates each 2 MiB chunk as it maps it.
- **Mapping guest memory costs by its size.** `WHvMapGpaRange` took about
  18 ms per GiB, so mapping 8 GiB before the guest started took 140 ms.
- **An access to an unmapped address always reaches the VMM.** With guest
  RAM reserved but neither committed nor mapped, every first touch of a
  chunk exits as a memory access; committing, mapping and populating the
  chunk and running the instruction again is all it takes. The Hyper-V
  pages the guest's kernel places in its RAM, the hypercall page and the
  reference TSC page, work wherever they land.
- **Decommitting behind a live mapping needs a populate to undo.** With
  `GpaAccessFaultExit` set, a guest touching a page whose host memory was
  decommitted while its chunk stayed mapped exits as a memory access.
  Committing the page again was not enough: the guest faulted again at the
  same address until the page was also populated. That would let runwhp
  give back free blocks smaller than a chunk, but each 4 KiB page the
  guest then touched again faulted, at about 0.45 ms each, so runwhp gives
  back whole chunks only.
- **Two more steps wait for RCU grace periods.** Mounting the cgroup
  hierarchy waited 13-16 ms for one, and the balloon's driver about as
  long as the kernel shut it down, unregistering free page reporting.
  Expedited, each takes well under a millisecond. Expediting grace periods
  for the whole of init's setup was no faster than for those two steps.
- **With RAM above 4 GiB, Linux sets aside bounce buffers.** x86 zeroes
  64 MiB of them at boot for devices that cannot reach high memory.
  No runwhp device needs them, and `swiotlb=noforce` keeps them away.
- **virtio-mmio numbers only the queues a driver uses.** With free page
  reporting the only balloon feature, Linux sets up the inflate, deflate
  and reporting queues as 0, 1 and 2, skipping the statistics and hinting
  queues without leaving gaps.
- **Free page reporting reports in rounds.** Two seconds after RAM is
  freed, the guest's kernel reports free blocks of the reporting order or
  larger, 32 to a request, and only part of what is free each round: 3 GiB
  a guest freed came back in seven rounds over about 14 s. Without huge
  pages, the default order is the page block's, 4 MiB here; runwhp asks
  for 2 MiB, its own unit, which left a little less behind.
- **What the guest keeps ends up everywhere it has been.** After a 4-vCPU
  `go build` in a 4 GiB guest, with its page cache dropped, the guest had
  4 GiB free but runwhp still held about 870 MiB, against 230 MiB before
  the build: some 400 MiB of the guest's free RAM was in blocks smaller
  than 2 MiB, with the pages it kept, mostly its kernel's own, scattered
  over every chunk the build had used. Compaction, built into a test
  kernel and run once the guest was idle, moved little: kernel pages do
  not move.
- **Go's monotonic clock steps by about 100 µs on Windows.** Of a million
  pairs of `time.Now()` calls, all but eleven read the same time, and the
  smallest step was 96 µs. `QueryPerformanceCounter` counts in 100 ns and
  costs about 60 ns a call, so runwhp times exits with it.
- **WinHvEmulation spends most of an MMIO exit translating an address.**
  For each access it emulated, it read 31 registers (0.5 µs), wrote 18
  (0.4 µs) and translated the instruction's virtual address (4-7 µs),
  around a device access of 1.3 µs: 10 µs an exit at boot, 15-20 µs under
  I/O. Every access the guest's kernel made, in a boot and under I/O, was a
  plain mov between a register and memory (`89`, `8B`, `8A`, some with REX),
  whose bytes come with the exit, and decoding it in Go takes one or two
  register calls: 2.3 µs an exit.
- **A network frame over several receive buffers has to arrive whole.**
  With mergeable buffers, the first buffer's header says how many the frame
  takes. runwhp published each buffer as it filled it, and a guest polling
  the ring on another vCPU could find the first before the rest; Linux then
  drops the frame ("buffers missing", counted as a length error on eth0).
  About 120 frames of 200 MB were lost so, and the host's stack retransmitted
  them, with backoff: downloads through host-gateway took 0.7-6.6 s, and
  once stalled for minutes. Published together, they take 0.6-0.75 s.
- **Most of a small FUSE request was the vCPU waiting to be woken.** Of
  about 190 µs per 820-byte write to a bind mount, Windows took 18 µs and
  the three MMIO exits, through the emulator, 55 µs. Serving the request on
  the vCPU that sent it, as blk does, took the write to about 140 µs, the
  fast path to about 100 µs, and the rest of this iteration's changes to
  about 85 µs.
- **EVENT_IDX matters for the network, not the console.** With it, a
  200 MB upload took 30-45% fewer interrupts and a download 60% fewer, and
  the guest no longer notified the device each time it posted receive
  buffers (256 times a boot before). Console output made as many exits
  either way; it takes either about 1.0 s or about 1.75 s for 200 MB,
  run to run, with or without it.
- **Checksum offload is worth little with 64 KiB frames.** With the
  guest leaving its checksums partial and the stack trusting them, a
  200 MB upload took 0.37-0.39 s instead of 0.41-0.59 s and a little less
  system time in the guest; a download took as long. gVisor decides for
  each packet whether to compute a checksum, from the link's capabilities,
  so they can follow the guest's driver.
- **FUSE's writeback cache gathers small writes as a disk would.** On a
  delegated bind mount, 20 MB written in 820-byte pieces went to the host
  as 44 FUSE writes, with 13 setattrs for the times, and took 0.13 s
  instead of 2.4 s. The kernel then trusts its own size and times for the
  files it caches: after the host replaced a file the container had read,
  the container still read the old contents, and its append wrote back the
  whole page over the host's change. A file left open reached the host
  within a few seconds, well before the 30 s the kernel allows.
- **The container could end its own VM.** It runs as root with every
  capability, in init's PID namespace. `reboot -f` reset the VM with no
  exit status, and `halt -f` and `poweroff -f` left it running until
  Ctrl+C, the second because the ACPI tables offered no power-off.
  busybox's `reboot` without `-f`, like `kill -TERM 1`, signals PID 1: Go
  then ended init, and the kernel panicked; `halt` sends SIGUSR1, which
  Go ignores when nothing listens. Dropping CAP_SYS_BOOT from init's
  bounding set, on every thread with `AllThreadsSyscall`, keeps it from
  every process init starts while init keeps it for its own reset.
- **Hardware-reduced ACPI powers off through two FADT registers and
  `\_S5`.** With the sleep control and status registers in the FADT and an
  `\_S5` package in the DSDT, the kernel reports `ACPI: PM: (supports S0
  S5)` and `poweroff -f` writes SLP_EN with sleep type 5 to the control
  register. The kernel then also prepares S5 before every reset, which
  costs about a millisecond.
- **The guest's clock starts 0-1 s behind and stays there.** The kernel
  reads the CMOS stub once, in whole seconds, so a guest's offset from the
  host is a sawtooth over the second it booted in: -0.94 to +0.10 s over
  eight runs. Idle for 45 minutes, it moved by no more than the 100 ms an
  `exec` takes to read it: the reference TSC page keeps time with the
  host's.
- **A time stamped as the console takes it arrives in under 0.5 ms.** Over
  120 messages a second apart, init found the host's time 0.01-0.47 ms
  ahead of its own, idle and with four vCPUs busy alike, after the first
  step at boot. Init leaves offsets under 1 ms alone, and slews up to
  20 ms, what the kernel's 500 ppm makes up within a minute.
- **`ADJ_NANO` is sticky.** Stepping the clock with `ADJ_SETOFFSET |
  ADJ_NANO` sets `STA_NANO` in the kernel's status, and from then on
  every `adjtimex` caller gets nanoseconds in `tv_usec`; busybox's
  `adjtimex` printed `tv_usec: 678660900`. Init steps in microseconds.
- **Modern Standby here does not pause a container.** The test machine
  offers only S0 low power idle, network connected, and no hibernation.
  It entered Modern Standby on its own twice while containers ran, for
  20 minutes and then for hours, but nothing stopped: a detached
  process logged every 30 s throughout, Windows' unbiased interrupt
  time, which leaves out sleep, kept pace with its interrupt time, and a
  guest's clock stayed where it was against the host's. No suspend or
  resume notification came through
  `PowerRegisterSuspendResumeNotification` either. A real suspend, and
  so the resume hook, could not be tried here.
- **Windows corrects its own clock, and a guest left alone does not
  follow.** The Time Service stepped the host's clock back 1.286 s
  against time.windows.com; iteration 10's guest, 0.2 s behind before,
  was 1.1 s ahead from then on, while one syncing every minute stayed
  within 50 ms.
- **A layer a guest has open cannot be removed, or moved aside.** Guests
  open layers for reading, shared only with readers, and `os.Remove` and
  `os.Rename` of one then fail with a sharing violation, on NTFS and on
  ReFS alike, while the guest carries on. A prune relies on it: it never
  has to know which layers running guests hold, and skips any it cannot
  remove.
- **`LockFileEx` is a cheap store-wide lock.** Opening the store's lock
  file, locking a byte of it shared, unlocking and closing it took about
  0.1-0.14 ms on NTFS and 21 µs on ReFS, averaged over 1,000. Any number of
  processes hold it shared at once; an exclusive lock waits for them all,
  and is granted the moment the last holder is killed: Windows releases a
  process's locks however it ends.
- **Without that lock, a prune races pulls and runs.** With the shared
  holds taken out, `image prune` in a loop removed the layers of an image
  being pulled, between their conversion and its record, both times, and
  `image prune -a` broke one or two of twelve runs pulling the image it
  removed. With them, neither happened.
- **An image record's configuration is not the blob.** Records are written
  with `json.MarshalIndent`, which re-indents an embedded raw message, so
  the configuration's hash is not its digest, and not the image ID Docker
  shows: Docker 29.10's ID for `alpine:3.20` is the configuration's
  digest, `bf8527eb`. Compacting the record's copy gave it back for
  `alpine:3.20` and `python:3.12-slim`, but would not for a configuration
  with characters JSON escapes for HTML, so a pull now records the digest
  the manifest names.
- **Layer files are not sparse.** tar2ext4 writes every block, so the 54
  layers in the test machine's store took the 2.3 GB on disk their sizes
  say.
- **Pulls of one image at once collided.** Every pull wrote its record to
  the same temporary file, and readers did not share delete; eight
  goroutines recording one image while others read it failed 64 of 80
  writes. Image records are now written as container records are.
- **The end-to-end tests had filled the test machine's store.** Of its 147
  image records, 137 were images two tests served from registries of their
  own on a random port, one record per run. And Docker Hub allows 100
  anonymous pulls an hour, which the new tests, each pulling into a store
  of its own, used up within a few runs; they now pull from a registry in
  the test process, fed once from Docker Hub.
- **A log can be rotated under its readers.** A file a reader holds open
  through `storefile.Open`, which shares delete, can be renamed, renamed
  again and removed, on NTFS and on ReFS: the name goes at once, and the
  reader goes on reading what it had to its end. Rotation needs no help
  from `logs -f`.
- **A lock can be handed to another process.** Made inheritable and passed
  in a child's handle list, the handle of a container's lock, opened with
  no sharing, keeps the container locked after the parent closes its own
  and exits, and nobody else can take it until the child lets it go.
- **Docker 29.10's own rules settled the edge cases.** `logs -t` prints
  nine digits of fraction; `--tail` counts the lines of both streams, a
  last line without its newline included; `wait` on a restarting
  container waits for its next run to end; `start` of one does nothing;
  and a container sent `kill -s HUP`, which it trapped, then exiting by
  itself later, was not restarted: any kill counts as stopping it by
  hand. `--rm` with `--restart` exits 125.
- **A rotated log can start in the middle of a line.** With a million
  numbered lines rotated at 1 MiB, `logs` began with the end of a line
  whose start was in the file dropped. A file's header now says which
  streams it starts in the middle of a line of, and a reader starting
  there skips to the next line.
- **The guest's process could beat the clock.** On iteration 12's build,
  with one vCPU, the guest's clock was more than 50 ms off the host's as
  the process read it in 1 of 50 runs: init set it on another goroutine,
  and nothing waited for it. Init now waits for the host's first time,
  which comes as it opens the control port, and was off in none of 100.
- **Writing a line at a time is slow.** `logs` wrote each line of a log
  with a write of its own, and formatted each line's time: 89 MB of short
  lines took 4.0 s to print, 12.4 s with `-t`. Buffered, 0.46 s and
  0.56 s.
- **50 boots in one process leak nothing measurable.** A container
  restarted every 10 ms, `true` in a 512 MiB guest, left its runwhp with
  206 handles and 112 MiB committed after 10 restarts, and 212 handles and
  94 MiB after 50.
- **tar-split gives layers back byte for byte from what ext4 keeps.** The
  layers of alpine:3.20, busybox:1.36, debian:12, nginx:alpine,
  postgres:17, node:22, golang:1.26 and python:3.12-slim, 44 in all,
  rebuilt from their tars' metadata and their files, looked up by name as
  tar2ext4 stores them (`path.Clean("/"+name)`, the last entry for a name
  winning), hashed to their diffIDs, padding after the end-of-archive
  marker included; no layer named a file twice. The metadata, gzipped,
  is 0.1-0.4% of a large layer's tar, a few hundred bytes for a small
  one: 2.0 MB for node:22's 1.16 GB, 326 KB for python:3.12-slim's
  124 MB. tar2ext4 writes regular files' contents as the tar holds them,
  and Go's tar reader, which it uses, expands GNU sparse files, which
  would not rebuild; none of these images has one.
- **In series, keeping the metadata made pulls 35% slower.** tar-split
  passes the tar on through a pipe of its own while it parses it, so
  converting, hashing and parsing took turns: pulling golang:1.26 from a
  local registry took 4.6-4.7 s instead of 3.3-3.5. Fed the same 1 MiB
  chunks on goroutines of their own, they run at once, and the pull
  takes 2.7-3.3 s, decompressing included.
- **Docker 29.10's save is an OCI layout plus manifest.json.** With the
  overlay2 store, as WSL's Docker has it, IDs are configuration digests,
  as runwhp's are. The archive holds each layer as a plain tar named by
  its diffID, the configuration, an OCI manifest, index.json, with
  containerd's name and the tag as annotations, manifest.json with
  `LayerSources`, oci-layout, repositories, and a legacy v1 JSON blob no
  loader needs. A repository given without a tag saves every tag of it;
  an image given by ID is saved with `RepoTags: null` and loads as
  `Loaded image ID: sha256:...`. `import` prints the new ID, its history
  says `Imported from -`, `-c RUN` is refused as "not a valid change
  command", and `docker tag` gives the new name no digest in
  `images --digests`.
- **runwhp's save gives Docker's blobs.** Saving alpine:3.20 gave the
  same layer, configuration and OCI manifest, byte for byte, as Docker
  29's save of it, and Docker loaded runwhp's archives of alpine and
  python:3.12-slim with their IDs and ran them; runwhp loaded Docker's
  with Docker's IDs.
- **Docker's layer size counts symlinks.** Docker sizes a layer by walking
  its directory, so a symlink adds the length of its target: alpine's
  layer is 7807582 bytes to Docker, and 7802737 summing its files. runwhp
  reads the targets from the tar's headers in the metadata.
- **A record's configuration can be compacted back to its blob**, for
  alpine and the other registry images on the test machine, which have
  compact, unescaped configurations. But a record written before this
  iteration lacks its layers' metadata too, which only the registry can
  give, so save fetches the blob with them rather than trust a
  reconstruction.
- **Docker 29.10's diff, on WSL, is its naive diff.** WSL's kernel has
  `redirect_dir`, so overlay2 compares the container's root with its
  image's, walking both, and lists the changes in a map's order. It lists
  the mount points of volumes and the working directory (`A /data`,
  `A /anon`, `A /newdir`), but not `/proc`, `/sys`, `/dev`, the `/etc`
  files it binds or `.dockerenv`, which its init layer holds. A directory
  whose times alone changed is not listed (`touch /root`), though it is
  copied up and goes into a commit's layer; one deleted and made again is
  `C`, with its new files `A`.
- **Docker's commit keeps the container's whole configuration**: hostname,
  attach flags, environment, command and entrypoint, working directory,
  user, labels, exposed ports, anonymous volumes and stop signal, as
  `config`, and before `-c` changes as `container_config`, beside the
  container's ID. Its history entry's `created_by` is the command without
  the entrypoint, with `-a`'s author and `-m`'s comment. A container that
  changed nothing gets no layer, and an `empty_layer` entry. An anonymous
  volume's directory goes into the layer, its files do not. `commit` prints
  the new ID; untagged, the image is `<none>`.
- **Docker's cp, from a console-less caller, says nothing**: `quiet` is
  whether stdout is a terminal. Files copied in keep the tar's owners
  unless `-a` gives them the container's user's. A copy onto a symlink in
  the container follows it. A read-only volume refuses with `mounted volume
  is marked read-only`, a missing source with `Could not find the file
  /nope in container NAME`, and a file named as a directory with `lstat
  /d/f1/: not a directory`. A stopped container's volumes and bind mounts
  are copied into and out of.
- **Docker's pause thaws on kill.** `kill -s TERM` on a paused container
  delivers the signal and the container exits; `exec`, `attach` and `rm`
  without `-f` are refused while it is paused, and `ps` shows `Up 1 second
  (Paused)`.
- **go-archive would grow init by 1.42 MB**, to 4.19 MB from 2.77 MB, with
  `TarWithOptions`, `Untar`, `TarResourceRebase` and `ChangesDirs` linked:
  its tars always pass through its compression, zstd, bzip2 and gzip, and
  its logging brings logrus. A run of alpine's `true` took 183-193 ms
  with it, against 182-216 ms without, over eight runs. Init writes and
  reads tars itself instead, as go-archive does, byte for byte; its tests
  check it against go-archive.
- **A volatile overlay leaves its mark when unmounted cleanly**:
  `work/incompat/volatile/dirty` stays, and overlayfs refuses to mount on
  that work directory again until it is removed.
- **What lands in the writable layer today**: for alpine with a network,
  `/etc/resolv.conf`, which alpine lacks; for busybox, `/etc/hostname`,
  `/etc/hosts`, `/etc/resolv.conf`, `/proc` and `/sys`; with volumes and a
  working directory, their mount points, as with Docker. The upper
  directory itself carries `trusted.overlay.uuid` and, once something is
  copied up, `trusted.overlay.impure`.
- **cgroup v2's freezer freezes a busy shell loop in about 13 ms**, in a
  WSL Docker container; a process started with `CLONE_INTO_CGROUP`, as Go
  does with `SysProcAttr.UseCgroupFD`, lands in the cgroup at once.
- **Docker loads runwhp's commits.** An image committed from an alpine
  container and saved by runwhp loaded into Docker 29.10 with runwhp's ID,
  ran, and showed the commit in its history; the save fetched the tar
  metadata of alpine's layer, pulled before iteration 14, through alpine's
  record.
- **containerd 2.4.2 run unelevated locks itself out of a root it makes.**
  It makes `root` and `state` for Administrators and SYSTEM alone, which an
  unelevated user's token cannot open, and every plugin then fails with
  "Access is denied"; directories made beforehand keep the ACL they
  inherit, and work. The CimFS snapshotter fails to start unelevated,
  formatting its scratch disk needing a privilege, and its failure fails
  the metadata store and everything on it; with it disabled, containerd
  starts in 0.02 s.
- **containerd's LCOW pieces need no privilege.** Its windows-lcow differ
  converts a layer with hcsshim's tar2ext4, as runwhp does, into an ext4
  image with a VHD footer, `layer.vhd`, and grants the Virtual Machines
  group read access, which the file's owner may. Its snapshotter makes a
  container's `sandbox.vhdx` by copying `scratch.vhdx` from its root, and
  only when that is missing runs runhcs, which needs an administrator; an
  empty one serves. runwhp boots the layer images as they are: alpine and
  two layers converted so, one with a whiteout and one with an opaque
  directory, read as runwhp's own conversions do.
- **nerdctl 2.4.2 mounts every image it runs on the host**, a view of its
  snapshot, with `mount.All`, which on Windows takes Windows layers alone:
  windows-lcow's mount fails there, and an empty list of mounts passes.
  containerd 2.4.2, deleting a task's bundle, unmounts its `rootfs`
  directory with bindfilter, which an unelevated process may not call,
  unless the directory is missing; and renaming the bundle fails while a
  process has it as its working directory.
- **nerdctl's spec for a Linux image on Windows** has the process as for
  Linux, a numeric user, the image's PATH and an empty `HOSTNAME` without
  `--hostname`, but memory and CPUs in `windows.resources`, no `linux`
  section, `-v` destinations as `C:\data` and an image's VOLUME as `\data`,
  and the hostname, the network and the mounts' kinds in annotations.
  `-p` with `--network none` lands in `network-config.json` in the
  container's state directory, with `0.0.0.0` when no address is given.
  nerdctl dedupes a `-v` on an image's VOLUME by destination, which on
  Windows it writes two ways, and so asks for both. `run -d` logs through
  `binary:///…/nerdctl.exe?_NERDCTL_INTERNAL_LOGGING=…`; foreground runs and
  exec give named pipes nerdctl serves. containerd asks a shim `-info` for
  the deprecated `runtime-allow-mounts` annotation, and logs an error if
  it gets no RuntimeInfo.
- **Linking containerd's API costs runwhp.exe 10.6 MB and no time.** With
  ttrpc and the task and event APIs it grows 8.6 MB, to 29.2 MB, from
  their generated gRPC code; with the LCOW snapshotter and gRPC's server,
  10.6 MB. `list -q` started in 35-39 ms either way, and `run --rm alpine
  true` took 175-225 ms. The shim is a binary of its own, so runwhp.exe
  stays as it was.
- **BuildKit's Dockerfile packages cost runwhp.exe half a megabyte.**
  Its parser, instructions, shell and linter packages, from buildkit
  v0.35.0, added 447 KB, and no measurable time to `list -q` or `run`;
  the module brought grpc 1.85.0, x/sys 0.50.0 and patch releases of
  ttrpc, klauspost/compress and x/net, under which the shim and its
  nerdctl tests are unchanged.
- **How Docker 29's BuildKit writes an image**, over a corpus in
  `out/check17/df`. Each history entry has the comment
  `buildkit.dockerfile.v0`; RUN, COPY and ADD entries end in
  ` # buildkit`, and RUN's list every argument in scope, as
  `RUN |2 A=1 B=bee /bin/sh -c ...`, proxy arguments a stage declares
  included; COPY's leave out `--from`, `--link` and `--checksum`.
  WORKDIR is a layer, its directory owned by the user then in effect. A
  layer's entry is dated when that layer was first made, so even
  `--no-cache` gave the same image ID; an entry with no layer takes the
  first new layer's time, or the time of the layer before it. RUN steps
  have the hostname `buildkitsandbox`, and their layers no mount points,
  hosts or resolv.conf. A cache mount starts 0755 and root's, or as its
  `uid`, `gid` and `mode` say.
- **BuildKit's COPY into a tree that is there** makes missing parents
  0755, owned as `--chown` says, and dated now; leaves a directory that
  is there its mode and owner, giving it the source's time; follows a
  symlinked source and a symlinked destination; takes `--chown=1000` as
  1000:1000, and a name from the image's own passwd and group files;
  refuses names with `--link`; keeps owners copying `--from` another
  stage, but not on the destination directory itself; and unpacks ADD's
  tars with their owners, unless `--chown`. A URL lands 0600, dated by
  `Last-Modified`.
- **BuildKit's client sends files from Windows** owned by root, 0755,
  read-only ones 0555, with their times, hidden ones too, symlinks as
  symlinks, and refuses a junction: "invalid file request".
- **A process runs one WHP partition with memory at a time.** A second
  partition, made while the first runs, fails to map its memory:
  `WHvMapGpaRange` returns HRESULT 0xC0370008 at its first chunk. So a
  build cannot read another stage's guest while its step's guest runs,
  and COPY --from spools what it copies first.
- **klauspost's gzip at its default level** compresses an 81 MB layer of
  python:3.12-slim to 39.3% at 113 MB/s, against compress/gzip's 38.0%
  at 40 MB/s, about as fast as save rebuilds a layer; push uses it.
- **How Docker 29's user-defined networks behave**, recorded in WSL:
  subnets from 172.18.0.0/16 on, past those in use; addresses handed out
  lowest free first as containers start, and given back as they stop;
  random MAC addresses. Only running containers keep a network from being
  removed, and one whose network is gone fails its start with "network ID
  not found". The embedded DNS at 127.0.0.11 answers a container's name in
  any case, its aliases, its 12-digit ID, its hostname and NAME.NETWORK,
  every holder of a shared alias, and PTR as NAME.NETWORK., with a TTL of
  600; resolv.conf has `options ndots:0`, and /etc/hosts names the
  container at each of its addresses. A container on two networks routes
  through the one whose name sorts first, though which is `eth0` varies.
  `network connect` works on a created container, and twice is no error;
  `--ip` outside the subnet and on `bridge`, aliases on `bridge`, `bridge`
  or `none` with a network of the user's, and connecting a `none`
  container are refused, with the texts runwhp now gives. Broadcasts reach
  the other containers, and a ping between two takes 0.1 ms.
- **Named pipes carry frames between processes at GB/s when writes are
  batched.** Between two Go processes with go-winio, frames of 1514 bytes
  went at 2.9-4.0 GB/s through a 256 KiB buffered writer and 0.6 GB/s a
  write each, 66-byte ones at 7-9 million a second and 0.6 million; 64 KiB
  frames at 3.5-5.7 GB/s and 1.5-2.4 GB/s. Message mode was no faster
  than byte mode, and 1 MiB pipe buffers helped only unbatched large
  frames. A 100-byte round trip took 30-40 µs. Go's monotonic clock on
  Windows ticks every 0.5 ms, so times shorter than that are measured over
  many rounds.
- **Linux names virtio-mmio network devices in the order they are
  given**, eth0 first, on runwhp's kernel and WSL's alike, and three of
  them cost nothing measurable at boot: init configures each by its MAC.
- **virtio-net's header must travel with a frame between guests.** A
  Linux guest that takes `VIRTIO_NET_F_CSUM` leaves TCP and UDP checksums
  partial, the pseudo-header's sum in the field, with `NEEDS_CSUM` and
  where to finish it in the header; runwhp dropped the header, which the
  host's stack never needed. A frame for another guest goes with the
  header's flags, to a guest that takes `GUEST_CSUM` as they are, and to
  one that does not finished by the device.
- **WSL's vEthernet sits in 172.20.48.0/20 on the test machine**, and the
  Wi-Fi in 192.168.2.0/24: the pools those overlap, 172.20.0.0/16 and
  192.168.0.0/20, are skipped, as a subnet over the host's would hide its
  network from the containers.
- **A file held open without FILE_SHARE_DELETE is a lease that dies with
  its process.** Opening it for DELETE fails with a sharing violation
  while its holder lives, and succeeds once it has gone, however it went,
  while readers that share delete can read it all along. An endpoint's
  address so goes back to the network the moment its runwhp is killed.
- **How Docker Compose 5.8 runs a project**, recorded with Docker 29.10 in
  WSL. Progress goes to stderr, a line each when it is not a console, with
  a space before and after: ` Container c19-db-1 Started `. up creates
  every container in dependency order, then starts them, printing
  `Waiting` and `Healthy` for each `service_healthy` dependency on every
  up. Containers are labelled with the project, the service, a container
  number, `oneoff`, a `config-hash` of the service less its build, scale,
  pull policy, profiles and dependencies, the image's ID, `depends_on` as
  `name:condition:restart`, the working directory, the config files and
  Compose's version; networks and volumes with their key, the project and
  a hash. A service's containers are aliased by the service's name; a
  one-off, `PROJECT-SERVICE-run-HEX`, by its own name only, with a 64-digit
  slug label. A network or volume of that name made outside compose is
  used, with a warning, and `down -v` skips such a volume. Attached up
  prints `Attaching to a-1, bb-1`, each line as `NAME  | line` padded to
  the longest name, a container's stderr to its own stderr, and
  `\r\e[Kbb-1 exited with code 3` as each stops; Ctrl+C gives exit 130,
  `--abort-on-container-exit` the exiting container's code, and an up of
  services that all exit gives 0. `logs -f` keeps following after the
  containers stop. Error texts, column layouts and project-name
  normalisation (lowercase, other characters dropped) are in
  `out/check19/notes.md`, which the tests follow.
- **Docker 29's healthchecks**: failures in the start period do not count
  while the container is starting, and checks then come every start
  interval; one success makes it healthy, and from unhealthy too; after the
  start period, as many failures in a row as the retries make it unhealthy,
  and the streak keeps counting. The last five results are kept, each with
  4096 bytes of output and `...`; a check past its timeout has exit -1 and
  `Health check exceeded timeout (1s): ` with what it wrote. A paused
  container is not checked, and `ps` shows `(Paused)` instead of its
  health; a restart starts again as `starting` with the results kept, and
  a stopped container keeps its last health.
- **Docker's ps filters**: values of a key are alternatives, except
  `label`, whose values must all match; `name` is a regular expression,
  case-sensitive; there is no `label!=`. `--label-file` is an env file: a
  key's leading spaces dropped, its value kept as written, a key alone
  taken from the environment or left out.
- **compose-go v2.17.0**, which Docker Compose 5.8 links, needs Go
  1.26.11, and was 10 days old; v2.16.2 needs Go 1.24, and differs in its
  dependencies only. It adds 2.86 MB to runwhp.exe. It reads Windows bind
  sources as they are, absolute or made absolute against the project
  directory, keeps `consistency: delegated` from the long syntax but drops
  `:delegated` from the short one, reads a one-letter volume name such as
  `v:/v` as a drive, and makes a range of ports single ones.
- **An exec round trip costs about 5 ms beyond starting runwhp**, whose
  start takes about 61 ms: `exec CONTAINER true` from the command line
  took 61-78 ms, and each cost the container's runwhp about 18 ms of CPU,
  the guest's included. A healthcheck every second so costs about 2% of
  a core.
- **Booting containers at once scales**: 8 `run -d` at once took 0.58-0.61 s
  against 1.46-1.50 s one after another, 5 took 0.41 s against 0.93-0.96 s,
  with no failures, and 8 at the default memory 0.90 s. The image store
  takes pulls from several goroutines, the same image twice included.
- **compose-go would slow every runwhp's start by 8 ms.** Its JSON Schema
  library, santhosh-tekuri/jsonschema/v6, compiles five drafts'
  metaschemas in a package init, 5.9 ms by `GODEBUG=inittrace=1`, and the
  larger binary loads slower: `runwhp help` took 44 ms against 36, and
  `run -d`, which starts two runwhps, 15-20 ms more. compose so runs in
  runwhp-compose.exe, the same command line built with `-tags compose`,
  whose containers' runwhps are runwhp.exe; it costs compose about 25 ms.
- **The docker CLI 29 against a pipe of runwhp's own**: Docker's static
  `docker.exe` 29.9.3 speaks API 1.56, Moby's `github.com/moby/moby/api`
  v1.56.1, so it negotiates nothing; Docker 29.10's daemon speaks 1.57,
  down to 1.40. A `/_ping` answering `Builder-Version: 1` and a CLI with
  no buildx give `docker build` the classic builder's endpoint, with a
  deprecation warning, and the context as a tar.
- **Hijacked streams over a named pipe need message mode.** The docker CLI
  half-closes its end once its input ends, and at once when it gives
  none; go-winio carries a half-close only on a message-mode pipe, as
  dockerd serves `docker_engine`, where a byte-mode pipe would end the
  connection. A server that took that half-close for the client leaving
  lost every attached run's output.
- **Docker 29's waits and names**: `wait?condition=not-running` on a
  created container answers 0 at once, and `next-exit` waits for its next
  run, on an exited container too; a container named with one character
  is refused (`name must be at least 2 characters`); a missing container
  is `No such container: NAME`, 404.
- **Docker 29's `/system/df`** gives both the usage objects of API 1.52
  (`ImageUsage`, `ContainerUsage`, `VolumeUsage`, `BuildCacheUsage`) and
  the lists they replaced (`LayersSize`, `Images`, ...), whatever the
  version asked; a build cache record's `Parents` key is spelt
  `" Parents"`.
- **The docker CLI's `images --filter reference=`** lists a row for each
  name and each repository digest the daemon gives an image, so a daemon
  must narrow both to those that match, as Docker's does.
- **A process with no console that starts a console program makes
  Windows give the program a console of its own**, unless it is started
  with `CREATE_NO_WINDOW` or `DETACHED_PROCESS`: about 300 ms each on the
  test machine. runwhp's background processes are detached, so every
  console program they start needs one of those flags.
- **docker.exe 29.9 takes 93-94 ms to start**, printing its version and
  connecting to nothing, which is most of what any docker command
  through runwhp's API costs beyond runwhp's own.
- **How Docker Compose 5.7 chooses its builder**: it asks the docker CLI
  whether BuildKit is on, which it is unless `DOCKER_BUILDKIT=0`, a
  daemon's `/_ping` says `Builder-Version: 1` *and* its OS type is
  windows; for a Linux daemon such as runwhp's it is on. It then looks
  for the buildx plugin, and without it warns that it falls back to the
  classic builder, which `/build` serves; with it, it runs `buildx bake`,
  which needs BuildKit's `/session` and `/grpc`. Its classic builds read
  the image's ID from the stream's `aux` message.
- **Random host ports, as each client asks**: Docker Compose (`ports:
  ["80"]`), Testcontainers for Java and .NET, and docker-py send
  `HostPort: ""`; testcontainers-go sends `"0"`; none sends
  `PublishAllPorts`. testcontainers-go publishes the image's
  `ExposedPorts` when given none, and reads the port bound from
  `NetworkSettings.Ports`, never `HostConfig.PortBindings`, which Docker
  29 leaves as asked. Docker 29 binds a random port on `0.0.0.0` and `::`
  alike, at the same number.
- **Testcontainers copies files into a created container, gzipped**:
  `PUT /containers/{id}/archive` with a gzip-compressed tar, before
  start, which Docker's daemon decompresses.
- **A container's pipe cannot carry a half-close**: it is a byte-mode
  pipe, so the end of a tar sent through it reaches init only as the
  tar's own end marker. A tar that ended early, as a gzipped one read
  plain does, left init waiting for the rest; the host now reads the tar
  as it sends it.
- **docker-py's `containers.run()` without `detach`** returns the
  container's output only if its inspect's `HostConfig.LogConfig.Type`
  is `json-file` or `journald`; otherwise it returns `None`, having read
  nothing.
- **Who finds the API how**: the docker CLI, Docker Compose, docker-py
  7.2 and Testcontainers for .NET follow the docker CLI's current
  context; testcontainers-go, Testcontainers for Java and Docker's Go SDK
  read only `DOCKER_HOST`, or their own settings, else Docker's
  `\.\pipe\docker_engine`. The Dev Containers CLI resolves the context
  itself and passes `DOCKER_CONTEXT` to every docker command.
- **Dev Containers' CLI** waits for its container with `docker events
  --filter event=start`, then keeps a `docker exec -i ... /bin/sh` open
  as a shell server; it checks `docker buildx version` and, without
  buildx, builds features with two classic `docker build`s, the second
  multi-stage with `ARG` before `FROM`.
- **Docker 29's events of a stop**: `kill` with signal 15, another with 9
  if the timeout passes, `stop`, then `die`; a restart's `die` carries
  `restarting: true`, and `start` and `restart` follow.
- **alpine:3.20's busybox has no `httpd`**; its `nc -lk -p PORT -e cat
  FILE` serves a file to every connection.
- **Which socket testcontainers-go binds into Ryuk**: Docker's own
  `//var/run/docker.sock` only when `/info` says `OperatingSystem: "Docker
  Desktop"`; for any other daemon, the path of the pipe it reached the
  daemon on, `npipe://` taken off, as in `//./pipe/runwhp_engine`, unless
  `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` says otherwise. Ryuk 0.14.0
  itself only pings, lists containers, networks, volumes and images by the
  session's labels, and removes them: containers with `force` and `v`,
  volumes with `force`, images with `noprune` false. It prunes nothing,
  and skips what was made after it began reaping.
- **testcontainers-go labels the volumes it mounts** through the mount's
  `VolumeOptions.Labels`, and Ryuk finds them by those labels: a daemon
  that makes the volume without them leaves it behind.
- **Docker Compose's `use_api_socket`** binds
  `/var/run/docker.sock:/var/run/docker.sock:rw`, whatever the daemon's
  socket; with credentials stored, it adds `DOCKER_CONFIG=/run/secrets/docker`
  to the container's environment and puts `run/secrets/docker/config.json`,
  the credentials as `auths`, into the created container with `PUT
  /archive` before starting it; with none, it does neither.
- **Dev Containers' docker-outside-of-docker 1.10** binds
  `/var/run/docker.sock` to `/var/run/docker-host.sock` with `--mount`,
  asks `--security-opt label=disable`, and for a non-root user runs
  `socat` as root, through sudo, to a socket at `/var/run/docker.sock`
  that user can open. The CLI passes mount sources as written.
- **Microsoft's devcontainers/base images list an empty layer**, the
  1024 zero bytes of an empty tar, whose diffID is
  `sha256:5f70bf18…`, which a store need not hold.
- **A half-close crosses a pipe in message mode only**: go-winio's
  `CloseWrite` writes a zero-length message, which its reader returns as
  end of file. Carried from a guest's `shutdown(SHUT_WR)`, through vsock's
  `SHUTDOWN` with its send flag, to `CloseWrite` on the pipe, and back
  the same way, a docker CLI in a container ends an attach's input as on
  the host.
- **`os.Stat` of a named pipe does not connect to it**: one that does not
  exist gives "cannot find the file", and one that does, busy or not,
  another error, so it tells them apart without taking an instance.
