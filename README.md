# runwhp - lightweight, rootless container runtime for Windows

`runwhp` is an experimental OCI runtime that runs Linux containers on any Windows
edition, including Home. It drives the Windows Hypervisor Platform (WHP)
directly from an ordinary user process, so it needs no Hyper-V role, no WSL, no
HCS, and no administrator rights at run time.

## Status

`runwhp` is experimental. It runs Linux containers from registry images,
interactively or not, with a network, the host's files and volumes of their
own, on a kernel of its own and on several processors. Containers on a
network of the user's reach each other by name, as on Docker's user-defined
networks, with no daemon between them. Containers are kept
after they stop, can run in the background, and can be started again,
stopped, signalled, attached to and run more commands in, and started
again by Docker's restart policies; their logs carry the time of each
line and are rotated. A container has half the
machine's memory by default, but takes only what it uses, and gives back
what it frees. Images, containers and volumes can be listed, measured and
pruned as Docker's can, and images tagged, imported, and saved to and
loaded from archives Docker takes, with the same IDs. A container's files
can be copied in and out, running or stopped, exported, listed as its
changes and committed to an image, and a container paused. Images are
built from Dockerfiles, as BuildKit builds them, each step in a guest of
its own, and pushed to registries. `runwhp compose` runs compose.yaml
projects as Docker Compose does, each container in a runwhp of its own,
with Docker's labels, healthchecks and filters. `runwhp docker-api` serves
Docker's Engine API, so that the docker CLI runs containers, builds and
pushes images, and manages networks and volumes through runwhp, as do
Docker Compose, Testcontainers, Docker's SDKs and Dev Containers. Stock
nerdctl runs Linux containers on it too, through a containerd of the
user's own and runwhp's containerd shim, unelevated.

See [status](docs/status.md) for details.

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

runwhp boots a kernel of its own, which its first `run` or `pull` fetches
from `ghcr.io/quasidock/runwhp-kernel` (about 8 MB). WSL is not needed.

## Usage

Build, from the repository root in PowerShell. This produces `out\runwhp.exe`,
`out\runwhp-compose.exe`, which runs `runwhp compose`, and the guest init
`out\runwhp-init`, which must stay next to them:

```powershell
.\build.ps1
```

Then:

```powershell
.\out\runwhp.exe run alpine:3.20 echo hello
.\out\runwhp.exe run -u nobody -e FOO=bar -w /tmp alpine:3.20 sh -c 'id; echo $FOO; pwd'
.\out\runwhp.exe run python:3.12-slim python3 -c 'print(1)'
.\out\runwhp.exe run -it alpine:3.20 sh
Get-Content data.csv | .\out\runwhp.exe run -i alpine:3.20 wc -l
.\out\runwhp.exe run alpine:3.20 sh -c 'apk add -q curl && curl -sI https://example.com'
.\out\runwhp.exe run -p 8080:80 nginx:alpine
.\out\runwhp.exe run --add-host host.docker.internal:host-gateway alpine:3.20 wget -qO- http://host.docker.internal:3000/
.\out\runwhp.exe run -v ${PWD}:/src -w /src alpine:3.20 ls -l
.\out\runwhp.exe run --mount type=bind,source=C:\data,target=/data,readonly alpine:3.20 du -sh /data
.\out\runwhp.exe run --cpus 8 -m 2g -v ${PWD}:/src -w /src golang:1.26 go build ./...
.\out\runwhp.exe run -v pgdata:/var/lib/postgresql/data -e POSTGRES_PASSWORD=pw -p 5432:5432 postgres:17
.\out\runwhp.exe run -v ${PWD}:/app -v /app/node_modules -w /app node:22 npm ci
.\out\runwhp.exe run --tmpfs /run --storage-opt size=200g debian:12 sh -c 'apt-get update && apt-get install -y build-essential'
.\out\runwhp.exe volume ls
.\out\runwhp.exe pull debian:12
```

Containers that outlive the command, in the background:

```powershell
.\out\runwhp.exe run -d --name web --restart always -p 8080:80 nginx:alpine
.\out\runwhp.exe logs -f web
.\out\runwhp.exe logs -t --since 10m -n 20 web
.\out\runwhp.exe exec -it web sh
.\out\runwhp.exe stop web
.\out\runwhp.exe start web
.\out\runwhp.exe list
.\out\runwhp.exe delete -f web
.\out\runwhp.exe run --rm alpine:3.20 echo gone afterwards
```

Keeping the store small:

```powershell
.\out\runwhp.exe images
.\out\runwhp.exe rmi python:3.12-slim
.\out\runwhp.exe system df -v
.\out\runwhp.exe container prune
.\out\runwhp.exe system prune -a
```

Images in and out of the store:

```powershell
.\out\runwhp.exe save -o python.tar python:3.12-slim
.\out\runwhp.exe load -i python.tar
.\out\runwhp.exe tag python:3.12-slim mine/python:dev
.\out\runwhp.exe history python:3.12-slim
.\out\runwhp.exe import -c 'ENV GREETING=hi' rootfs.tar mine/rootfs:1
.\out\runwhp.exe build -t registry.example.com/mine/app:1 .
.\out\runwhp.exe push registry.example.com/mine/app:1
```

A project of several containers, from a compose.yaml:

```powershell
.\out\runwhp.exe compose up -d --wait
.\out\runwhp.exe compose ps
.\out\runwhp.exe compose logs -f web
.\out\runwhp.exe compose exec db psql -U postgres
.\out\runwhp.exe compose down -v
```

The docker CLI, through runwhp's Engine API; see [Docker Engine API](#docker-engine-api):

```powershell
.\out\runwhp.exe docker-api start
docker --context runwhp run --rm -it alpine:3.20 sh
```

A container's files:

