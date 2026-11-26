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
  read-only. `runwhp volume` cannot copy files in or out: `cp` reaches a
  volume only through a container that mounts it.
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
  delegated ones, one fewer for each volume, and one fewer for each network
  beyond the first: each device, the writable layer, the vsock device and
  the balloon included, takes an I/O APIC line.
- The network is IPv4 only, and carries no ICMP to the host or beyond it:
  only containers on a user-defined network ping each other. Only TCP
  ports can be published, each with an explicit host port, and there is
  no `-P`, `--dns` or `--dns-search`.
- `network connect` and `disconnect` refuse a running container: its VM
  cannot take or give up a network device while it runs. A created
  container's VM is booted again for them.
- `network create` takes no `--subnet`, `--gateway`, `--ip-range`,
  `--internal`, `--ipv6` or `-o`, and no driver but `bridge`:
  runwhp chooses each network's subnet as it is made, and a host route
  that comes later may hide part of the host's network from its
  containers. `--ip` needs no `--subnet`, where Docker's does.
- There is no `--network host`, `container:NAME`, `--link`,
  `--mac-address` or `gw-priority`, and a container on runwhp's default
  network cannot join another, as one on Docker's `bridge` can. MAC
  addresses come from the containers' addresses, where Docker 29's are
  random, and the interface that sorts first by its network's name is
  always `eth0`.
- nerdctl's containers, and build steps, cannot join a user-defined
  network.
- DNS on a user-defined network is answered at its gateway, not at
  `127.0.0.11` as Docker's is, so a program told to ask `127.0.0.11`, as
  nginx often is, gets no answer. Answers live 60 s, not 600. A name no
  container has goes to Windows's resolver, which takes about 2.7 s to give
  up on a single-label one.
- Network names are case-insensitive, as container names are, where
  Docker's are not. Networks belong to a store: containers run with
  different `RUNWHP_ROOT`s do not meet.
- A ping between containers takes about 1 ms, where Docker's takes 0.1 ms:
  each wakes two guests. For up to 2 s after a container stops, the others
  may still find its address, and their frames for it are dropped; and
  for up to 2 s after one starts, a name or alias others already find
  elsewhere, a scaled service's say, may be answered without it.
- `compose` refuses a named volume two services would mount at once, or a
  service with replicas mounts, where Docker shares it: a runwhp volume is
  a disk only one running VM can have. Bind mounts can be shared.
- `compose up` stops a container it makes again before making the new one,
  as a volume or an address is one VM's at a time, so a service is down
  while it is replaced; Docker Compose makes the new one first. Anonymous
  volumes live in the container's writable layer, and go with it, where
  Docker Compose carries them over to the new container.
- `compose` refuses UDP ports and ports without a host port, as `run`
  does, and publishes ports on 127.0.0.1 unless an address is given, where
  Docker publishes them on every address.
- `compose build` builds one service after another, a step at a time, where
  BuildKit builds them at once. `cpus` is rounded up to whole vCPUs.
