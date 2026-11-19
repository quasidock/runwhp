# runwhp - minimalistic container runtime for Windows

`runwhp` is an experimental OCI runtime that runs Linux containers on any Windows
edition, including Home. It drives the Windows Hypervisor Platform (WHP)
directly from an ordinary user process, so it needs no Hyper-V role, no WSL, no
HCS, and no administrator rights at run time.

Quasidock itself runs native Windows programs and does not run Linux
containers. `runwhp` is a separate subproject for software that has no Windows
build.

## Status

`runwhp` is at the design stage. Nothing here is a usable runtime yet.

- **Proven by a spike** (2026-11-17): an unelevated Go program boots a Linux
  kernel on WHP, delivers device interrupts, attaches virtio block devices, and
  runs a process chrooted into an overlay root built from an ext4 image layer.
  See [Boot spike](#boot-spike).
- **Proposed, not yet agreed**: the architecture in
  [Proposed design](#proposed-design).
- **Not started**: networking, host file sharing, more than one vCPU, the OCI
  runtime command line, and a kernel of our own.

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

## Proposed design

One small virtual machine per container, with the VMM process acting as the
container's supervisor. This matches Quasidock's existing model of one
supervisor process and one Job Object per container, with no daemon.

| Concern | Approach | State |
| --- | --- | --- |
| Hypervisor | WHP from Go through `WinHvPlatform.dll`, no cgo | Spiked |
| Boot | Direct kernel boot (Linux 64-bit boot protocol), no firmware | Spiked |
| Clock and timers | Hyper-V enlightenments supply TSC and APIC timer frequencies | Spiked |
| Interrupts | Hypervisor's local APIC plus an emulated I/O APIC | Spiked, edge-triggered only |
| Device discovery | Hardware-reduced ACPI with a generated DSDT | Spiked |
| Root filesystem | One read-only ext4 image per layer on virtio-blk, overlayfs in the guest, writable scratch disk | Spiked with a single layer and a tmpfs upper |
| Layer conversion | `tar2ext4`, cached per layer digest | Not started |
| Guest agent | Tiny init as PID 1 that mounts the overlay and execs the process, instead of hcsshim's GCS plus runc | Spiked as a test program only |
| Bind mounts and volumes | 9p server in Go on the host, carried over vsock | Not started |
| Networking | User-mode network stack in the VMM (gVisor netstack); published ports are loopback listeners | Not started |
| Command line | runc-compatible `create`, `start`, `state`, `kill`, `delete`, `exec` | Not started |
| Kernel | Our own build with virtio drivers built in, shipped as a signed OCI artifact | Not started |

### Relationship to Quasidock

`runwhp` is meant to stand alone, with no dependency on Quasidock's store or
recipes. Quasidock could later call it for images that have no native Windows
build, or when [`quasidock convert`](dockerfile-conversion.md) rejects a
Dockerfile. Whether Quasidock should do that is undecided.

### Out of scope for now

Linux GUI applications, GPU access, ARM64 hosts, and running many containers in
one shared VM.

## Boot spike

The spike lives in [`spike/`](spike/) and is throwaway evidence, not
the start of the runtime. It is about 1,800 lines of Go, guest init included,
with no dependencies beyond the standard library.

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
go build -o out/spike/runw-spike.exe ./spike

$env:GOOS = 'linux'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags="-s -w" -o out/spike/init ./spike/init
$env:GOOS = $null; $env:CGO_ENABLED = $null
```

Kernel and userspace only:

```powershell
.\out\spike\runwhp-spike.exe -init out\spike\init
```

With disks. The fixture script runs inside WSL and needs `e2fsprogs`; pass it
the WSL form of your checkout's `out/spike` path:

```powershell
wsl -e sh /mnt/e/quasidock/spike//fixtures.sh /mnt/e/quasidock/out/spike
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

- The serial port takes no input, so there is no interactive console.
- Interrupts are edge-triggered only; level-triggered lines and EOI tracking
  are not implemented.
- One vCPU. Starting further processors is untested.
- Device emulation blocks the vCPU. Disk requests should move to worker
  goroutines.
- Guest memory is committed up front and never returned to Windows.
- The guest clock starts at whatever the RTC stub reports and is not kept in
  step with the host afterwards.
- The test disk image is built with `mkfs.ext4` inside WSL. The runtime must
  generate ext4 itself.
- String port I/O is not handled and stops the VMM with an error.
- No tests, and no way yet to run this in CI: it is unknown whether hosted
  Windows runners expose WHP.

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
