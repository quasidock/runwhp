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
