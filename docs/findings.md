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
