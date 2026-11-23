# Known gaps

- Nothing attaches to a container in the background: there is no
  `attach`, `start -a` or `wait`. Its output is in its log or the files it
  was given, and its stdin can only be a file or a pipe.
- There are no restart policies and no `container prune`; containers a
  plain `run` keeps stay until `delete`.
- Logs grow until the container is deleted, carry no timestamps, and are
  not rotated.
- A foreground `run` ends with its terminal. A container in the background
  outlives the terminal, but not a logoff, which ends it as killing its
  runwhp would, nor a session whose job object keeps its processes in, as
  some ssh servers and CI runners do. Whether it outlives closing Windows
  Terminal, VS Code or mintty has been checked only with a pseudo console
  closed under it.
- A container run with `--rm` cannot be restarted.
- A container in the background holds `runwhp.exe` open, so a build of
  runwhp has to move it aside, as `build.ps1` does.
- Keeping a record makes `run alpine:3.20 true` about 10 ms slower with
  `--rm`, and about 35 ms slower for a kept container, whose writable layer
  is frozen at the end, than in iteration 7; the vsock device is about 5 ms
  of it.
- `runwhp exec` needs vsock in the guest's kernel. runwhp's has it; WSL's
  builds it as modules, which have to be passed with `--module`.
- Volumes cannot be resized, nor shared by containers running at once, even
  read-only. `runwhp volume` cannot copy files in or out, and reading one
  from Windows needs WSL.
- Image `VOLUME` declarations are ignored: those paths stay in the writable
  layer, as they would with `docker run --rm`.
- A full host disk shows in the container as I/O errors on the writable layer
  or a volume.
- Every `write()` to a bind mount is a request to the host, about 130 µs each
  on the test machine, so a program writing in small pieces is slow there;
  see [Iteration 5 results](status.md#iteration-5-results). Making files is slow too,
  at 3-4 ms each, mostly in Windows itself.
- Host changes in bind mounts raise no inotify events in the container.
  Extended attributes cannot be set there, and file locks are not shared with
  host programs.
- The kernel's first fetch needs ghcr.io, unless `--kernel` names one. A new
  kernel build reaches runwhp only when someone moves the pin in
  `internal/kernel/pin.go`, and the pinned digest is the only check on it:
  builds are not signed.
- An image can have at most 54 non-empty layers, one more with
  `--network none`, one fewer with bind mounts, and one fewer for each
  volume: each device, the writable layer, the vsock device and the
  balloon included, takes an I/O APIC line.
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
- A guest gives back only the RAM it has free in whole, aligned 2 MiB
  blocks. Without compaction in runwhp's kernel, the pages a guest keeps,
  its own kernel's included, end up scattered over the RAM it has used:
  after a `go build` in a 4 GiB guest freed its memory, about 600 MiB of
  it stayed with the guest, in blocks too small to report.
- `runwhp stats` keeps no history, and shows only running containers.
- A guest gives back its page cache only once it is idle, and gradually:
  a busy container keeps what it has cached, up to all its free RAM.
- A guest that hands the balloon RAM it still uses can make its own VMM
  fault on it, and end its own container.
- Every guest's kernel describes all of its RAM, in 1/64 of it, which
  each container holds for as long as it runs: 128 MiB with 8 GiB of
  RAM. Hot-plugging memory (virtio-mem) would avoid it.
- The guest clock starts at whatever the RTC stub reports and is not kept in
  step with the host afterwards.
- String port I/O is not handled and stops the VMM with an error.
- It is unknown whether hosted Windows CI runners expose WHP, so the
  end-to-end tests run only by hand.
