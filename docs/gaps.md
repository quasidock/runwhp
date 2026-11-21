# Known gaps

- There is no detaching or reattaching, and no way to send signals other
  than SIGINT and SIGTERM.
- Nothing written outside a bind mount or volume survives the run: the
  scratch disk is deleted when it ends.
- Volumes cannot be resized, nor shared by containers running at once, even
  read-only. `runwhp volume` cannot copy files in or out, and reading one
  from Windows needs WSL.
- Image `VOLUME` declarations are ignored: those paths stay in the writable
  layer, as they would with `docker run --rm`.
- A volume mounted on `/etc` keeps, after its first run, its own `hosts`,
  `hostname` and `resolv.conf`, rather than those runwhp writes.
- A full host disk shows in the container as I/O errors on the scratch disk
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
- An image can have at most 56 non-empty layers, one more with
  `--network none`, one fewer with bind mounts, and one fewer for each
  volume: each device, the scratch disk included, takes an I/O APIC line.
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