- There is no `compose watch`, `top`, `events`, `cp`, `attach`, `stats`,
  `wait`, `scale`, `export`, `commit`, `publish` or `alpha`, and no
  animated progress: compose writes Docker Compose's plain progress lines,
  as it does when its output is not a console. `depends_on`'s `restart:
  true` restarts nothing.
- `compose` reads compose files with compose-go v2.16.2; Docker Compose
  5.8 has v2.17.0, which needs a newer Go than runwhp's.
- Attached `compose up` shows the containers' output from when it started,
  not their earlier logs, and follows the containers there were then.
  `compose logs -f` follows the containers there were when it started.
  `--timestamps` prints UTC times, where Docker Compose's are local.
- `compose`'s containers boot runwhp's kernel, or `RUNWHP_KERNEL`'s, never
  with `--module`.
- The Engine API is tested with the docker CLI 29 alone. Docker Compose
  through it, the Docker SDKs, Testcontainers and VS Code's Dev Containers
  are untried; Testcontainers' Ryuk, which mounts the API's socket into a
  container, cannot work, as no pipe reaches a guest.
- The API serves its own pipe, `\\.\pipe\runwhp_engine`, never Docker's
  `docker_engine`, so tools that take no docker context or `DOCKER_HOST`
  do not find it. It runs until `docker-api stop`, a logoff, or the end of
  a session whose job object keeps its processes in, and nothing starts
  it at logon.
- Execs, attaches, log follows and event streams the API serves end with
  the server; the containers do not. An exec started with `-d` is not
  followed: its inspect never says it ended.
- The API has no `top`, `update` (but an empty one), image search,
  `attach/ws`, checkpoints, plugins or swarm, and no BuildKit session:
  `docker build` uses the classic builder's endpoint, warning that it is
  deprecated, and `docker buildx` cannot build through runwhp. A build's
  secrets and ssh, `--cache-from`, `--output` and a git or URL context are
  refused.
- Containers the API makes without a name go by their short ID, where
  Docker gives each a name of two words. One-character names are refused,
  as Docker 29 refuses them.
- `docker stop` and `docker wait --condition` on a created container
  answer as Docker's do, but the container's VM, already booted, stays up
  until it is started or removed.
- Events about containers that other runwhps change come up to a second
  late, 250 ms while a client follows them, and a container made and
  removed between two looks, by runwhp's own commands, shows no events.
- The API's `system df` gives Docker 29's usage objects and the older
  lists, but not the kernels runwhp's own `system df` counts. Its stats
  give a container's memory used and cached, not the guest's other cgroup
  figures, nor a pids limit.
- The API answers every version from 1.40 to 1.56 with 1.56's shapes; a
  client of an older version may find fields it does not know.
- A healthcheck runs through `exec`'s server, so it needs vsock in the
  guest's kernel, as `exec` does. Each check's result is written to the
  container's state file.
- The network device has one queue pair and no segmentation offload, which
  the link's 65520-byte MTU makes up for in part: 200 MB crosses in about
  0.4 s out of the container and 0.6-0.8 s into it on the test machine.
- runwhp's own prunes take no `--filter`, such as `until=` or `label=`;
  the API's take `label` for containers and volumes, and `dangling` for
  images, but no `until`. runwhp's `images` takes no filter but a
  repository, where the API's takes Docker's `reference`, `dangling`,
  `label`, `before` and `since`; and `list` no `before`, `since`, `publish`
  or `expose` filter. runwhp has no `login` of its own: `pull` and `push`
  use the credentials `docker login` stored, through Docker's daemon or
  runwhp's Engine API. There is no
  `--insecure-registry`: only registries on localhost, a loopback or a
  private address, or a `.local` name are spoken to over plain HTTP.
- `build` boots a guest for each `COPY` and `ADD`, and two for each
  `RUN`, about 0.1-0.2 s and 0.6-0.8 s a step on the test machine, where
  BuildKit starts a container. A process runs one guest at a time, so
  stages are built one after another, and `COPY --from` reads its files
  from a guest of their own first and spools them.
- `RUN` runs on at most about 54 layers below it, as `run` does, where
  Docker allows 127.
- `WORKDIR` makes its directory in the stage's next step that boots a
  guest, not in a layer of its own as BuildKit does, so an image has
  fewer layers than BuildKit's, and its history marks `WORKDIR` empty.
- A `RUN --mount=type=cache` volume is given to one step at a time,
  whatever `sharing` says, as `locked`. `RUN --mount=type=bind` takes the
  context only, read-only, and shows files `.dockerignore` excludes.
  There are no `type=ssh` mounts, no `bind,from=`, and no `--ssh`.
- `build` takes no git or URL context, `--cache-from`, `--cache-to`,
  `--output`, `--build-context`, `--ulimit` or `--shm-size`, and ignores
  `# syntax=`: it is its own Dockerfile frontend. Its steps boot
  runwhp's kernel, or `RUNWHP_KERNEL`'s, never with `--module`.
- A RUN step's environment has `HOSTNAME`, which init sets and BuildKit
  does not.
- `push` rebuilds and gzips every layer of the image, though the
  registry has it, and recompresses a pulled image's layers, which then
  get digests of their own: runwhp keeps no compressed layers, and
  remembers no digests a layer was pulled or pushed with. A second push
  of the same image uploads nothing.
- A prune waits for a build or a push to finish.
- `cp`, `export`, `diff` and `commit` on a stopped container boot a guest
  with its storage, about 0.1 s, and hold its lock meanwhile: a `start`
  then is refused as busy, rather than waiting, and `delete` says the
  container is running.
- A container kept from before iteration 15 has the mount points init made
  for it in its writable layer, `/etc/resolv.conf` for alpine with a
  network, say, and `diff` and `commit` show them. Its command is recorded
  whole, so its commit has it as `Cmd`, without an entrypoint.
- `diff` lists changes sorted by path, where Docker's order is a map's.
  Like Docker's overlay2 without `redirect_dir`, it reads a directory the
  container made again as changed, not as deleted and added.
