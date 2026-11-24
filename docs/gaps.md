# Known gaps

- `attach` to a container run in the foreground shows its output, but
  cannot type into it: its input belongs to the runwhp that runs it. An
  attached runwhp that falls 4 MiB behind the container's output is cut
  off, rather than holding the output up.
- Restart policies end with the runwhp running the container: a logoff,
  or killing that runwhp, ends the container for good, and nothing starts
  containers at logon, so `unless-stopped` is `always`. There is no
  `update --restart`: a container's policy is the one it was made with.
- A newer runwhp's `system prune` can remove the kernel an older runwhp's
  container is waiting to restart on; it then fails to boot, and is
  tried again every minute.
- Logs are not compressed, and take no `--details`. A `logs -f` that falls
  more files behind than the log keeps loses what was rotated away, and
  says nothing of it. stdout and stderr are interleaved in the order
  runwhp received them, which, for writes close together, may not be the
  order the container made them in.
- A foreground `run` ends with its terminal. A container in the background
  outlives the terminal, but not a logoff, which ends it as killing its
  runwhp would, nor a session whose job object keeps its processes in, as
  some ssh servers and CI runners do. Whether it outlives closing Windows
  Terminal, VS Code or mintty has been checked only with a pseudo console
  closed under it.
- A container run with `--rm` cannot be restarted, nor given `--restart`.
- A container handed over to restart, after a foreground `run`, is served
  by no runwhp for a moment: commands sent to it wait while its lock is
  held, but a `wait` already waiting on the foreground runwhp loses its
  answer, and waits 10 s for the container to stop before failing.
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
- Every `write()` to a bind mount is a request to the host, about
  90-110 µs each on the test machine, so a program writing in small pieces
  is slow there, unless the mount is delegated; see
  [Iteration 10 results](status.md#iteration-10-results). Making files is
  slow too, at about 3.5 ms each, mostly in Windows itself, delegated or
  not.
- A delegated bind mount's writes reach the host late, are lost if the
  container is stopped hard, and can overwrite host changes to files the
  container has cached; see [Delegated mounts](../README.md#delegated-mounts).
  Large writes followed by an `fsync` take about twice as long on one,
  copied into the guest's page cache first.
- Host changes in bind mounts raise no inotify events in the container.
  Extended attributes cannot be set there, and file locks are not shared with
  host programs.
- The kernel's first fetch needs ghcr.io, unless `--kernel` names one. A new
  kernel build reaches runwhp only when someone moves the pin in
  `internal/kernel/pin.go`, and the pinned digest is the only check on it:
  builds are not signed.
- An image can have at most 54 non-empty layers, one more with
  `--network none`, one fewer with bind mounts, one fewer again with
  delegated ones, and one fewer for each volume: each device, the writable
  layer, the vsock device and the balloon included, takes an I/O APIC line.
- The network is IPv4 only and carries no ICMP. Only TCP ports can be
  published, each with an explicit host port, and there is no `-P`, `--dns`
  or `--dns-search`.
- The network device has one queue pair and no segmentation offload, which
  the link's 65520-byte MTU makes up for in part: 200 MB crosses in about
  0.4 s out of the container and 0.6-0.8 s into it on the test machine.
- The prunes take no `--filter`, such as `until=` or `label=`, and
  `images` no filter but a repository. There is no `tag`, `save`, `load`
  or `history`, and no build, so `system df` has no build cache.
- An image's SIZE is that of its ext4 layers, as the store holds them,
  so it differs from Docker's for the same image.
- `system prune` removes every kernel but the one this runwhp boots, so
  a runwhp of another version sharing the store fetches its kernel again.
- `system prune --volumes` removes named volumes no container uses, where
  Docker's removes only anonymous ones; runwhp's anonymous volumes live in
  their containers' writable layers and go with them.
- `container prune` stops the VMs of created containers to remove them,
  since a created container's VM is already booted.
- Nothing is pruned on its own: the store grows until a prune or `rmi`.
- Interrupts are edge-triggered only; level-triggered lines and EOI tracking
  are not implemented.
- The container cannot reboot, halt or power off its VM: it lacks
  CAP_SYS_BOOT, as Docker containers do, and `reboot -f` fails with EPERM.
  There is no `--cap-add` to give it back. It shares init's PID namespace,
  so it can see init and signal it, though init ignores the signals.
- `--cpus` is a count, not Docker's quota, and there is no `--cpuset-cpus`.
- Each device has one virtqueue, which every vCPU shares.
- Every interrupt from a virtio device costs two exits beyond the one that
  asked for it, as the guest reads its interrupt status and acknowledges
  it: virtio-mmio has no other way, where MSI-X with virtio over PCI would.
  A request to a bind mount takes three exits in all.
- Device register accesses other than plain movs, which Linux does not
  make, still go through `WinHvEmulation.dll`, several times slower.
- 200 MB of console output takes either about 1.0 s or about 1.75 s, run to
  run, for reasons not yet known.
- A guest gives back only the RAM it has free in whole, aligned 2 MiB
  blocks. Without compaction in runwhp's kernel, the pages a guest keeps,
  its own kernel's included, end up scattered over the RAM it has used:
  after a `go build` in a 4 GiB guest freed its memory, about 600 MiB of
  it stayed with the guest, in blocks too small to report.
- `runwhp stats` keeps no history, and shows only running containers.
- A guest gives back its page cache only once it is idle, and gradually:
  a busy container keeps what it has cached, up to all its free RAM, and
  an idle one keeps 2% of its RAM cached, 157 MiB with this machine's
  default.
- A guest that hands the balloon RAM it still uses can make its own VMM
  fault on it, and end its own container.
- Every guest's kernel describes all of its RAM, in 1/64 of it, which
  each container holds for as long as it runs: 128 MiB with 8 GiB of
  RAM. Hot-plugging memory (virtio-mem) would avoid it.
- The guest's clock follows the host's, but only every minute and when
  Windows resumes: a container that sets its own clock has it put back.
  The resume hook is untested: the test machine's Modern Standby never
  paused a container, and it has no other sleep state.
- With `RUNWHP_CLOCK_SYNC=0`, the guest keeps the CMOS stub's whole
  seconds, up to a second behind the host's clock.
- String port I/O is not handled and stops the VMM with an error.
- It is unknown whether hosted Windows CI runners expose WHP, so the
  end-to-end tests run only by hand.