```powershell
.\out\runwhp.exe cp web:/etc/nginx/nginx.conf .
.\out\runwhp.exe cp .\site web:/usr/share/nginx/html
.\out\runwhp.exe diff web
.\out\runwhp.exe commit -m 'with the site' web mine/web:1
.\out\runwhp.exe export -o web.tar web
.\out\runwhp.exe pause web
.\out\runwhp.exe unpause web
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-d`, `--detach` | off | Run the container in the background and print its ID; see [Containers](#containers) |
| `--rm` | off | Remove the container when it stops. Without it, the container is kept, with its writable layer, until `runwhp delete` |
| `--restart` | `no` | Start the container again when its process ends: `on-failure[:N]` when it exits non-zero, at most N times, `always`, or `unless-stopped`, the same here. Not with `--rm`; see **Restarting** under [Containers](#containers) |
| `--log-driver` | `local` | `local` keeps the container's output in its log; `none` keeps none. `json-file` is `local` under Docker's name, which the Engine API reports |
| `--log-opt` | `max-size=20m`, `max-file=5` | `max-size=SIZE` rotates the log as a file would grow past SIZE, `-1` never; `max-file=N` keeps N files, the one being written included; repeatable. See **Output** under [Containers](#containers) |
| `--name` | none | Name the container, as well as its 64-digit ID. Names are case-insensitive |
| `-l`, `--label` | the image's | `KEY=VALUE`, or `KEY` for an empty value: a label on the container, over the image's; repeatable. See **Labels and filters** under [Containers](#containers) |
| `--label-file` | | A file of `KEY=VALUE` lines, read as Docker's client reads one; repeatable |
| `--health-cmd` | the image's `HEALTHCHECK` | A command `/bin/sh` runs in the container to check its health; see **Healthchecks** under [Containers](#containers) |
| `--health-interval`, `--health-timeout`, `--health-start-period`, `--health-start-interval`, `--health-retries` | `30s`, `30s`, `0s`, `5s`, `3` | When and how long checks run, and how many failures in a row make the container unhealthy; durations such as `500ms` or `1m` |
| `--no-healthcheck` | off | Run no healthcheck, though the image has one |
| `--stop-signal` | the image's, else SIGTERM | The signal `stop` sends first |
| `--stop-timeout` | `10` | Seconds `stop` and `restart` wait before SIGKILL, unless `-t` says |
| `-i`, `--interactive` | off | Pass stdin to the container. Without it, the container reads `/dev/null`. In the background, without a file or pipe for stdin, the container's input is open to `attach` |
| `-t`, `--tty` | off | Run the container on a pseudo-terminal of the console's size, with stderr merged into stdout. With `-i`, stdin must be a console, and keys go to the container raw. `-it` works as a shorthand |
| `--entrypoint` | image's | Replace the entrypoint; `""` removes it. Also drops the image's command, as in Docker |
| `-e`, `--env` | | `KEY=VALUE`, or `KEY` to copy it from the host; repeatable |
| `-w`, `--workdir` | image's, else `/` | Working directory; created if missing |
| `-u`, `--user` | image's, else root | `name`, `uid`, `name:group` or `uid:gid`, resolved against the image's `/etc/passwd` and `/etc/group` |
| `-m`, `--memory` | half this machine's memory | Guest RAM, from `128m` up to this machine's memory. A guest takes only the memory it touches; see [Memory](#memory) |
| `--cpus` | the host's, up to 4 | vCPUs, a whole number up to the host's logical processors, at most 64. Unlike Docker's `--cpus`, it counts processors rather than setting a quota, so `1.5` is refused |
| `--hostname` | the ID's first 12 digits | Hostname, also in `/etc/hostname` and `/etc/hosts` |
| `--network`, `--net` | `user` | `user` gives the container a network of its own through runwhp; `none` leaves it only loopback; a network's name puts it on that network, with the containers there, as `name=NAME,alias=ALIAS,ip=ADDRESS` does with an alias and an address; repeatable for networks of the user's. See **User-defined networks** under [Networking](#networking) |
| `--network-alias` | | Another name for the container on its first network; repeatable |
| `--ip` | | The container's address on its first network |
| `-p`, `--publish` | | `[IP:][HOSTPORT:]PORT[/tcp]`: listen on the host and forward to the container's TCP port; repeatable. IP defaults to `127.0.0.1`; without `HOSTPORT`, or with `0`, any free port is taken, which `port` shows |
| `-P`, `--publish-all` | | Publish every exposed TCP port, the image's `EXPOSE` and `--expose`'s, on a free host port of `127.0.0.1` |
| `--expose` | | `PORT[-PORT][/PROTO]`: a port the container listens on, published only by `-P`; repeatable |
| `--add-host` | | `NAME:IP` or `NAME=IP`, added to `/etc/hosts`; repeatable. `NAME:host-gateway` also lets the container reach the host's `127.0.0.1` at that name |
| `-v`, `--volume` | | `HOSTPATH:PATH[:ro][,delegated]`: bind a host directory or file at `PATH`; a missing `HOSTPATH` is made as a directory, and `delegated` lets the guest cache writes; see [Delegated mounts](#delegated-mounts). `NAME:PATH[:ro][,nocopy]`: mount the named volume `NAME`, made if missing. `PATH` alone: an anonymous volume, removed with the container. Repeatable. See [Host files](#host-files) and [Volumes](#volumes) |
| `--mount` | | Docker's long form of `-v` and `--tmpfs`: `type=bind,source=HOSTPATH,target=PATH[,readonly][,consistency=delegated]`, where `HOSTPATH` must exist; `type=volume[,source=NAME],target=PATH[,readonly][,volume-nocopy][,volume-opt=size=SIZE]`; `type=tmpfs,target=PATH[,tmpfs-size=SIZE][,tmpfs-mode=MODE]` |
| `--tmpfs` | | `PATH[:OPTIONS]`: mount a tmpfs, `noexec`, `nosuid` and `nodev` unless the options say `exec`, `suid` or `dev`; repeatable. See [Volumes](#volumes) |
| `--storage-opt` | `size=64g` | `size=SIZE`: how large the container's writable layer may grow, `1g` to `16t`. See [Storage](#storage) |
| `--pull` | `missing` | When to pull the image: `missing`, `always` or `never`. `never` also keeps runwhp from fetching its kernel |
| `--kernel` | runwhp's own | bzImage to boot instead; see [Kernel](#kernel) |
| `--debug` | off | Kernel console, init's steps and VMM statistics on stderr |

Exit codes follow Docker: the container's own code, 128+N if a signal N killed
it, 125 if runwhp failed, 126 if the command cannot be run, 127 if it does not
exist, and 130 if runwhp stopped the VM after Ctrl+C. When runwhp fails after
the guest booted, it prints the last kernel console lines.

Ctrl+C and Ctrl+Break send SIGINT to the container's process group, as a
terminal would. A container that does not handle it exits with 130; one that
traps it decides for itself. A second Ctrl+C stops the VM at once. With
`-it`, Ctrl+C is a key like any other, and the container's terminal turns it
into SIGINT. Closing the console window, logging off or shutting down sends
SIGTERM to the container's main process and stops the VM 10 seconds later.
While the guest is still booting, Ctrl+C stops the VM at once.

With `-i` and a console but no `-t`, the console edits each line before
sending it, ends lines with `\n`, and Ctrl+Z at the start of a line ends the
input, as usual on Windows.

Images, layers and runwhp's kernel are cached in `%LOCALAPPDATA%\runwhp`, or
in `RUNWHP_ROOT` when it is set. `runwhp pull` fetches the kernel as well as
the image, so a machine prepared with it can run that image offline.
Registry credentials come from Docker's configuration and credential helpers.
Only `linux/amd64` images run. `runwhp images` lists what is cached,
and `rmi`, the prunes and `system df` keep it in check; see
[Cleaning up](#cleaning-up).

In Git Bash, set `MSYS_NO_PATHCONV=1`, or MSYS rewrites container paths such
as `/tmp` into Windows paths before runwhp sees them; then
`-v "$(pwd -W):/src"` binds the current directory. Under mintty, the
default Git Bash window, stdin is a pipe rather than a console, so `-it`
fails with "the input device is not a TTY"; prefix the command with
`winpty`. Pipes from MSYS programs into native ones are slow (about 8 MB/s
on the test machine), and stdin through `-i` is no exception.

### Containers

Every `runwhp run` makes a container: a record in the store's `containers`
directory holding its configuration, resolved when it was made, its state,
its name if it has one, and its writable layer. A container is named by
its ID, its name, or any unique prefix of its ID.

| Command | |
| --- | --- |
| `create [run flags] IMAGE [COMMAND...]` | Make a container and boot it, but hold its process until `start`; prints its ID |
| `start [-a [-i]] CONTAINER...` | Start a created container's process, or a stopped container again, in a new VM on the writable layer it left. A running one is left as it is. `-a` attaches to it from its first byte of output, `-i` to its input too; see **Attaching** below |
| `attach [--no-stdin] [--sig-proxy=false] [--detach-keys KEYS] CONTAINER` | Show a running container's output as it comes, and pass it input if it takes it; exit with its exit code, or 0 on detaching |
| `wait CONTAINER...` | Wait for containers to stop, and print their exit codes |
| `stop [-t SECONDS] CONTAINER...` | Send the container's stop signal, SIGTERM unless `--stop-signal` or the image names another, then SIGKILL after the timeout: its `--stop-timeout`, or 10 s |
| `restart [-t SECONDS] CONTAINER...` | `stop`, then `start` |
| `pause CONTAINER...`, `unpause CONTAINER...` | Freeze every process in a running container, and thaw them; see **Pausing** below |
| `kill [-a] CONTAINER [SIGNAL]` | Send a signal, by name or number, to the container's process, or with `-a` to every process in it. The default is SIGTERM, as with runc; Docker's `kill` sends SIGKILL. A created container ends without starting |
| `exec [-i] [-t] [-d] [-e] [-w] [-u] CONTAINER COMMAND...` | Run another command in a running container |
| `logs [-f] [-t] [-n N] [--since TIME] [--until TIME] CONTAINER` | Print a container's output as its log holds it: `-t` with the time each line was written, `-n` the last N lines, `--since` and `--until` the lines written in a window, given as a date and time, Unix seconds, or a duration before now such as `10m` |
| `list [-q] [-f FILTER]... [--format table\|json]` | List containers, newest first, or those that match Docker's filters; see **Labels and filters** below |
| `state CONTAINER` | Describe a container as JSON: runc's fields (`ociVersion`, `id`, `status`, `pid`, `bundle`), then runwhp's own |
| `stats [--no-stream] [--format table\|json] [CONTAINER...]` | Show what running containers use, every second until Ctrl+C, or once with `--no-stream`; see [Memory](#memory) |
| `top CONTAINER [-ef\|aux]` | List a running or paused container's processes, as `ps -ef` or `ps aux` shows them, with the guest's PIDs: every process in the guest but runwhp's init, from the `/proc` init holds, so an image needs no `ps` |
| `port CONTAINER [PORT]` | List a running container's published ports as they were bound, or where one is |
| `delete [-f] CONTAINER...` | Remove containers, with their writable layers, logs and anonymous volumes. A running one needs `-f`, which kills it first |
| `rename CONTAINER NAME` | Rename a container |

A container made without `--name` gets one, an adjective and a noun, as
Docker's do: `brisk_heron`.

States are runc's: `creating`, `created`, `running` and `stopped`, and
runwhp's own `restarting`, for a container waiting to be started again by
its restart policy. Unlike with runc, a stopped container can be started
again. A paused container is `running`, and `state` says `paused`, as
`docker inspect` does.

**Pausing.** The container's process, and those `exec` starts, run in a
cgroup of their own in the guest, which `pause` freezes through
`cgroup.freeze`, and `unpause` thaws, each in a few tens of ms. init, which
stays outside it, goes on serving the host: the clock, `stats`, `logs`,
`cp`, `diff`, `export` and `commit` work on a paused container, and its
memory is reclaimed as an idle one's is. `exec`, `attach`, and `delete`
without `-f` are refused, with Docker's words, and `list` shows
`Up 3 minutes (Paused)`. `kill` thaws the container once its signal is
sent, so that the signal is taken, as `docker kill` does, and so `stop`,
`restart` and `delete -f` work as on a running one. A process that leaves
the cgroup, which a container's root can do, is not paused.

**Who runs a container.** A container has a VM only while a runwhp process
runs it, holding the record's `lock` file exclusively; whether that file is
held is how other runwhps tell whether it runs. A foreground `run` runs its
own container, which ends with its terminal, as before. `create`, `run -d`
and `start` hand the container to a runwhp of its own, started detached
from the console, in a process group of its own and, where the caller's
job object allows, out of that job, so closing the terminal or the IDE
leaves it running. A session whose job does not let its processes leave
it, as some ssh servers and CI runners make, gets a warning, and its
containers end with it. No container outlives the logon session: logging
off ends them as killing their runwhp would. A container whose runwhp died
is recorded as stopped, with exit code 255, by whoever looks next; its
writable layer replays its journal when it starts again, keeping what the
container had synced. The runwhp running a container, in the foreground or
not, serves a named pipe, for this user only, on which `start`, `stop`,
`kill` and `exec` reach it.

**Output.** A container in the background writes its output to the files
the command that started it had for stdout and stderr, if they are files
(`runwhp run -d ... > out.log`), and otherwise to its log, which `logs`
prints, stdout to stdout and stderr to stderr. Pipes are not handed over:
capturing one, as `id=$(runwhp run -d ...)` does, would then wait for the
container to end. With `-i`, its stdin is the caller's if that is a file
or a pipe, and otherwise open to `attach`. With `-t`, its terminal is 24 by
80 until someone attaches. A container run in the foreground writes its
output to its log as well as to the terminal, unless it is run with
`--rm`, so `logs` works on every kept container; `--log-driver none` keeps
no log. runwhp's own messages about a container in the background go to
`runwhp.log` beside the log, kept to two files of 1 MiB.

**Logs.** A log records when each piece of output was written, by the
host's clock, and rotates as Docker's `local` driver does: once its file
would grow past 20 MB it is renamed `log.1`, the files before it moving up
one, and the fifth is dropped, so a container's log takes at most 100 MB;
`--log-opt max-size` and `max-file` change that. `logs -t` prints the time
each line was written, which is when its first byte was. Lines of stdout
and stderr are in the order runwhp received them, which, for writes
close together, need not be the order the container made them in.
`logs -f` follows the log from file to file as it rotates, and loses
output only if it falls more than the files kept behind. Logs written by
runwhp before iteration 13 have no times, and still print.

**What start uses.** A container's configuration is resolved when it is
made: the image's layers, the command and environment, `-e KEY` values,
bind sources as absolute paths, volumes, ports, the hostname, and a kernel
named with `--kernel` or `RUNWHP_KERNEL`. `start` uses that, not the
caller's directory or environment, and never pulls: a layer missing from
the store fails it. It binds the published ports again, failing if one is
taken, and makes missing bind directories again, as `-v` did. runwhp's own
kernel and `runwhp-init` are those of the runwhp that starts it. A created
container holds its RAM, volumes and ports until it is started or deleted.

**Attaching.** `runwhp attach` shows a running container's output from
then on, wherever else it goes: its log, its files, or the console of the
runwhp that runs it in the foreground. It exits with the container's exit
code once it stops. A container in the background run with `-i` and no
file or pipe for input takes the attached console's input: keys as they
are pressed with `-t`, lines without. `run -d -it` keeps a shell for
whoever attaches next, and `create -i` takes input once, so the first
attached input to end, as `start -ai` with a file does, ends the
container's. Ctrl+P Ctrl+Q, or `--detach-keys`, detach and leave it
running; without `-t`, Ctrl+C sends it SIGINT. Several runwhps can attach
at once, and the terminal takes the size of the last to change. A runwhp
attached to a container run in the foreground gets its output only. The
container's output never waits for an attached runwhp: one that falls
4 MiB behind is cut off. `start -a` boots a stopped container, attaches,
and only then starts its process, so its first output is not missed; it
takes one container, and its output also goes to the container's log. `wait` prints how containers ended, once they have.

```powershell
.\out\runwhp.exe run -dit --name dev alpine:3.20 sh
.\out\runwhp.exe attach dev       # Ctrl+P Ctrl+Q to leave it running
.\out\runwhp.exe wait dev
```

**exec.** `runwhp exec` runs a command in a running container, with the
container's environment, working directory and user unless `-e`, `-w` or
`-u` say otherwise; the working directory must exist. `-i`, `-t` and Ctrl+C
work as with `run`, and a second Ctrl+C kills the command; `-d` starts it
and returns. The command reaches a server in `runwhp-init` over a virtio
socket device, which nothing inside the container can connect to. When the
container's process exits, everything exec started is killed, and `exec`
exits with 137.

**Restarting.** `--restart` starts a container again when its process
ends, in a new VM on the writable layer it left: `on-failure` when it
exits other than 0, at most N times with `on-failure:N`, and `always` or
`unless-stopped` whatever it exits with; without a daemon that starts
containers as it starts, the last two do the same. The runwhp running the
container restarts it, after Docker's delays: 100 ms, doubling each time
up to a minute, and back to 100 ms once a run has lasted 10 seconds.
Meanwhile the container is `restarting`, which `list` shows as
`Restarting (CODE) ... ago`, with how many times it has been started again
in `state`'s `restartCount`. A container is not started again when:

- `stop`, `kill`, with any signal, or `delete -f` ended it, as with Docker;
- its first run never started its process: a port taken, say, fails
  `run`. Later boots that fail, the same port taken, are retried like a
  run that ended;
- the runwhp running it ends, killed or with the logon session, which also
  ends the container. Nothing starts containers again at logon.

While it waits, `start` leaves it be, `wait` waits for its next run to end,
`stop`, `kill` and `delete -f` end it for good, and `exec`, `attach` and
`delete` without `-f` refuse it. `restart` stops it and starts it again,
counting restarts from 0. A container run in the foreground with
`--restart` is handed over, as Docker's daemon keeps it: `run` exits with
the first run's exit code, and a runwhp in the background takes the
container over, with its lock, and starts it again, its output going to
the log, its `-i` input open to `attach`. A foreground run interrupted
with Ctrl+C, or ended with its console, is not handed over. Input from a
file or pipe goes to the first run only.

**Healthchecks.** The runwhp running a container checks its health as
Docker's daemon does: the image's `HEALTHCHECK`, or `--health-cmd` and the
other `--health-*` flags over it, run in the container through the same
server `exec` uses. The first check comes an interval after the process
starts; while the container is `starting` within its start period, checks
come every start interval, and their failures do not count; after that,
every interval. A check that exits 0 makes the container `healthy`; as many
failures in a row as the retries make it `unhealthy`; a check that outruns
its timeout is killed and fails, with Docker's "Health check exceeded
timeout". `state` shows the check, and its health with the last five
results, each with up to 4096 bytes of output; `list` shows
`Up 3 minutes (healthy)`, `(unhealthy)` or `(health: starting)`, and
filters with `-f health=`. A paused container is not checked. A container
started again starts again as `starting`, keeping its last results, and a
stopped one keeps its last health. Being unhealthy restarts nothing, as with
Docker. A check costs the container's runwhp about 18 ms of CPU on the test
machine, guest included. Like `exec`, it needs vsock in the guest's kernel,
which runwhp's has.

```powershell
.\out\runwhp.exe run -d --name db --health-cmd 'pg_isready -U postgres' --health-interval 2s -e POSTGRES_PASSWORD=pw postgres:17
.\out\runwhp.exe list -f health=healthy
```

**Labels and filters.** A container has its image's labels, with
`--label-file`'s and `--label`'s over them, as Docker gives it; `state`
shows them, and `commit` writes them into the image. `list -f` takes
Docker's container filters: `id` (a prefix), `name` (a regular
expression), `label` (`KEY` or `KEY=VALUE`), `status` (`created`,
`running`, `paused`, `restarting`, `exited`), `health` (`starting`,
`healthy`, `unhealthy`, `none`), `network`, `ancestor` (the image, by name
or ID), `exited` (an exit code) and `volume`. A key given more than once
matches any of its values, except `label`, which must match every one, and
every key given must match.

```powershell
.\out\runwhp.exe run -d --label tier=back --name api mine/api:1
.\out\runwhp.exe list -f label=tier=back -f status=running
```

**Removing.** `--rm` removes a container when it stops, however it stops,
and such a container cannot be started again, nor given `--restart`. Other containers stay until
`delete`; in Git Bash, `runwhp delete $(runwhp list -q)` removes every one
that is not running.

### Networking

Unless run with `--network none`, the container has one interface, `eth0`,
on a private network that exists only inside runwhp:

| Address | What it is |
| --- | --- |
| `10.0.2.15/24` | The container |
| `10.0.2.2` | The gateway, which is runwhp itself. With `--add-host NAME:host-gateway`, connections to it reach the host's `127.0.0.1`; without it they are refused |
| `10.0.2.3` | DNS, answered by runwhp |

runwhp ends the container's TCP connections and UDP flows itself and opens
the same connections from the host, as any program on the host would. The
container reaches whatever the host reaches, LAN and VPN included, and nothing
in the host's network configuration changes. Names resolve through Windows's
own resolver, so the hosts file, VPN DNS and per-domain DNS servers apply as
they do on the host. The network is IPv4 only: AAAA queries get an empty
answer, which sends programs straight to IPv4.

The host's loopback is closed to the container unless `host-gateway` opens
it. A service that listens on all of the host's interfaces can still be
reached at the host's LAN address, as with Docker.

`-p 8080:80` listens on the host's `127.0.0.1:8080` and forwards each
connection to port 80 in the container, which sees it come from `10.0.2.2`.
`-p 0.0.0.0:8080:80` listens on every interface; Windows Firewall then asks,
once, whether to allow it, and allowing it on public networks needs an
administrator. A connection that arrives before anything in the container
listens on the port is accepted and then closed, as with Docker.

`-p 80`, without a host port, takes any free one, as Docker's random host
ports do; `-P` publishes every port the image exposes, or `--expose`
names, so. Each boot binds them anew, so a container started again may
get other ports; `runwhp port` and the Engine API's inspect give those
bound now.

`ping` gets answers only from the gateway: runwhp carries TCP and UDP, not
ICMP. Containers on a network of their own ping each other.

#### User-defined networks

The default network keeps every container alone, at the same address.
Containers on a network made with `runwhp network create` reach each
other, by name, alias or address, as on a Docker network:

```powershell
.\out\runwhp.exe network create app
.\out\runwhp.exe run -d --name db --network app -e POSTGRES_PASSWORD=pw postgres:17
.\out\runwhp.exe run -d --name web --network app --network-alias api -p 8080:8080 mine/web:1
.\out\runwhp.exe run --rm --network app alpine:3.20 sh -c 'nc -z db 5432 && wget -qO- http://api:8080/'
```

| Command | |
| --- | --- |
| `network create [--label K=V]... NAME` | Make a network, and print its ID. Its subnet is runwhp's to choose |
| `network ls [-q] [--no-trunc] [-f FILTER]...` | List networks: the user's, and the built-in `user` and `none`; or those that match `name`, `id`, `label`, `driver` or `type` (`custom` or `builtin`) |
| `network inspect NETWORK...` | Describe networks as JSON, in the shape of `docker network inspect`, with the containers running on them |
| `network rm NETWORK...` | Remove networks no container is running on |
| `network prune [-f]` | Remove every network no container is running on; `system prune` does too |
| `network connect [--alias NAME]... [--ip ADDRESS] NETWORK CONTAINER` | Put a container that is not running on a network |
| `network disconnect NETWORK CONTAINER` | Take a container that is not running off a network |

`run` and `create` take `--network NAME` more than once, Docker's long
form `--network name=NAME,alias=ALIAS,ip=ADDRESS`, and `--network-alias`
and `--ip`, which go with the first network named. `bridge` and `default`
are other names for `user`; a container is on `user` or `none`, or on
networks of the user's, not both.

**Addresses.** A network gets a subnet from Docker's pools as it is made:
`172.18.0.0/16`, then `172.19.0.0/16` and on to `172.31.0.0/16`, then
`192.168.0.0/16` in `/20`s, past any the host's interfaces or other
networks are on. Its gateway is the subnet's first address. A container
leases the lowest free address after it as its VM boots, and gives it
back as it stops, as Docker hands addresses out, unless `--ip` asked for
one; its MAC address is `02:42` and its address, as Docker's were. A
container on more than one network has an interface on each, `eth0` for
the network whose name sorts first, with the default route, as Docker
routes through that network, and the others after it.

**Names.** Each container's runwhp answers DNS at the gateway: the
names, aliases, 12-digit IDs and hostnames of the containers running on
its networks, in any case, alone or followed by a dot and the network's
name, then everything else through Windows's resolver, as on the default
network. Addresses resolve back to `NAME.NETWORK`. `/etc/resolv.conf`
names the gateway with `options ndots:0`, and `/etc/hosts` gives the
container's hostname its addresses, as Docker's do. The host is
`host-gateway`, `10.0.2.2`, on every network.

**How frames move.** A network has no switch. Each container's runwhp
routes its guest's frames for the others on the network straight to their
runwhps, over a named pipe each boot listens on, for this user only, and
answers its guest's ARP requests for them itself; broadcasts and
multicasts go to every container on the network. Frames for the gateway,
DNS and everything outside go to the container's own network stack, as on
the default network, and so leave through the host's sockets. A container
finds the others through the store: its lease is a file under the
network, which its runwhp holds open as long as the VM runs, so a runwhp
that is killed leaves an address that the next container to boot takes.
TCP, UDP, ICMP and any other IPv4 protocol pass between containers, from
their own addresses. 200 MB crosses in about 0.45 s, and a ping takes
about 1 ms, the time to wake two guests rather than one; see
[Iteration 18 results](docs/status.md#iteration-18-results).

**Connecting.** A VM cannot take a new network device while it runs, so
`network connect` and `disconnect` refuse a running container. A stopped
one is changed for its next start; a created one, whose VM already waits
to start, is booted again at once, in about 0.4 s; one waiting to start
again under its restart policy starts so. Off its last network, a
container has loopback alone, as with Docker. A container keeps the names
of its networks: one that was removed fails its start, as with Docker.

### Host files

`-v HOSTPATH:PATH` makes a host directory or file appear at `PATH` in the
container. Changes either side makes show on the other.

- `HOSTPATH` is an absolute Windows path (`C:\src`, `C:/src`,
  `\\server\share\dir`), a path starting with a dot (`.`, `.\src`), which is
  relative to the current directory, or Git Bash's `/c/src`. A name without
  a path, such as `data`, names a volume; see [Volumes](#volumes).
- `-v` makes a missing `HOSTPATH` as a directory, as Docker does; `--mount`
  requires it to exist.
- `PATH` must be absolute, and cannot be `/` or lie under `/proc` or `/sys`.
  A missing `PATH` is made in the container; if it leads through a link in
  the image, such as alpine's `/var/run`, the mount goes where the link
  leads. Mounts inside mounts work whatever order they are given in. A file
  bound over one runwhp writes, such as `/etc/hosts`, wins.
- `:ro` makes the mount read-only, and `:rw`, the default, writable.
  `:delegated`, or `consistency=delegated` with `--mount`, lets the guest
  cache writes; see [Delegated mounts](#delegated-mounts). `z`, `Z`,
  `cached` and `consistent` are accepted and ignored. Options combine with
  commas, as in `:ro,z`.

**Owners and modes.** Windows has no Unix owners or modes. A file that has
never had them shows as belonging to the container's user, the one `-u` or
the image's `USER` names, with mode 755. A file Windows marks read-only
shows without write bits. Everything the container sets is kept: files it
makes belong to the process that made them, and `chmod`, `chown`, symlinks,
FIFOs, sockets and device nodes survive the run. runwhp stores them as WSL
does with its `metadata` mount option, in the extended attributes `$LXUID`,
`$LXGID`, `$LXMOD` and `$LXDEV` and in WSL's reparse points, so WSL shows
the same owners, modes and links. Taking away the owner's write bit also
marks the file read-only for Windows. On a volume without extended
attributes, such as FAT or some network shares, `chmod` and `chown` succeed
and change nothing.

**Links.** A symlink made in the container is a WSL symlink, which Windows
programs do not follow. Windows symlinks and junctions show as symlinks: one
pointing into the same mount shows its container path, and any other its
Windows path, which leads nowhere in the container. runwhp never follows a
link on the host, so no link reaches outside what was mounted. Hard links
work within a mount.

**Names.** Lookups are case-insensitive, as on the host, unless the directory
has been made case-sensitive with `fsutil file setCaseSensitiveInfo`.
Listings show names as stored. Characters Windows forbids in names
(`\ : * ? " < > |`) are stored as WSL stores them, as U+F000 plus the
character.

**Caching.** The container caches names and attributes for a second, and file
contents until a file's size or modification time changes, so a change made
on the host shows within about a second. Host changes raise no inotify
events in the container, so file watchers there must poll.

Removing a file that is open works as on Linux: the name goes at once and
the open file stays readable. A rename replaces the target, even a read-only
one. File locks are kept in the guest and are invisible to host programs.

#### Delegated mounts

Each `write()` to a bind mount reaches the host as it is made, which costs
a program writing in small pieces dearly: about 100 µs a write on the test
machine. `-v C:\out:/out:delegated` lets the guest gather writes in its page
cache, as it does for its own disks, and send them in large pieces: when a
file is closed or synced, when the guest's kernel writes back dirty pages,
which it does within about 30 seconds, and when the container stops.
Writing 200 MB in 820-byte pieces took 1.2 s on a delegated mount, against
22-28 s on an ordinary one, but large writes followed by an `fsync` take
about twice as long, since they go through the page cache first. The
guest then trusts what it has cached:

- A host program sees the container's writes to a file only once the
  container closes or syncs it.
- A container stopped hard, by a second Ctrl+C or a killed runwhp, loses
  what it had not written back.
- The guest keeps its own idea of the size and times of files it has
  cached, so a host program's change to a file the container has used may
  go unseen there, and the container's later writes can overwrite it.

Delegate trees the container owns while it runs, such as build output,
caches and installed dependencies, not files edited on the host at the same
time. A read-only mount is never delegated. Delegated mounts are served by a
second virtio-fs device, which takes one more interrupt line; see
[Known gaps](docs/gaps.md).

### Memory

A container's guest has the memory `-m` gives it, half this machine's by
default, but takes from Windows only what it touches: runwhp commits and
maps guest RAM 2 MiB at a time, the first time the guest or a device uses
it, rather than all of it when the VM starts. Task Manager's commit size
for the container's `runwhp.exe` shows what it has taken.

- **A floor per GiB.** The guest's kernel describes every page of its
  RAM when it boots, in 1/64 of the RAM, so a guest always takes that
  much: 128 MiB for an 8 GiB guest. Describing the pages costs about 8 ms
  of start-up per GiB. A guest with the default memory, half of this
  15 GiB machine's, runs `true` in about 225 ms, holding about 206 MiB;
  one with `-m 512m` in about 165 ms, holding 62 MiB. Iteration 8, which
  committed all of a 512 MiB guest up front, took about 265 ms.
- **Above 4 GiB.** Guest RAM runs up to the device windows at 3.25 GiB
  and goes on from 4 GiB, as on a PC.
- **What the guest frees goes back.** The guest's kernel reports RAM it
  has had free for two seconds to a virtio memory balloon, in blocks of
  2 MiB, and runwhp gives it back to Windows: 3 GiB a guest frees is back
  within about 15 s. The guest takes it again by touching it. A block is
  reported only once all of it is free, and pages the guest keeps, its own
  kernel's especially, end up scattered over the RAM it has used, so some
  of what it frees stays with it.
- **What a container uses.** `runwhp stats` shows, for each running
  container, its processors' time as a share of one (`200%` is two vCPUs
  kept busy), what its guest uses of its memory (all but what Linux counts
  as available), what its runwhp holds of this machine's (`HOST MEM`, the
  process's commit, guest RAM included), the network and disk bytes it has
  moved, and its processes' threads. The guest's numbers come from
  runwhp's init, the rest from runwhp itself.
- **An idle guest gives back its page cache.** Files a container reads or
  writes stay cached in its guest's RAM, as on any Linux. Once the guest
  has kept its processors idle, below 5% of one, for a minute, runwhp's
  init reclaims a tenth of that cache every 5 s, down to 2% of the guest's
  RAM, through the memory cgroup's `memory.reclaim`, and the balloon gives
  what that frees back to Windows: 1.5 GiB of cache goes in about three
  minutes. Reading the files again costs little, since the layers, the
  writable layer and volumes are files the host caches too. Becoming busy
  again stops it. `RUNWHP_RECLAIM_IDLE` sets the idle time in seconds; `0`
  turns it off.

### Storage

Whatever the container writes outside its mounts goes to its writable layer:
a sparse file holding an ext4 file system, `rw.ext4` in the container's
directory, made when the container first boots and kept until it is
deleted. With `--rm` it is a scratch disk instead, in the store's `scratch`
directory, made for the run and deleted when it ends; Windows deletes it
even when runwhp is killed, and the next run removes one the machine
stopped under.

- The writable layer is limited by its disk's size, 64 GiB unless
  `--storage-opt size=` says otherwise, and by the host's free space, not by
  `--memory`. A full host disk shows in the container as I/O errors.
- The file takes only what is written. Space the container frees goes back
  to the host during the run: the guest discards the blocks, and runwhp
  punches them out of the file. NTFS does this in 64 KiB units, ReFS in
  clusters.
- A kept writable layer has a journal, and `fsync` in it waits for the
  host's disk, as it would under Docker. Before the VM resets, init freezes
  it, as it does each volume, which leaves it clean. A scratch disk has no
  journal, and `fsync` there returns without waiting for the host's disk,
  since nothing there outlives the run. Either is formatted in a few ms
  while the kernel loads; deleting a scratch disk takes about 4 ms at exit.
- runwhp binds `/etc/hostname`, `/etc/hosts` and `/etc/resolv.conf` over
  the image's, as Docker does, so they are no part of a container's
  changes. They can be edited in place, but not replaced: `sed -i` on them
  fails. A mount of the user's own at one of those paths wins.
- The mount points init makes for itself, `/proc`, `/sys`, `/dev` and those
  three files, go in a layer of their own when the image lacks them, as in
  Docker's init layer: a tmpfs between the image's layers and the writable
  one, so they are not the container's changes either. Its directories
  take the image's mode, owner and times. A container kept from before
  iteration 15 has them in its writable layer already, and `diff` shows
  them. Mount points of the container's own mounts and its working
  directory go in the writable layer, as with Docker.
- On a store whose volume has no sparse files, such as FAT or exFAT, runwhp
  says so and keeps the writable layer in guest memory, so a stopped
  container loses its changes.

### Container files

`cp`, `export`, `diff` and `commit` reach a container's files as Docker's
do, whether it is running, created, paused or stopped.

- **How they reach them.** init serves the container's files on a vsock
  port of its own, from the moment its root is ready, so a created
  container is served too; the runwhp running the container passes the
  connection on. A stopped container's files are served by a guest booted
  for the purpose with its storage: its layers, its writable layer, its
  volumes and its bind mounts, with the container's own kernel, modules and
  init, but no network and no process, in 256 MiB and two vCPUs. It is
  ready about 0.1 s after the command starts, and on its way out freezes
  what it wrote, as a container's guest does. Meanwhile the command holds
  the container's lock: a `start` is refused, saying the container is
  busy, and another `cp` waits its turn.
- **cp** follows `docker cp`: its client side is the Docker CLI's, through
  github.com/moby/go-archive, and init's side follows Docker's daemon. A
  path in the container resolves as the container sees it, its mounts
  included: symlinks before the last element are followed within its root,
  and the last element is followed only with `-L`. Files copied in keep
  the tar's owner, which is root for files from Windows, with the modes
  go-archive gives Windows files, or, with `-a`, belong to the container's
  user. A symlink copied out to Windows needs the right to make one,
  Developer Mode or an administrator, as with Docker's CLI. Without a
  console on stdout it prints nothing; with one, `Successfully copied`
  and the size. Errors are Docker's words.
- **export** writes the overlay alone, without what is mounted in it, as
  `docker export` does: the image's files, the container's changes, the
  init layer's mount points, and the directories of volumes, but not their
  files.
- **diff** reads the writable layer alone, so its cost is that of the
  changes, not of the image, as Docker's overlay2 does on a kernel without
  `redirect_dir`, which runwhp's lacks. It lists what Docker's does: a path
  added, changed or deleted, and every directory above a change; not a
  directory whose times alone changed, nor a volume's files. Docker lists
  them in no particular order; runwhp sorts them.
- **commit** pauses a running container while it reads the writable
  layer, unless `-p=false`, and makes an image of the container's image
  with that layer on top, as an image layer's tar, whiteouts included. A
  container that changed nothing adds no layer, as with Docker. Its
  configuration is the image's with the container's over it: command and
  entrypoint, environment, working directory, user, hostname, published
  ports and anonymous volumes, then `-c` changes, which `import` takes too.
  Its history adds the container's command, `-a`'s author and `-m`'s
  comment. Like a tagged or loaded image, it has no registry digest; `save`
  fetches the tar metadata of base layers pulled before iteration 14
  through any image in the store that has them and came from a registry. A
  container records its image's ID and keeps that image's configuration in
  the store, so it can be committed after its image is removed. Containers
  made before iteration 15 record their command whole: their commits have
  it as `Cmd`, with no entrypoint.

| Command | |
| --- | --- |
| `cp [-a] [-L] [-q] CONTAINER:SRC DEST\|-` | Copy a container's file or directory to Windows, or as a tar to stdout. `container cp` too |
| `cp [-a] [-L] [-q] SRC\|- CONTAINER:DEST` | Copy a file or directory into a container, or a tar from stdin into a directory of it |
| `export [-o FILE] CONTAINER` | Write the container's root file system to a tar, to stdout unless `-o`; refuses a console. `container export` too |
| `diff CONTAINER` | List the container's changes: `A`dded, `C`hanged, `D`eleted. `container diff` too |
| `commit [-a AUTHOR] [-c CHANGE]... [-m MESSAGE] [-p=false] CONTAINER [REPOSITORY[:TAG]]` | Make an image of the container's changes, and print its ID. `container commit` too |

### Volumes

`-v NAME:PATH` mounts the named volume `NAME`, making it if there is none.
`runwhp volume create NAME` makes one ahead of time, of another size than
the default 64 GiB with `--opt size=`, labelled with `--label`, which
`volume inspect` shows and `volume ls -f label=` filters on, as `name`,
`driver` and `dangling` do. A volume is a directory in the
store's `volumes` directory holding a sparse ext4 disk image, `disk.ext4`,
which the container gets as a disk of its own.

- **Filled from the image.** A new volume mounted where the image has files
  gets a copy of them, as with Docker: owners, modes, times, links, device
  nodes and extended attributes, and the directory's own owner and mode. The
  copy is made while the volume is empty, from the image alone, before
  anything is mounted. `nocopy` (`-v NAME:PATH:nocopy`, or `volume-nocopy`
  with `--mount`) leaves it empty. The container sees the file system's
  `_data` directory, so `lost+found` never shows.
- **One container at a time.** runwhp opens a volume's disk image
  exclusively, so a second container asking for it fails with "volume NAME
  is in use", as does `runwhp volume rm`. One container can mount a volume
  at several paths. `volume rm` and `volume prune` also leave alone a
  volume that a stopped container mounts, as Docker does.
- **Kept safe.** The file system has a journal. When the container ends,
  init freezes it, which writes everything back and leaves it clean, before
  the VM resets. A VM stopped at once, by a second Ctrl+C, the 10-second
  timeout or a killed runwhp, leaves its journal to be replayed on the next
  mount, and what the container had synced is there. `fsync` in a volume
  waits for the host's disk.
- **Space.** The image takes what is written, about 3 MiB when new on NTFS,
  which allocates sparse files 64 KiB at a time, and gives back what the
  container deletes. Its size is a limit, and cannot be changed yet.
- **From Windows** a volume is a disk image, not a folder. WSL can read one
  that no container is using: `wsl -u root mount -o loop,ro,noload` its
  `disk.ext4` on a directory.

`-v PATH` alone, or `--mount type=volume` without a source, makes an
anonymous volume: a directory of the container's own, in its writable
layer, filled from the image in the same way, and removed with the
container. Under a bind mount, as in
`-v .:/app -v /app/node_modules`, it keeps that directory in the container
rather than on the host.

`--tmpfs PATH[:OPTIONS]` mounts a tmpfs with mode 1777, `noexec`, `nosuid`
and `nodev`, unless the options say otherwise with `mode=`, `exec`, `suid`
or `dev`. `ro`, `size`, `uid`, `gid` and `nr_inodes` are taken as tmpfs
takes them. What it holds counts against `--memory`.

| `runwhp volume` | |
| --- | --- |
| `create [--opt size=SIZE] [NAME]` | Make a volume, from `1g` to `16t`; without a name, a random one |
| `ls [-q]` | List volumes with their size and what they take on disk |
| `inspect NAME...` | Describe volumes as JSON, saying whether a container is using them |
| `rm NAME...` | Remove volumes and all they hold |
| `prune -a [-f]` | Remove every volume no container uses, after asking unless `-f`. Without `-a` it removes nothing: anonymous volumes go with their containers |

### Cleaning up

Images, their layers and runwhp's kernels stay in the store until they are
removed, and so do containers, unless run with `--rm`, and volumes.
`runwhp images` lists the images, `rmi` removes them, `system df` shows
what everything takes and what pruning would free, and the prunes remove
what nothing uses. Every prune asks before it removes anything, unless
given `-f`.

- **What keeps a layer.** Images share layers, and so do the containers
  made from them. A layer stays while an image record or a container lists
  it, stopped containers included. A container boots from the layers it
  was made with, never from its image's record, so it keeps them when its
  image is removed with `rmi -f` or pulled again to a new digest; they go
  with the container.
- **Dangling** means the layers nothing lists, which pulling an image
  again leaves behind, images whose layers are gone, and untagged images
  no container uses: those loaded or imported without a name, listed as
  `<none>:<none>`. runwhp keeps a record for each reference, so pulling
  or tagging another image under a name leaves none.
  `image prune` removes what dangles; with `-a`, it removes every image no
  container was made from.
- **Image IDs** are configuration digests, as Docker shows them, and an
  image can be named by a unique prefix of one. An image's SIZE is that of
  its ext4 layers, which is not Docker's.
- **Containers.** `container prune` removes stopped containers and created
  ones; a created container's VM, waiting to start, is stopped first, as
  `delete` stops it.
- **Kernels.** `system prune` removes every kernel but the one this runwhp
  boots and any a container was given from the store with `--kernel`. A
  runwhp of another version that boots one of those fetches it again.
- **Volumes.** `system prune --volumes` removes every volume no container
  uses, running or not, and what is in it, as `volume prune -a` does.
  Docker's removes only anonymous volumes, which runwhp keeps in their
  containers' writable layers.
- **Prunes while runwhp pulls and runs.** A pull holds the store's lock
  until its image is recorded, and a run or create until its container
  is, so that no prune removes layers one has stored or resolved but not
  yet recorded. A prune or `rmi` waits for them, and says so. A layer a
  running guest has open is never removed.

| Command | |
| --- | --- |
| `images [-q] [--digests] [--format table\|json] [REPOSITORY[:TAG]]` | List images, newest first; `image ls` too |
| `image inspect IMAGE...` | Describe images as JSON, in the shape of `docker image inspect`, with the containers made from them |
| `rmi [-f] IMAGE...` | Remove images, by reference, ID or ID prefix, and their layers nothing else lists. Without `-f`, refuses an image a container was made from, and an ID several references name; `image rm` too |
| `image prune [-a] [-f]` | Remove dangling images and layers, or with `-a` every image no container uses |
| `container prune [-f]` | Remove stopped and created containers. `container ls` and `container rm` are `list` and `delete` |
| `system df [-v] [--format table\|json]` | Show what images, containers, volumes and kernels take, and what pruning would free; with `-v`, each of them |
| `system prune [-a] [--volumes] [-f]` | Remove stopped and created containers, dangling images (with `-a`, unused ones), old kernels, temporary files, and with `--volumes` unused volumes |

### Images in and out

Images leave the store in archives that Docker, Podman and runwhp load,
and come into it from them, keeping their IDs, as well as by `pull`.

- **What the store keeps.** A layer is kept as an ext4 image, which
  guests attach, and, beside it, its tar's own metadata in tar-split's
  format, as Docker keeps it: every byte of the tar but its files'
  contents, a few tenths of a percent of its size. The two give the tar
  back byte for byte. Each image's configuration is kept too, as the
  registry or the archive gave it, in the store's `blobs` directory.
  Layers pulled before iteration 14 have no metadata.
- **save** writes images as Docker 29's `docker save` does: an OCI image
  layout, with each layer as a plain tar named by its diffID, and
  Docker's `manifest.json` beside it, which older Docker, Podman and
  go-containerregistry read. It rebuilds each layer's tar from its
  metadata and its files, which it reads in a guest it boots for the
  purpose, with the layers attached read-only, about 0.2 s before the
  first byte; every layer must hash to its diffID, or `save` fails. The
  blobs are Docker's, byte for byte: alpine:3.20's layer, configuration
  and manifest are those Docker 29 writes. A layer pulled before
  iteration 14 gets its metadata from the registry it came from, read
  once more and not kept, and `save` says so. An image named by
  reference is saved under it; by ID, without a name; a repository
  without a tag saves every tag of it. A save holds the store's lock, so
  prunes wait for it.
- **load** takes such archives, from a file or from stdin, which it keeps
  in the store's `tmp` first: Docker's `manifest.json`, or an OCI image
  layout alone, as skopeo and buildah write it, with layers plain or
  compressed with gzip or zstd. Images keep their IDs, and come with their
  layers' metadata, so they can be saved again as they were. Only
  linux/amd64 images are taken.
- **import** makes an image of one layer from a tar of a root file system,
  plain or compressed with gzip, zstd or bzip2, as `docker import` does:
  `-c` takes Dockerfile instructions for its configuration (`CMD`,
  `ENTRYPOINT`, `ENV`, `EXPOSE`, `LABEL`, `ONBUILD`, `STOPSIGNAL`, `USER`,
  `VOLUME` and `WORKDIR`), and `-m` its history's comment.
- **Names.** An image tagged, loaded or imported has no registry digest,
  so `images --digests` shows `<none>`, as Docker does. One loaded or
  imported without a name is listed as `<none>:<none>`, and named by its
  ID, which `run` takes as Docker's does.
- **history** shows how an image was built, from its configuration,
  newest step first, with Docker's size for each step's layer: what its
  files take once unpacked, symlinks counting their targets' length. A
  layer pulled before iteration 14 shows its ext4 image's size instead.

- **push** writes an image to its registry as `docker push` does with
  Docker's classic store: a Docker v2 schema 2 manifest, indented as
  Docker writes it, gzip layers, and the configuration blob as it is, so
  the image keeps its ID and `docker pull` gives it back. The store keeps
  no compressed layers, so push rebuilds each from its metadata and files,
  as `save` does, in a guest booted for the purpose, gzips it and uploads
  it, three at a time while the next is rebuilt; a layer the registry has
  already is not uploaded, and as gzip gives the same bytes each time, a
  second push of an image uploads nothing. A pulled image pushed to
  another repository gets layer digests of its own. The pushed digest is
  kept, and `image inspect` lists it in `RepoDigests`. Credentials are
  those `docker login` stored; a registry on localhost, a loopback or a
  private address, or a `.local` name is spoken to over plain HTTP, as
  pulls do.


| Command | |
| --- | --- |
| `save [-o FILE] IMAGE...` | Write images to an archive, to stdout unless `-o`; refuses a console. `image save` too |
| `load [-i FILE] [-q]` | Load the images in an archive, from stdin unless `-i`; `-q` prints only what was loaded. `image load` too |
| `import [-c CHANGE]... [-m MESSAGE] FILE\|- [REPOSITORY[:TAG]]` | Make an image of one layer from a root file system's tar, and print its ID. `image import` too |
| `tag SOURCE TARGET` | Name an image anew, replacing what TARGET named. `image tag` too |
| `history [-H=false] [--no-trunc] [-q] [--format table\|json] IMAGE` | Show how an image was built. `image history` too |
| `push [-a] [-q] NAME[:TAG]` | Push an image to its registry; `-a` every tag of the repository. `image push` too |

### Building images

`runwhp build` builds an image from a Dockerfile, as `docker build` does
with BuildKit, Docker 29's builder, and records it in the store:

```powershell
.\out\runwhp.exe build -t app:1 .
.\out\runwhp.exe build -f build\Dockerfile --target release --build-arg VERSION=1.2 -t app:1.2 .
Get-Content Dockerfile | .\out\runwhp.exe build -t app:stdin -
.\out\runwhp.exe build --secret id=npm,src=$HOME\.npmrc -t web:1 .
.\out\runwhp.exe builder prune -a
```

- **How it runs.** The Dockerfile is parsed by BuildKit's own parser, and
  dispatched as BuildKit dispatches it: the same stages, argument scopes,
  configuration and history, down to each entry's `created by` and
  BuildKit's comment, and the same rules for when those entries are
  dated. Each step that changes files boots a guest of its own on the
  layers below it, as Docker's classic builder ran each in a container,
  and its changes become the step's layer. A `RUN` takes two guests: one
  runs the command, with the network unless `--network none`, as the
  image's user, in its working directory, with the hostname
  `buildkitsandbox`; and, since a guest stops with its process, a second
  reads what it changed. `COPY` and `ADD` put the files into one guest
  that reads its changes back. Every other instruction changes only the
  configuration, and boots nothing. Stages are built one at a time, and
  only those the target needs.
- **The cache.** A step that has run before with the same inputs is not
  run again: its key is its parent's, the instruction as expanded, the
  arguments, environment, user, working directory and mounts in effect,
  and, for `COPY` and `ADD`, what the files hold, their names, modes and
  links, but not their times, as BuildKit's. A step's layer keeps the
  time it was first made, so building again from the cache gives the same
  image, with the same ID. `--no-cache` runs every step. The cache's
  entries keep their layers from prunes; `builder prune` removes the
  entries no image uses, and with `-a` all of them, and `system prune`
  does as much. `system df` counts them.
- **WORKDIR** makes its directory, as the user in effect, in the stage's
  next step that boots a guest, or in one more step at the stage's end,
  rather than in a layer of its own, as BuildKit does, and its history
  entry is empty, as the classic builder's was; the files in the image
  are the same.
- **Files from Windows.** The build context is a directory, less what its
  `.dockerignore` excludes, or `Dockerfile.dockerignore` beside the
  Dockerfile. Its files arrive as BuildKit's client sends them from
  Windows: owned by root, with the times they have, directories and files
  `0755` and read-only files `0555`; symlinks as they are, with Developer
  Mode, and junctions refused, as BuildKit refuses them.
- **COPY and ADD** follow BuildKit's rules: directories copied by their
  contents, wildcards, destinations made with their parents, a directory
  that is there keeping its mode and owner, `--chown` by name or number,
  resolved in the image's own `/etc/passwd` and `/etc/group`, `--chmod` in
  octal or chmod's symbolic form, `--exclude`, `--parents`, `--link`,
  `--from` a stage or an image, and heredocs. `ADD` unpacks a local tar,
  plain or compressed with gzip, zstd or bzip2, and fetches a URL,
  checked with `--checksum`, from the host, as BuildKit's daemon fetches
  it.
- **RUN** takes BuildKit's heredocs, `--network=none`, and these mounts:
  `type=cache`, a volume of its own per id, kept in the build cache and
  given to one step at a time; `type=tmpfs`; `type=bind` of a context
  directory or file, read-only; and `type=secret`, from `--secret`'s file
  or environment variable, as a file or with `env=` as a variable, in no
  layer and no history. Mount points are left out of the step's layer, as
  BuildKit leaves them.
- **Output** is BuildKit's plain progress, on stderr, each step's output
  timed from its start, then the image's ID; `-q` prints only the ID.
  Linter warnings follow, as Docker prints them. Ctrl+C stops the step
  under way, and the build exits 130; the steps done stay in the cache.

| Flag | Meaning |
| --- | --- |
| `-t`, `--tag NAME` | Name the image; repeatable. Without one, the image is untagged, listed as `<none>` |
| `-f`, `--file FILE` | The Dockerfile; `PATH\Dockerfile` by default, `-` for stdin |
| `--build-arg K[=V]` | A build argument; `K` alone takes it from the environment |
| `--target STAGE` | Build that stage, and what it needs |
| `--no-cache`, `--pull` | Run every step; pull every `FROM` image again |
| `-q`, `--iidfile FILE` | Print only the ID; write it to FILE |
| `--network default\|host\|none` | `RUN` steps' network: runwhp's own, or none |
| `--add-host NAME:IP` | An `/etc/hosts` entry for `RUN` steps; `host-gateway` reaches the host |
| `--label K=V` | A label on the image |
| `--secret id=ID[,src=FILE\|,env=VAR]` | A secret for `RUN --mount=type=secret` |
| `-m`, `--memory`, `--cpus` | Each `RUN` guest's memory and vCPUs, as `run` takes them |

| Command | |
| --- | --- |
| `build [flags] PATH\|-` | Build an image. `image build` too |
| `builder prune [-a] [-f]` | Remove the build cache no image uses, or all of it and its cache mounts |

### Compose

`runwhp compose` runs a compose.yaml project as Docker Compose does:

```powershell
.\out\runwhp.exe compose up -d --wait
.\out\runwhp.exe compose ps
.\out\runwhp.exe compose logs -f
.\out\runwhp.exe compose exec web sh
.\out\runwhp.exe compose run --rm app npm test
.\out\runwhp.exe compose down -v
```

```yaml
services:
  db:
    image: postgres:17
    environment: {POSTGRES_PASSWORD: pw}
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "postgres"]
      interval: 2s
    volumes: [pgdata:/var/lib/postgresql/data]
  app:
    build: ./app
    depends_on: {db: {condition: service_healthy}}
    environment: {DATABASE_URL: "postgres://postgres:pw@db/postgres"}
  web:
    image: nginx:alpine
    ports: ["8080:80"]
    depends_on: [app]
volumes:
  pgdata: {}
```

**The model.** There is no daemon. `runwhp compose` runs
`runwhp-compose.exe`, built from the same code with compose-go, whose
dependencies take some 8 ms to start that no other runwhp process should
pay, as Docker's CLI runs its compose plugin. Each container runs in a
runwhp.exe of its own in the background, as `run -d` runs one, and `compose` finds the
project's containers, networks and volumes again by their labels, Docker
Compose's own: `com.docker.compose.project`, `.service`,
`.container-number`, `.config-hash` and the rest, which `list -f label=`
and `state` show. Containers are named `PROJECT-SERVICE-N`, networks
`PROJECT_NAME`, volumes `PROJECT_NAME`, and built images `PROJECT-SERVICE`,
as Docker Compose names them. A service's containers join the project's
networks with the service's name as an alias, so they reach each other by
it, through the networks of [User-defined networks](#user-defined-networks);
a scaled service's name resolves to every replica.

compose-go, the library Docker Compose reads compose files with, reads the
project: the compose.yaml, compose.yml, docker-compose.yaml or
docker-compose.yml in this directory or the nearest above it, or `-f`'s or
`COMPOSE_FILE`'s, merged; `.env` or `--env-file`; interpolation, `extends`,
`include` and profiles (`--profile`, `COMPOSE_PROFILES`). The project's name
is `-p`'s, `COMPOSE_PROJECT_NAME`'s, the files' `name:`, or the directory's,
lowercased.

| Command | |
| --- | --- |
| `up [-d] [--build] [--no-build] [--pull P] [--force-recreate] [--no-recreate] [--no-deps] [--no-start] [--remove-orphans] [--scale S=N]... [--wait] [--wait-timeout S] [--abort-on-container-exit] [--exit-code-from S] [-t S] [--no-log-prefix] [--no-color] [--timestamps] [SERVICE...]` | Build and pull what is missing, make the networks and volumes, create each service's containers in dependency order, and start them, each once what it `depends_on` is started, healthy or completed successfully. Without `-d`, follow their output, `NAME  \| line`, until they stop or Ctrl+C stops them, a second Ctrl+C killing them |
| `down [-v] [--rmi local\|all] [--remove-orphans] [-t S]` | Stop the containers, dependents first, and remove them and the networks the project made; `-v` its volumes too, `--rmi` its images |
| `create`, `start`, `stop [-t S]`, `restart [-t S]`, `kill [-s SIG]`, `pause`, `unpause`, `rm [-f] [-s]` | As their Docker Compose namesakes, on the services named or all |
| `ps [-a] [-q] [--services] [--status S] [--format table\|json] [SERVICE...]` | List the project's containers, running ones without `-a` |
| `logs [-f] [-t] [-n N] [--since T] [--until T] [--index N] [--no-color] [--no-log-prefix] [SERVICE...]` | Print their output, each line after its container's name |
| `exec [-d] [-T] [-e] [-u] [-w] [--index N] SERVICE COMMAND...` | Run a command in a service's running container, with a terminal when stdin is a console, unless `-T` |
| `run [--rm] [-d] [-T] [--no-deps] [--build] [--name N] [--entrypoint E] [-e] [-l] [-u] [-w] [-v] [-p] [-P] SERVICE [COMMAND...]` | Start what the service depends on, then run a one-off container of it, `PROJECT-SERVICE-run-HEX`, in the foreground, or in the background with `-d` |
| `build [--no-cache] [--pull] [SERVICE...]` | Build the images of services with a `build:` section, one after another, as `build` builds them |
| `pull [SERVICE...]`, `push [SERVICE...]` | Pull the services' images, four at a time; push those built for them |
| `config [-q] [--services] [--volumes] [--networks] [--images] [--format yaml\|json]` | Print the project as compose-go reads it |
| `images`, `port SERVICE PORT`, `ls [-a] [-q]`, `version` | As Docker Compose's |

Without a compose file, `-p NAME` lets `ps`, `logs`, `start`, `stop`,
`restart`, `kill`, `pause`, `unpause`, `rm` and `down` work on a project's
containers by their labels. Progress goes to stderr as Docker Compose writes
it when its output is not a console, a line each:
` Container app-web-1  Started `.

**Changes.** `up` makes again a container whose service changed, as the
`config-hash` label tells, or whose image did, and leaves the others, as
Docker Compose does; `--force-recreate` and `--no-recreate` say otherwise.
A container made again is stopped first, then renamed aside, made anew and
removed, since a runwhp volume and address are one VM's at a time; Docker
Compose makes the new one first. Containers of services the project no
longer has are warned of, and `--remove-orphans` removes them.

**What a service may ask for.**

| | |
| --- | --- |
| Taken as `run` takes it | `image`, `build`, `command`, `entrypoint`, `environment`, `env_file`, `ports` (TCP, on 127.0.0.1 unless an address is given, as `-p`, on a free host port without one), `expose`, `extra_hosts`, `networks` with `aliases` and `ipv4_address`, `links` (as aliases), `volumes` (bind, named, anonymous, tmpfs; `consistency: delegated` for [Delegated mounts](#delegated-mounts)), `tmpfs`, `secrets` and `configs` from files (bound read-only), `restart`, `deploy.restart_policy`, `deploy.replicas`, `scale`, `mem_limit`, `cpus` and `deploy.resources.limits` (`cpus` rounded up to whole vCPUs), `healthcheck`, `depends_on`, `hostname`, `user`, `working_dir`, `tty`, `stdin_open`, `stop_signal`, `stop_grace_period`, `labels`, `logging` (`local`, `json-file` and `none`; `max-size` and `max-file`), `pull_policy`, `platform: linux/amd64`, `storage_opt: size`, `attach`, `profiles`, `extends`, `include` |
| Refused, before anything is made | `network_mode` other than `none` or `bridge`; `devices`, `gpus`, `volumes_from`, `ipc`, `pid` and `uts` of other containers or the host; UDP ports; IPv6 and link-local addresses; network drivers other than `bridge`, `ipam`, `internal` and IPv6 networks; volume drivers and `driver_opts`; a volume two services would mount at once; secrets and configs other than from files; build `ssh`, `additional_contexts`, `cache_to`, other platforms, and git or URL contexts; lifecycle hooks, `provider`, `models` and `jobs` |
| Ignored, with a warning | `privileged`, `cap_add`, `cap_drop`, `security_opt`, `ulimits`, `sysctls`, `shm_size`, `init`, `userns_mode`, the `oom_*`, `pids_limit`, the CPU shares, sets and quotas, `blkio_config`, `mem_reservation`, `memswap_limit`, `read_only`, `dns`, `dns_search`, `dns_opt`, `domainname`, `mac_address`, `develop`, `deploy.resources.reservations`, other log drivers, build `cache_from` and `privileged` |

Containers run on runwhp's kernel, or `RUNWHP_KERNEL`'s, never with
`--module`, and with half this machine's memory and up to 4 vCPUs unless a
service says otherwise, as `run`'s do.

### Docker Engine API

`runwhp docker-api start` serves Docker's Engine API, so that the docker
CLI, and other clients of Docker's API, run containers through runwhp:

```powershell
.\out\runwhp.exe docker-api start
docker --context runwhp run --rm alpine:3.20 echo hello
docker context use runwhp
docker run -d --name web -p 8080:80 nginx:alpine
docker build -t mine/app:1 .
docker compose up -d   # with Docker Compose's plugin; see Docker's tools below
.\out\runwhp.exe docker-api status
.\out\runwhp.exe docker-api stop
```

**The model.** There is still no daemon that holds containers. `start`
runs a runwhp in the background, as `run -d` runs a container's, that
serves the API on a named pipe of runwhp's own, `\\.\pipe\runwhp_engine`,
or for a store other than the default one, `RUNWHP_ROOT`'s, a pipe with
the store's hash in its name. It writes a docker context for the pipe,
named `runwhp` (or with the same hash), into the docker CLI's
configuration, `%USERPROFILE%\.docker` or `DOCKER_CONFIG`, and leaves the
current context as it is; `DOCKER_HOST=npipe:////./pipe/runwhp_engine` works
as well. Only with `start --docker-engine` does it serve Docker's own
pipe, `\\.\pipe\docker_engine`, too, where tools find Docker Desktop when
given no context or `DOCKER_HOST`; it refuses to if something else serves
it. Only the user and SYSTEM can connect. The server translates each request onto what
runwhp's own commands do, on the same store: each container runs in a
runwhp of its own, as `run -d`'s does, so the API and runwhp's commands see
the same containers, images, networks and volumes, and containers outlive
the server; `stop` ends it, leaving them running, and `start` serves them
again. Its log is `docker-api\server.log` in the store; `start --debug`
adds create requests' bodies.

It answers API versions 1.40 to 1.56, those of Docker 29, with Docker's
types (`github.com/moby/moby/api`), headers, errors and statuses. The
docker CLI 29.9, Docker's static `docker.exe` for Windows, is what is
tested, end to end: `run` (attached, `-d`, `-i`, `-t`, `--rm`),
`create`, `start [-a]`, `stop`, `restart`, `kill`, `pause`, `unpause`,
`rename`, `rm`, `wait`, `ps` with Docker's filters, `inspect`, `logs [-f]`,
`attach` with detach keys, `exec [-i] [-t] [-d]`, `cp`, `diff`, `export`,
`commit`, `stats`, `top`, `port`, `events`, `pull`, `images`,
`image inspect`, `history`, `tag`, `rmi`, `save`, `load`, `import`,
`build`, `push`, `login`, `network`, `volume`, `system df`,
`system prune`, `info` and `version`. So are Docker Compose, Docker's Go
and Python SDKs, testcontainers-go and the Dev Containers CLI: see
[Docker's tools](#dockers-tools).

**Creating a container.** A create request becomes what `run` would make
of the same flags, with `run`'s checks; what runwhp cannot do as asked is
refused before anything is made, naming the flags, and what it leaves out
without changing what the container does is warned of, as the docker CLI
prints a create's warnings:

| | |
| --- | --- |
| Taken | image, command, `--entrypoint`, `-e`, `-w`, `-u`, `--hostname`, `-i`, `-t`, `-l`, `--stop-signal`, `--stop-timeout`, the `--health-*` flags, `-m`, `--cpus` (rounded up to whole vCPUs, with a warning), `--network` (Docker's `bridge` and `default` are runwhp's default network) with `--network-alias` and `--ip`, `-p` (on 127.0.0.1 unless an address is given; without a host port, or with `0`, on a free one), `-P` and `--expose`, `--add-host`, `-v`, `--mount`, `--tmpfs`, anonymous volumes, `--restart`, `--rm`, `--log-driver json-file` (Docker's default, and the API's: runwhp's own log under Docker's name), `local` or `none` with `--log-opt`, `--storage-opt size` |
| Refused | `--privileged`, `--cap-add`, `--device`, `--gpus`, `--pid`, `--ipc`, `--uts` and `--userns` of the host, `--network host` and `container:`, `--dns*`, `--link`, `--volumes-from`, volume drivers, `--cpuset-*`, `--cpu-quota`, `--sysctl`, `--security-opt`, `--runtime` other than runwhp, UDP ports, binds of Docker's socket or of a named pipe, `--group-add`, `--pids-limit`, `--mac-address`, `--domainname`, IPv6 addresses, relative bind paths, platforms other than `linux/amd64` |
| Warned of | `--init` (runwhp's init is always there), `--shm-size`, `--oom-score-adj`, `--ulimit`, `--memory-swap`, `--cpu-shares`, `--blkio-weight`, `--cgroupns`, `--read-only` |

A missing image is a 404, as Docker's daemon answers it, and the docker CLI
pulls it and asks again, so `docker run` pulls as it does against Docker. A
bare `-e KEY` is the client's to resolve, and one it did not is dropped.
Containers boot runwhp's kernel, or the server's `RUNWHP_KERNEL`, never
with `--module`. A created container's VM is already booted, as runwhp's
`create` boots one, where Docker's created containers take nothing.

**Streams.** Attaches and execs take the connection over, as Docker's do,
and carry the container's output in Docker's multiplexed framing, which is
the log's, unless it has a terminal; the pipe is in message mode, as
Docker's is, so that a client's half-close ends the container's input. With
a terminal, the detach keys, Ctrl+P Ctrl+Q unless the client says, detach,
as Docker's daemon does. Execs live in the server: one made, started and
inspected by the API ends with the server, as do attaches and log follows;
the containers do not, nor do execs started with `-d`, whose inspect says
when and how they ended while the server runs. An archive put into a
container may be compressed, as Docker's daemon takes it. Stats come from the container's runwhp each second,
with `system_cpu_usage` worked out so that the CPU percentage the docker
CLI shows is runwhp's own.

**Events.** The server reports what its requests did, and watches the
store's containers, every second, or every 250 ms while a client follows
`/events`, so that what runwhp's own commands, compose, and restart policies
do shows too: created, started, died, paused, unpaused, renamed, health
changed and destroyed. A stop through the API adds a kill with the stop
signal and a stop, and a restart a restart, as Docker's daemon gives them.
It keeps the last 1024 events for `--since`.

**What boots a guest.** A process runs one guest at a time, and the server
none: a build, a save, a push, and the files of a stopped container, for
`cp`, `diff`, `export` and `commit`, run in a `runwhp __api-job` of its own,
which ends with the server. A running container's files are reached through
its runwhp, in the server.

**Images.** Pulls report Docker's progress, from the layers runwhp fetches
and converts: `Pulling fs layer`, `Downloading`, `Pull complete`, or
`Already exists`. A client's credentials, its `X-Registry-Auth`, go with its
pull or push; `docker login` checks them against the registry, then stores
them in Docker's `config.json`, where runwhp's own pulls and pushes find
them too. `docker build` uses the classic builder's endpoint: the docker
CLI sends the context, which a job extracts and builds as `runwhp build`
does, and gets the same image ID; the static `docker.exe` has no buildx, and
runwhp has no BuildKit session. `GET /distribution/NAME/json`, the SDKs'
distribution inspect, looks an image up in its registry without pulling it.

#### Docker's tools

These run through the API as they do against Docker, each tested end to
end. Each finds the API through `DOCKER_HOST`, the docker CLI's current
context, or, with `docker-api start --docker-engine`, Docker's own pipe:

| Tool | Tested | What to know |
| --- | --- | --- |
| Docker Compose, the plugin | 5.7.1 | Put `docker-compose.exe` in `%USERPROFILE%\.docker\cli-plugins` as `docker-compose.exe`, or in `DOCKER_CONFIG`'s. Without buildx it builds with the classic builder, saying so, as runwhp serves; with buildx installed it would want BuildKit, which runwhp does not serve: set `DOCKER_BUILDKIT=0`. `ports: ["80"]` gets a free host port. A project it makes shows in `runwhp compose ps`, and the reverse |
| testcontainers-go | 0.45.0 | Set `TESTCONTAINERS_RYUK_DISABLED=true`: its reaper mounts Docker's socket, which no container here can reach, and is refused. It reads `DOCKER_HOST`, or `tc.host` in `~/.testcontainers.properties`, or finds `docker_engine` with `--docker-engine`, not docker contexts. Its modules, wait strategies, files and networks work |
| Docker's Go SDK, `github.com/moby/moby/client` | v0.6.2 | `client.FromEnv` reads `DOCKER_HOST`, not contexts |
| docker-py | 7.2.0, with pywin32 | `docker.from_env()` reads `DOCKER_HOST`, or since 7.2 the current context. `containers.run` gives a run's output, as the API reports runwhp's log as `json-file` |
| Dev Containers | the CLI 0.90.0, which VS Code's extension bundles | From an image, a Dockerfile with features, built with the classic builder, or a compose file. In VS Code, point the extension at runwhp with `docker.context`, or start the API with `--docker-engine`. A volume two dev containers mount at once is refused, as a runwhp volume is one running container's |

Testcontainers for Java and .NET, and Docker's other SDKs, are untried.

### nerdctl and containerd

runwhp is also a containerd runtime: stock nerdctl, through a containerd of
the user's own, runs Linux containers on it, unelevated. `runwhp
containerd` starts that containerd, already set up, and prints what
nerdctl needs:

```powershell
runwhp containerd
# in another terminal, with the nerdctl.toml lines it printed:
nerdctl run --rm --runtime io.containerd.runwhp.v1 --platform linux/amd64 --network none alpine echo hello
```

containerd 2.4 and nerdctl 2.4 must be installed, `containerd.exe` on
PATH or given with `--containerd`. runwhp works with neither unless asked.

- **What runs.** containerd runs runwhp's shim,
  `containerd-shim-runwhp-v1.exe`, which build.ps1 builds beside
  runwhp.exe, for each task of the runtime `io.containerd.runwhp.v1`. The
  shim is the task's VMM: it boots the container's VM, as runwhp does,
  in its own process, which outlives containerd, and serves containerd the
  task API. `nerdctl run`, `create`, `start`, `stop`, `kill`, `rm`, `ps`,
  `logs`, `exec`, `wait`, `pause`, `unpause` and `inspect` work as with any
  runtime, with input (`-i`) and, run detached, a terminal (`-t`), whose
  size the shim passes on. Containers survive `runwhp containerd` ending,
  and containerd finds them again when it starts.
- **Images and snapshots.** nerdctl pulls images into containerd's own
  store. Each layer is converted to an ext4 image, `layer.vhd`, by
  containerd's LCOW differ, which uses the same converter as runwhp, and
  kept by runwhp's snapshotter, containerd's LCOW snapshotter served by the
  shim beside containerd. It differs in two ways: a view of an image has no
  mounts, since nerdctl mounts one on the host for every container, which
  Windows cannot do with Linux layers; and a container's snapshot holds
  runwhp's writable layer, `rw.ext4`, which outlives the task, so
  `nerdctl start` keeps what a stopped container wrote. These layers are
  not runwhp's: `runwhp images` does not list them, and `runwhp pull` does
  not share them.
- **The spec.** nerdctl on Windows writes a Linux image's spec with
  memory and CPUs in its Windows section, destinations as Windows paths and
  no hostname but in an annotation; the shim reads it that way, and reads a
  Linux spec, as ctr writes one, too. `--memory` sizes the guest's RAM and
  `--cpus` counts vCPUs, rounded up, as `runwhp run` takes them; without
  them, runwhp's defaults apply. `-u` takes a name, or a uid with a group:
  containerd resolves a uid alone through a host mount of the image, which
  fails.
- **The network.** nerdctl's own networks need HNS, which needs an
  administrator, so runs say `--network none`, and every container gets
  runwhp's network anyway, with nerdctl's `-p` ports published on the host
  as `runwhp run -p` publishes them, TCP only, on `0.0.0.0` unless an
  address is given. `--annotation io.runwhp.network=none` leaves a container
  with loopback alone. `--add-host NAME:host-gateway` reaches the host's
  loopback with `host_gateway_ip = '10.0.2.2'` in nerdctl.toml.
- **Mounts.** nerdctl on Windows takes `-v` destinations as Windows paths:
  `-v C:\src:C:\data` mounts `C:\src` at `/data`, the drive letter
  dropped. Bind mounts, named volumes and an image's VOLUMEs are host
  directories, shared as bind mounts are; a volume is filled from the
  image while it is empty, as Docker fills one. `--mount` and `--tmpfs`
  are not on nerdctl for Windows.
- **Output.** A foreground run's output goes to nerdctl's pipes; `run -d`'s
  to nerdctl's logging binary, which keeps the log `nerdctl logs` reads,
  started by the shim as containerd's logging protocol has it. nerdctl
  keeps no log of a foreground run on Windows.

`runwhp containerd` keeps everything in one directory,
`%LOCALAPPDATA%\runwhp\containerd` unless `--dir` says, on NTFS:
containerd's root and state, which it makes itself first, since
containerd run unelevated makes them for Administrators and SYSTEM alone
and locks itself out; the snapshots; the shims' containers, in a runwhp
store of their own that runwhp's prunes leave alone; the generated
`config.toml`; and `containerd.log`. The configuration disables CRI and the
CimFS snapshotters, which cannot start unelevated, and points containerd at
runwhp's snapshotter. containerd and the snapshotter run in a job that ends
with `runwhp containerd`; the shims leave it.

| Command | |
| --- | --- |
| `containerd [--address PIPE] [--dir DIR] [--containerd EXE] [--debug]` | Run a containerd of your own for nerdctl until Ctrl+C, at `\\.\pipe\containerd-runwhp` unless `--address` names another pipe; `--debug` also prints its log |

### Kernel

runwhp boots unmodified upstream Linux, currently 6.18.64 (a longterm
release), built for it with everything built in and no modules. The
configuration, [`kernel/runwhp.config`](kernel/runwhp.config), applies over
`allnoconfig`, so anything it does not name stays off, and has four groups:

| Group | What |
| --- | --- |
| Booting on runwhp's VMM | Hyper-V clock and APIC enlightenments without VMBus, ACPI (which needs PCI support, though runwhp boots with `pci=off`), virtio-mmio, the COM1 console, an uncompressed initramfs |
| runwhp-init | ext4, overlayfs, tmpfs with xattrs, devtmpfs, virtio block, console and network devices, IPv4 and Unix sockets |
| Containers | Namespaces, cgroup v2 controllers, seccomp, BPF, IPv6, 32-bit binaries, `binfmt_misc`, FUSE, TUN, veth, bridges, nftables and iptables (both over nftables and legacy) |
| Later work | Host file sharing: virtio-fs, which bind mounts now use, and 9p, which nothing uses yet; vsock, which `runwhp exec` uses; the virtio balloon with free page reporting, which gives back the memory a guest frees |

CPU vulnerability mitigations stay at upstream's defaults. The kernel is
LZ4-compressed, which decompresses fastest.

**Where it comes from.** CI publishes each build to
`ghcr.io/quasidock/runwhp-kernel` as an OCI artifact, tagged with the
upstream version and a build revision (`6.18.64-1`).
[`internal/kernel/pin.go`](internal/kernel/pin.go) names the build this
runwhp boots by its manifest digest, which stands in for a signature: runwhp
fetches only that digest and checks every byte against it. The first `run`
or `pull` downloads the bzImage, anonymously, into
`kernels\sha256\<digest>\bzImage` in the store, and it is never fetched
again. `--pull never` does not fetch it at all.

**Another kernel.** `--kernel PATH`, or the `RUNWHP_KERNEL` environment
variable, boots any other bzImage. The WSL kernel builds `virtio_blk` as a
module, which runwhp then has to load from its initramfs; copy it out of a WSL
distro and pass it with the development flag `--module`:

```powershell
wsl -e sh -c 'cp /lib/modules/$(uname -r)/kernel/drivers/block/virtio_blk.ko /mnt/c/temp/'
.\out\runwhp.exe run --kernel 'C:\Program Files\WSL\tools\kernel' --module C:\temp\virtio_blk.ko alpine:3.20 uname -r
```

`runwhp exec` needs vsock too, which the WSL kernel also builds as
modules: pass `vmw_vsock_virtio_transport_common.ko` and
`vmw_vsock_virtio_transport.ko`, from its `kernel/net/vmw_vsock`, the same
way.

**Building it.** [`kernel/Dockerfile`](kernel/Dockerfile) builds the kernel
from kernel.org's tarball, checked against the checksum in kernel.org's
signed `sha256sums.asc`, in a Debian image pinned by digest whose packages
come from a fixed `snapshot.debian.org` date, with the build's timestamp,
user and host fixed. The same inputs should give the same bzImage. Locally
it needs Docker inside WSL:

```powershell
.\kernel\build.ps1                  # out\kernel\bzImage, in a few minutes
$env:RUNWHP_KERNEL = "$PWD\out\kernel\bzImage"   # boot it instead of the pinned one
```

[`kernel/full.config`](kernel/full.config) records the configuration
`runwhp.config` resolves to, and the build fails if they disagree, so every
consequence of a change, or of a new upstream release, shows in review.
`.\kernel\build.ps1 -Target config` regenerates it. The build also fails if a
setting in `runwhp.config` does not survive, which is how Kconfig reports a
missing dependency or a renamed option: it does not.

**Publishing it.** [`.github/workflows/kernel.yml`](.github/workflows/kernel.yml)
builds the kernel for every pull request that touches it, and publishes it
from `main`. A tag never moves: if it already names a different build, the
job fails, and `KERNEL_REVISION` in the Dockerfile has to go up. Rerunning it
on an unchanged build publishes nothing. To move runwhp to a new build, copy
the tag and digest from the job's summary into `pin.go`. A new package on
ghcr.io starts private, and runwhp pulls anonymously, so the package has to
be made public once in its settings; the job checks for that.

**Source.** Besides the bzImage, the artifact carries the kernel's
`.config`, a tar of `kernel/`, and kernel.org's tarball, so the source of
the binary is published with it, as GPLv2 asks. runwhp downloads only the
bzImage.

### How a run works

1. The image's layers are pulled and each one is converted to an ext4 image
   with `tar2ext4`, with OCI whiteouts turned into overlayfs whiteouts. Layers
   are cached by diffID and verified against it; empty layers are skipped.
   From before it resolves the image until its container is recorded,
   runwhp holds the store's lock shared, so that no prune removes the
   layers it is about to use.
2. runwhp records the container in the store, takes its lock and serves
   its pipe; for `create`, `run -d` and `start`, a runwhp of its own does
   this and what follows, detached from the caller. It loads its kernel from
   the store, fetching it first if this is the first run, and writes an
   initramfs holding `runwhp-init` and a JSON spec of the process.
   Meanwhile it opens the writable layer: the container's `rw.ext4`, made
   the first time with a journal, or with `--rm` a scratch disk made for
   the run. Either is a sparse file holding an ext4 file system runwhp
   formats itself, writing a few hundred KiB of it.
3. The guest gets the vCPUs `--cpus` asks for, each run by a thread of its
   own. runwhp starts only the first. The others wait until the guest's
   kernel starts them with INIT and SIPI, which the hypervisor's local APIC
   emulation delivers, as it does every interrupt between processors.
   runwhp describes the vCPUs to the guest, in ACPI's MADT and in CPUID, as
   single-threaded cores of one package that share their last-level cache.
   The I/O APIC sends each device's interrupt to the CPU the guest's kernel
   chose for it. Guest RAM is reserved, in one span of runwhp's address
   space with the device hole left out, but nothing of it is committed:
   each 2 MiB of it is committed, mapped into the guest and populated the
   first time runwhp's loader, a device or the guest touches it, the last
   as an exit for an unmapped address, after which the instruction runs
   again. Each access to a device's registers is an exit to runwhp, on the
   vCPU's thread; the guest's kernel makes them with plain movs, which
   runwhp decodes itself, from the instruction bytes the exit carries.
   Anything else goes to `WinHvEmulation.dll`, several times slower.
4. The guest gets one multiport virtio console, with ports named `stdout`,
   `stderr`, `stdin` and `control`; a virtio socket device; a virtio
   memory balloon; a virtio network device, unless `--network none`; a
   virtio-fs device, with bind mounts, and a second for delegated ones; one
   read-only virtio-blk disk per layer; the writable layer; and one disk
   per named volume. Every device offers notification suppression
   (`VIRTIO_RING_F_EVENT_IDX`): the guest asks for interrupts only once it
   has taken what it had, and runwhp for notifications only once it has run
   out of buffers, so a guest refills its receive queues without an exit.
   Each disk's
   serial number names it, so init does not depend on probe order. The vCPU
   that submits disk requests serves the first itself, without holding the
   device, and goroutines serve any others, up to 16 at a time per disk,
   all through overlapped I/O; requests complete in whatever order they
   finish.
   Each port's output is written to the host by a goroutine of its own, and
   input is delivered to the guest from another, so no vCPU ever waits on
   the host's console or pipes.
5. The network device's far end is a user-mode TCP/IP stack inside runwhp,
   gVisor's netstack. Frames the guest sends are copied out on the thread
   of the vCPU that sent them and processed on other goroutines; frames for
   the guest go into receive buffers it has posted, or wait in a bounded
   backlog. The link's MTU is 65520, so bulk data crosses in few, large
   frames, and neither side computes TCP and UDP checksums: the guest sends
   frames with them left partial, and frames for it come marked valid,
   since every frame ends in the stack. The stack takes packets for any
   address, and for each new TCP connection or UDP flow it opens a host
   socket to the same destination and copies between the two. Published
   ports are host listeners whose connections the stack opens towards the
   guest.
6. The virtio-fs device's far end is a FUSE server inside runwhp, which
   serves every mount at once: its root, which only init sees, holds them as
   entries `0`, `1`, and so on. The vCPU that submits requests serves the
   first itself, as for disks, and goroutines any others, up to 16 at a
   time, through Windows' native API; requests complete in whatever order
   they finish. A request about a file Windows may have to fetch first, such
   as a OneDrive placeholder, always goes to a goroutine. A read or write
   carries up to 512 KiB. Delegated mounts have a FUSE server of their own,
   whose connection takes the kernel's writeback cache.
7. `runwhp-init` mounts the layers under an overlay whose upper layer is on
   the writable layer, adds `/proc`, `/sys`, a minimal `/dev` and a
   hostname, configures `eth0` when there is a network, resolves the
   container's user, mounts the virtio-fs device, tells it who owns host
   files without an owner, mounts each volume's file system, and fills new
   volumes from the image. Then, parents first, it binds each share, each
   volume's `_data` and each anonymous volume onto its target, mounts each
   tmpfs, and binds its own `/etc/hostname`, `/etc/hosts` and, with a
   network, `/etc/resolv.conf` over the image's. It moves the overlay over
   the initramfs as the root, as `switch_root` does, which leaves the
   virtio-fs root and the volumes' file systems out of the container's
   reach. For `create` it then says it is ready and waits to be started.
   It runs the process in the root, and starts serving `runwhp exec`. The process's stdin, stdout and stderr are
   pipes, or with `-t` a pseudo-terminal from the container's own devpts,
   and init relays them to and from the ports. A port can be open only once
   at a time, so handing the process the ports themselves would break
   `/dev/stdout` and similar paths.
8. The `control` port carries JSON lines both ways: signals, terminal sizes,
   requests for statistics, the order to start and the host's time from
   the host; from init, when the container is ready, when the process has
   started, the statistics asked for, and how the process ended. The
   kernel boots with the CMOS stub's whole seconds, so runwhp sends its
   time as init opens the control port, every minute after that
   (`RUNWHP_CLOCK_SYNC=SECONDS` changes it, and 0 turns it off), and as
   Windows resumes from sleep; it reads the clock as the message leaves,
   which then takes about 0.3 ms to arrive. Init steps the guest's clock
   by more than 20 ms off, slews it within that, and leaves it within
   1 ms, and starts the container's process only once the first time has
   come, waiting up to 500 ms for it. Init drops CAP_SYS_BOOT from everything it starts, and ignores
   the signals that `reboot` and `halt` send it, so only init resets the
   machine; the ACPI tables also offer a power-off, which stops the VM
   as a reset does. While the process
   runs, init gives page cache back once the guest is idle, and the
   balloon hands the guest's free page reports to runwhp, which gives
   those chunks back to Windows. When the process
   exits, init kills whatever is left, `runwhp exec`'s processes included,
   waits for the last output to reach the ports, reports how it ended, and
   resets the machine, which ends the VMM. First it freezes each volume's
   file system, and a kept writable layer's, which writes it back and
   empties its journal, and writes back the bind mounts' file systems, but
   not a scratch disk's, which is about to be thrown away.
9. `runwhp exec` asks the container's runwhp, on its pipe, to connect it
   to init's exec server, on vsock port 1024; runwhp dials it over the
   virtio socket device and passes bytes both ways without reading them.
   One connection carries the request, signals, terminal sizes and how the
   command ended, and one more each stream: stdin, stdout and stderr, or
   stdout alone with a terminal. Each connection has its own flow control,
   so a slow reader holds up only its own stream. Init runs the command
   with the container's environment, working directory and user unless
   told otherwise; a reaper of its own hands every exit status to whoever
   started the process.

### Code layout

| Path | Contents |
| --- | --- |
| [`cmd/runwhp`](cmd/runwhp) | Command line; built with `-tags compose`, `runwhp-compose.exe`, which `runwhp compose` runs |
| [`cmd/containerd-shim-runwhp-v1`](cmd/containerd-shim-runwhp-v1) | The containerd shim, and runwhp's snapshotter |
| [`internal/ocispec`](internal/ocispec) | An OCI runtime spec, as nerdctl writes one on Windows or ctr one for Linux, turned into a container's configuration |
| [`internal/vmm`](internal/vmm) | Running a container's VM: its devices and storage, the guest's boot, the supervisor that holds the container and serves its pipe, attached clients and pause, and the helper guests that read image layers and stopped containers' files |
| [`internal/whp`](internal/whp) | Bindings for `WinHvPlatform.dll` and `WinHvEmulation.dll` |
| [`internal/vm`](internal/vm) | Partition setup, guest memory committed as it is touched, vCPU loops, decoding of device register accesses, processor topology, serial port, RTC stub, I/O APIC |
| [`internal/boot`](internal/boot) | Kernel loader, page tables, ACPI tables, initramfs builder. Builds on any OS |
| [`internal/virtio`](internal/virtio) | virtio-mmio transport, block device, multiport console, network device, file system device, socket device, memory balloon. Builds on any OS |
| [`internal/netstack`](internal/netstack) | The host end of the container's network: gVisor's netstack, TCP and UDP forwarding, DNS, published ports. Builds on any OS |
| [`internal/netswitch`](internal/netswitch) | User-defined networks without a switch: each guest's frames routed to the others' VMMs, ARP and names answered for them, and the links between VMMs. Builds on any OS |
| [`internal/network`](internal/network) | User-defined networks in the store: subnets, address leases held by the runwhps running containers, what each knows of the others, and their pipes |
| [`internal/compose`](internal/compose) | compose.yaml projects, read with compose-go: services turned into what `run` takes, refused or warned of where runwhp cannot do as they ask, and named, labelled and hashed as Docker Compose does. Builds on any OS |
| [`internal/dockerapi`](internal/dockerapi) | The Engine API's parts that need no Windows: API versions, Docker's filters and JSON message streams, stats as Docker gives them, and the events found by comparing containers' states, with their broker. Builds on any OS; the server itself, its endpoints and jobs are `cmd/runwhp`'s `dockerapi*` files |
| [`internal/fuse`](internal/fuse) | The FUSE protocol as virtio-fs speaks it, served from a file system interface. Builds on any OS |
| [`internal/disk`](internal/disk) | The files behind virtual disks: overlapped I/O from many goroutines, sparse files, freeing ranges, exclusive and delete-on-close opens |
| [`internal/ext4`](internal/ext4) | Makes empty ext4 file systems as mke2fs would, and reads back whether one was left clean. Builds on any OS |
| [`internal/volume`](internal/volume) | Named volumes in the store: names, making, listing and removing them |
| [`internal/container`](internal/container) | Containers in the store: records, names, locks, state, restart policies, logs, written, rotated and read with their times, and the pipe of the runwhp running one, whose attach requests carry frames in the framing of logs before iteration 13 |
| [`internal/hostfs`](internal/hostfs) | The host end of bind mounts: Windows files served as that file system, with WSL's metadata |
| [`internal/image`](internal/image), [`internal/layer`](internal/layer) | Registry pulls, image records and configuration blobs, layer conversion and cache with each layer's tar metadata, image archives read and written, imports, commits and Dockerfile changes, pushes, and listing and removing them |
| [`internal/build`](internal/build), [`internal/buildcache`](internal/buildcache) | The Dockerfile builder: stages, arguments, history and configuration as BuildKit's, the build context and `.dockerignore`, COPY's rules, RUN's heredocs and mounts, the step cache's keys and entries, and the guests that run steps |
| [`internal/rootfs`](internal/rootfs) | The host's client of init's root file server, which cp, export, diff, commit and build reach guests' files through |
| [`internal/gc`](internal/gc) | What in the store is in use: which images and containers list each layer, which volumes and kernels are used; removals planned from that, and the store lock that keeps pulls and runs from racing them |
| [`internal/storefile`](internal/storefile) | Reading and replacing records while other runwhps read them |
| [`internal/kernel`](internal/kernel) | The kernel artifact's format, the pinned build, and fetching it into the store |
| [`kernel`](kernel) | The kernel's configuration and build; [`kernel/push`](kernel/push) publishes it |
| [`internal/guestspec`](internal/guestspec) | The contract between host and guest: the spec, control messages and events, exec's and the file servers' protocols |
| [`internal/passwd`](internal/passwd) | User and group resolution |
| [`guest/init`](guest/init) | `runwhp-init`, PID 1 in the guest: the container's root, its init layer, mounts and cgroup, its process and exec's, and the server of its files, written and read as go-archive does, and of image layers' files a guest booted for save runs |
| [`test/e2e`](test/e2e) | End-to-end tests against WHP |

`go test ./...` runs the unit tests anywhere. The end-to-end tests need a
build, WHP and network access, and run with `RUNWHP_E2E=1`:

```powershell
$env:RUNWHP_E2E = '1'; go test -count=1 ./test/e2e
```

They boot whatever runwhp would: the pinned kernel, or the one
`RUNWHP_KERNEL` names. `RUNWHP_E2E_RUN_FLAGS` adds flags, separated by
semicolons, to every `runwhp run`, such as `--kernel` and `--module` for the
WSL kernel; tests of runwhp's own kernel are then skipped.

## Proposed design

One small virtual machine per container, with the VMM process acting as the
container's supervisor, with no daemon.

| Concern | Approach | State |
| --- | --- | --- |
| Hypervisor | WHP from Go through `WinHvPlatform.dll`, no cgo | Done |
| Device registers | Plain movs, all the guest uses, decoded by the VMM; `WinHvEmulation.dll` for anything else | Done |
| Boot | Direct kernel boot (Linux 64-bit boot protocol), no firmware | Done |
| Clock and timers | Hyper-V enlightenments supply TSC and APIC timer frequencies; init keeps the wall clock with the host's, which sends it its time | Done |
| Interrupts | Hypervisor's local APIC plus an emulated 64-pin I/O APIC | Done, edge-triggered only |
| Processors | One thread per vCPU; the guest starts the others through the hypervisor's local APIC; topology described in CPUID | Done, up to 64 |
| Device discovery | Hardware-reduced ACPI with a generated DSDT | Done |
| Memory | Guest RAM below the device hole and above 4 GiB, committed as the guest touches it; free page reporting through a virtio balloon gives back what it frees | Done; up to the host's RAM, half of it by default, given back 2 MiB at a time |
| Root filesystem | One read-only ext4 image per layer on virtio-blk, overlayfs in the guest, writable layer on a disk of its own | Done; the writable layer is a sparse file, kept with the container, or with `--rm` formatted for each run |
| Layer conversion | `tar2ext4`, cached per layer digest, removed once no image record or container lists it | Done |
| Container I/O | Multiport virtio console with named ports for stdout, stderr, stdin and control; pipes or a pty in the guest | Done, including `-i`, `-t`, resize and signals |
| Guest agent | Tiny init as PID 1 that mounts the overlay and execs the process, instead of hcsshim's GCS plus runc | Done (`runwhp-init`), with an exec server |
| Host to guest | Console ports for the main process; a virtio socket device for further connections | Done: `runwhp exec` uses vsock |
| Bind mounts and volumes | virtio-fs, with a FUSE server in Go serving Windows files and keeping Unix metadata as WSL does, and the writeback cache for delegated mounts; volumes as journaled ext4 disk images on virtio-blk | Done; a volume is used by one container at a time |
| Networking | User-mode network stack in the VMM (gVisor netstack); published ports are loopback listeners; on user-defined networks, frames routed from VMM to VMM over named pipes, with ARP and DNS answered by each | Done, IPv4 only, TCP ports only |
| Command line | runc-compatible `create`, `start`, `state`, `kill`, `delete`, `exec` | Done over runwhp's own images, plus `run`, `list`, `stats`, `stop`, `restart`, `rename`, `logs`, `attach`, `wait`, `top`, `port`, `pause`, `unpause`, `cp`, `export`, `diff`, `commit`, `images`, `rmi`, the prunes, `system df`, `tag`, `history`, `save`, `load`, `import`, `build`, `builder prune`, `push`, `network`, `compose`, `containerd` and `docker-api`, restart policies, healthchecks, labels and filters; OCI bundles only through the shim |
| containerd | A shim v2 (`containerd-shim-runwhp-v1`) that is the task's VMM, taking nerdctl's spec and containerd's LCOW layers; a snapshotter whose views have no mounts; a user containerd set up by `runwhp containerd` | Done: stock nerdctl 2.4, unelevated, `--network none` and runwhp's network |
| Docker Engine API | `runwhp docker-api`: a runwhp in the background serving API 1.40-1.56 on a pipe of its own, with Moby's API types, translating each request onto runwhp's commands; guests booted for builds, saves, pushes and stopped containers' files in jobs of their own; Docker's `docker_engine` pipe too, when asked | Done: the docker CLI 29, Docker Compose 5.7, testcontainers-go, Docker's Go and Python SDKs and the Dev Containers CLI, end to end |
| Container files | A file server in init, reached through the container's runwhp, or for a stopped container through a guest booted with its storage; tars written and read as Docker's daemon does | Done: `cp`, `export`, `diff`, `commit` |
| Container logs | Frames stamped with the host's time, rotated as Docker's `local` driver rotates | Done; every kept container has one |
| Image archives | Docker 29's `docker save` format, rebuilt from each layer's tar-split metadata and its files, read in a guest booted for the purpose | Done; same IDs and blobs as Docker |
| Image builds | runwhp's own Dockerfile builder: BuildKit's parser, and its dockerfile2llb's dispatch, with each RUN, COPY and ADD in a guest of its own, a cache keyed by each step's inputs, and `push` from the store, its layers rebuilt and gzipped | Done; BuildKit's history and configuration, and cache, tmpfs, bind and secret mounts; WORKDIR folded into the next step |
| Kernel | Our own build with virtio drivers built in, shipped as a signed OCI artifact | Done: Linux 6.18 LTS on ghcr.io, pinned by digest rather than signed |

### Out of scope for now

Linux GUI applications, GPU access, ARM64 hosts, and running many containers in
one shared VM.

## Boot spike

The spike lives in [`spike/`](spike/) and is kept as a reference; the runtime
in `internal/` grew out of it. It is about 1,800 lines of Go, guest init
included, with no dependencies beyond the standard library.

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
go build -o out/spike/runwhp-spike.exe ./spike

$env:GOOS = 'linux'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags="-s -w" -o out/spike/init ./spike/init
$env:GOOS = $null; $env:CGO_ENABLED = $null
```

Kernel and userspace only:

```powershell
.\out\spike\runwhp-spike.exe -init out\spike\init
```

With disks. The fixture script runs inside WSL and needs `e2fsprogs`. Run it
from the repository root; WSL starts in the same directory, so relative paths
work:

```powershell
wsl -e sh spike/fixtures.sh out/spike
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

## Open questions

- containerd and nerdctl can drive runwhp now, through its shim and
  snapshotter. Whether runwhp's own command line should also take OCI
  bundles, as runc's does, is still open; and whether nerdctl should run
  Linux containers on Windows without the flags it needs today
  (`--runtime`, `--platform`, `--network none`) is nerdctl's to settle.
- Should the Engine API start at logon? It serves Docker's own
  `\\.\pipe\docker_engine` when started with `--docker-engine`. And now
  that Docker Compose works through the API, should it take over from
  `runwhp compose`, or BuildKit's session be served for buildx, which
  Docker Compose and Dev Containers use when it is installed?
- Should `build` also run BuildKit itself, in a guest, for what its own
  builder lacks: `bind,from=` and ssh mounts, cache export and import,
  stages built at once, and steps that share one guest rather than boot
  one each?
- Is a one-time administrator step acceptable on machines where the hypervisor
  is off, or must there be a fallback that needs none?
- virtio-mmio with generated ACPI, as spiked, or virtio over PCI with MSI-X,
  which avoids both the I/O APIC and the bytecode but needs PCI emulation?
  MSI-X would also give each device a queue and an interrupt per vCPU, and
  spare the two exits every virtio-mmio interrupt costs, reading the
  interrupt status and acknowledging it: two of the three exits of a small
  bind mount request.
- What protection does the guest kernel owe a container from itself? The VM
  is the boundary with the host, and the container is the guest's only
  tenant, so KASLR is off; CPU mitigations are on, at a cost to system
  calls that has not been measured.