- `cp` resolves a path's symlinks in the container, then opens what they
  lead to: a container changing its files meanwhile can redirect it within
  its own root, as with Docker. A tar copied in that names a path through a
  symlink pointing outside the destination is refused.
- A symlink copied out to Windows needs Developer Mode or an administrator,
  as with Docker's CLI on Windows. Files copied in from Windows are root's,
  0755 for files and directories alike, as go-archive makes them.
- A container's root can move its processes out of the container's cgroup,
  and they are then not paused.
- `commit` writes the container's configuration as go-containerregistry
  writes Docker's, with no `container_config`, no `Domainname` and no
  attach flags. A container made before iteration 19 has no labels of its
  own, and its commit keeps its image's.
- `save` reads layers in a guest it boots for the purpose, about 0.2 s
  before the first byte, and needs vsock in its kernel: runwhp's has it,
  and `save` boots runwhp's, or `RUNWHP_KERNEL`, never `--module`. Layers
  pulled before iteration 14 have no tar metadata, and `save` fetches it
  from the registry they came from, reading each such layer once more,
  through the image's own record or, for an image tagged, committed or
  loaded, any image in the store that has the layer and came from a
  registry; without one, it cannot get it.
- `save` writes about 130 MB/s, for reasons not yet known: a guest four
  times as large does no better.
- A layer whose tar names a file twice, or holds a GNU sparse file,
  cannot be rebuilt byte for byte, and `save` fails on it; none of the
  images checked has either.
- `load` refuses images other than linux/amd64, where Docker loads them;
  from a multi-platform index, it takes the linux/amd64 image alone. `import`
  takes no xz and no URL.
- An image tagged, loaded or imported has no registry digest, as with
  Docker; one loaded or imported without a name is listed as
  `<none>:<none>` and `image prune` removes it unless a container uses
  it.
- `history` gives a layer pulled before iteration 14 its ext4 image's
  size, not Docker's.
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
- `cp` of many small files out to Windows takes about 1 ms a file,
  extracting them through go-archive, as Docker's CLI does: 11.7 s for
  10,000, where GNU tar takes 2.7 s for the same tar.
- nerdctl runs Linux containers on runwhp only when told to, every time:
  `--runtime io.containerd.runwhp.v1 --platform linux/amd64 --network
  none`, as nerdctl.toml has no setting for the runtime or the platform.
- nerdctl's own networks, `nat` and CNI's, need HNS and an administrator,
  so its containers run with `--network none`, and get runwhp's network
  anyway, unless annotated `io.runwhp.network=none`. They cannot reach
  each other by name, and nerdctl on Windows has no `-P`, nor `-p` without
  a host port.
- nerdctl on Windows takes `-v` destinations as Windows paths only: the
  shim drops the drive letter, so `C:\data` and `D:\data` are both `/data`.
  It has no `--mount` or `--tmpfs`.
- `nerdctl run -u UID` without a group fails: containerd reads the image's
  `/etc/passwd` through a mount on the host, which Windows cannot make of
  a Linux layer. `-u NAME` and `-u UID:GID` work.
- `nerdctl run -it`, `exec -it` and `attach` need a console, and have not
  been tried in one; `-d -t` has. nerdctl keeps no log of a foreground run
  on Windows, and refuses `-d -i` without `-t`.
- nerdctl's `commit` needs a differ that compares snapshots, which LCOW's
  lacks; its `cp` and `export` are not built for Windows; `stats` gets no
  numbers from the shim, which runwhp's `stats` would give, and nerdctl on
  Windows shows none anyway; `top` lists the shim's pid; `compose` and
  `--restart` are untried.
- A layer pulled by nerdctl is kept by containerd, as containerd's LCOW
  differ converts it, and not by runwhp: `runwhp images` does not list it,
  `runwhp pull` does not share it, and `save` could not rebuild its tar.
  Its ext4 image is at most 128 GiB.
- runwhp's snapshotter gives views no mounts, so no other client of it can
  read an image's files through one.
- `runwhp containerd` runs in the foreground, and nothing starts it at
  logon. Containers outlive it; ended, they wait for it to start again to
  be removed.
- The shim boots runwhp's kernel, or `RUNWHP_KERNEL`'s, never with
  `--module`, so not WSL's.
- The shim's containers are records in a runwhp store of their own, which
  `runwhp list` shows with `RUNWHP_ROOT` set to it; runwhp's commands there
  are untried.
