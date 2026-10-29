<h1 align="center">runner-up</h1>

<p align="center">
  <a href="https://github.com/kunchenguid/runner-up/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/kunchenguid/runner-up/ci.yml?style=flat-square&label=CI" /></a>
  <a href="https://github.com/kunchenguid/runner-up/actions/workflows/release.yml"><img alt="Release" src="https://img.shields.io/github/actions/workflow/status/kunchenguid/runner-up/release.yml?style=flat-square&label=Release" /></a>
  <a href="#"><img alt="Platform" src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square" /></a>
  <a href="https://x.com/kunchenguid"><img alt="X" src="https://img.shields.io/badge/X-@kunchenguid-black?style=flat-square" /></a>
  <a href="https://discord.gg/BW4aJuQhTf"><img alt="Discord" src="https://img.shields.io/discord/1439901831038763092?style=flat-square&label=discord" /></a>
</p>

<h3 align="center">Your machines, GitHub's queue, one fresh place per job.</h3>

You have spare machines: a Linux box, a Mac, a Windows PC. Your GitHub Actions
jobs wait in line for hosted runners, or run on a self-hosted runner that keeps
every job's leftovers for the next one.

runner-up runs GitHub Actions jobs on your own machines. Each job gets a fresh
Docker container or KVM virtual machine on Linux, a fresh tart clone of a
macOS image on a Mac, or a fresh Windows account or Hyper-V virtual machine on
Windows, which is removed when the job ends. A place can also run its jobs on
the machine itself.

One coordinator talks to GitHub through runner scale sets and hears of every
waiting job. Jobs wait at GitHub and hold nothing until a place is free. Then
the coordinator picks the job its order rules choose, acquires it, and starts a
one-job runner for it. It starts the runner by running the agent on that
place's machine over SSH.

- **Places.** Each machine has places. A place runs one job at a time and
  serves one or more pools, the labels jobs ask for in `runs-on`.
- **Budgets.** A machine's places share its cores and memory, less the room
  kept for the machine's owner. A place takes a job only while its share fits.
  A Mac runs at most two macOS virtual machines at once, counting ones
  runner-up did not start (see [Macs](#macs)).
- **Order.** When a place frees, the order rules pick which waiting job it
  takes, across repositories (see [Order rules](#order-rules)).
- **Forks.** Jobs from another repository's pull request are refused by
  default, on every provider (see [Pull requests from forks](#pull-requests-from-forks)).
- **Restarts.** A running job keeps its place while the coordinator restarts.
  A crashed place is cleaned up, and its job gets a new runner.
- **Sign-in.** The coordinator uses the GitHub command line's sign-in. It
  stores no token: it asks `gh auth token` each time it opens a session.

## Status

runner-up is young. Read this before you trust it with your jobs.

- **Runner scale sets are in preview.** runner-up talks to GitHub through
  [`actions/scaleset`](https://github.com/actions/scaleset), GitHub's client
  for runner scale sets, which GitHub still calls a preview. A change on
  GitHub's side can break runner-up until it catches up.
- **Proven with real jobs:** Docker and KVM places on Linux, tart and host
  places on a Mac, a job running on through a coordinator restart, and a
  crashed place's cleanup.
- **Proven only in part:** Hyper-V places have run only a check's test jobs,
  not yet anyone's daily work. Account places on Windows pass their tests but
  have not yet run a real job.
- **Order rules** are proven by tests that replay GitHub's recorded scale set
  messages. Within one pool of one repository GitHub picks which job a runner
  takes, so the order there is only close (see [Order rules](#order-rules)).

## Quick Start

```sh
# What to serve, and the label jobs ask for.
runner-up init --repo you/app --pool my-pool

# Add a machine over SSH; it gets as many places as its cores and memory fit.
runner-up machine add box --ssh ci@box

runner-up serve
```

Then ask for the pool in a workflow:

```yaml
jobs:
  build:
    runs-on: my-pool
```

## Install

**macOS / Linux**

```sh
curl -fsSL https://kunchenguid.github.io/runner-up/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://kunchenguid.github.io/runner-up/install.ps1 | iex
```

A Windows machine then needs one step as administrator (see
[Windows machines](#windows-machines)).

**Nix**

```sh
nix run github:kunchenguid/runner-up -- version
```

Install into your Nix profile:

```sh
nix profile add github:kunchenguid/runner-up
```

Or add to your flake inputs:

```nix
runner-up.url = "github:kunchenguid/runner-up";
```

The flake exposes `#default` and `#runner-up` package outputs, plus `apps` for
`nix run`, for `aarch64-darwin`, `aarch64-linux` and `x86_64-linux`. It builds
with Go 1.27.3, which your own nixpkgs may not have yet, so let it keep its own
nixpkgs rather than follow yours.

**Go**

```sh
go install github.com/kunchenguid/runner-up@latest
```

**From source**

```sh
git clone https://github.com/kunchenguid/runner-up.git
cd runner-up
make install
```

## What you need

On the coordinator's machine (Linux or macOS):

- [`gh`](https://cli.github.com), signed in with `gh auth login` as an admin of
  each repository you serve. To serve an organization, the sign-in also needs
  the `admin:org` scope: `gh auth refresh -h github.com -s admin:org`.
- SSH access to each machine with a key, without a password prompt
  (`ssh -o BatchMode=yes ci@box true` must work).
- runner-up (see [Install](#install)).

On each machine (Linux on x86-64 or arm64, or a Mac), for the provider its
places use:

- **Docker places:** Docker, with the SSH user in the `docker` group.
- **KVM places:** `/dev/kvm` usable by the SSH user (the `kvm` group),
  `qemu-system-x86_64` or `qemu-system-aarch64`, `qemu-img`, and on arm64 the
  UEFI firmware (`qemu-efi-aarch64`). You also need a cloud image with
  cloud-init on the machine, such as Ubuntu's:

  ```sh
  curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
  ```

- **tart places** (a Mac with Apple silicon): [tart](https://tart.run), from
  its makers (`brew install cirruslabs/cli/tart`), and a macOS image pulled on
  the Mac (see [macOS images](#macos-images)). runner-up runs the tart you
  install and never bundles it; tart's license is its makers' (FSL-1.1).
- **host places** (Linux or a Mac): nothing more. The job runs as the SSH user
  (see [Trade-offs](#trade-offs)).


On each Windows machine (Windows 10, 11 or Server 2019 and later, x86-64):

- **Account places:** nothing more. Each job runs as a fresh standard account
  that runner-up's Windows service makes, contains to the place's cores and
  memory, and deletes afterwards.
- **Hyper-V places:** Hyper-V, and a base disk for the job VMs: an Ubuntu cloud
  image converted to VHDX, such as with `qemu-img` on any machine:

  ```sh
  curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
  qemu-img convert -O vhdx -o subformat=dynamic noble-server-cloudimg-amd64.img noble.vhdx
  ```

  Copy `noble.vhdx` to the machine, for example to `C:\runner-up\noble.vhdx`.
  A disk you build with your own tools works too, if cloud-init still runs at
  its boot. The runner gets the variables the disk sets in `/etc/environment`,
  as a login would: set `RUNNER_TOOL_CACHE` there for tools you install in
  advance, as GitHub's Ubuntu images do.

runner-up installs itself on each Linux machine and Mac when you add it. A Windows
machine needs one step first (see [Windows machines](#windows-machines)).

## Setting up

```sh
# What to serve, and the label jobs ask for.
runner-up init --repo you/app --pool my-pool

# Add machines. Each gets as many places as its cores and memory fit,
# less the room kept for its owner (1 core and 1024 MB by default).
runner-up machine add box --ssh ci@box
runner-up machine add big --ssh ci@big --provider kvm \
    --image /home/ci/noble-server-cloudimg-amd64.img --place-cores 4 --place-memory 8192
# A Mac: two tart places of 4 cores and 8 GB by default.
runner-up machine add mac --ssh you@mac --image ghcr.io/cirruslabs/macos-sequoia-base:latest

runner-up check-config
runner-up serve
```

`init` checks that your sign-in can create runners for each repository.
`machine add` reaches the machine over SSH. It copies runner-up there (to
`~/.local/bin`) when it is missing, reads the machine's cores and memory, and
checks that its provider works: Docker by default on Linux, tart on a Mac, or
the one `--provider` names (`docker`, `kvm`, `tart` or `host`). It copies
itself only to a machine of its own system and architecture; for another,
download runner-up for it from the
[releases](https://github.com/kunchenguid/runner-up/releases) and pass
`--install FILE`. Both commands write the setup file,
`~/.config/runner-up/config.json` by default. You can edit that file by hand,
and it never holds a token.

Then ask for the pool in a workflow:

```yaml
jobs:
  build:
    runs-on: my-pool
```

`runner-up status` shows each place, the job it runs, and the jobs waiting.

### Windows machines

On the Windows machine, in a terminal opened with **Run as administrator**, run
runner-up's Windows build (see [Install](#install)) once:

```bat
runner-up.exe setup --user ci --ssh --ssh-key C:\path\to\coordinator-key.pub --hyperv
```

`setup` installs runner-up in `C:\Program Files\runner-up` and on the PATH,
and installs the runner-up service, which makes and deletes the job accounts.
`--user` names the account the coordinator signs in as; it may be a standard
account. `--ssh` turns on Windows' OpenSSH server, and `--ssh-key` lets the
coordinator's key sign in as that account. `--hyperv` turns Hyper-V on and lets
the account make virtual machines; leave it out for account places only.
Running `setup` again is harmless and updates runner-up. Restart the machine
if it says so.

Then, on the coordinator:

```sh
runner-up machine add pc --ssh ci@pc                       # account places
runner-up machine add pc --ssh ci@pc --provider hyperv \
    --image 'C:\runner-up\noble.vhdx' --place-cores 4 --place-memory 8192
```

`machine add` notices the machine runs Windows. Account places run the jobs of
a pool whose workflows expect Windows (`shell: pwsh`, Git Bash for
`shell: bash` when Git for Windows is installed). Hyper-V places run Linux
jobs. A Hyper-V place joins Hyper-V's Default Switch; set `--switch` for
another.

### Running as a service

For example, as a systemd user service on the coordinator's machine:

```ini
# ~/.config/systemd/user/runner-up.service
[Unit]
Description=runner-up coordinator
After=network-online.target

[Service]
ExecStart=%h/bin/runner-up serve
Restart=on-failure
# Stop only the coordinator: running jobs' place runners carry on.
KillMode=process

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now runner-up
loginctl enable-linger "$USER"   # keep it running while you are logged out
```

On a Mac, run it as a LaunchAgent, so it runs in your login session and `gh`
can read its sign-in from your keychain:

```xml
<!-- ~/Library/LaunchAgents/com.github.runner-up.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.github.runner-up</string>
  <key>ProgramArguments</key>
  <array><string>/Users/you/bin/runner-up</string><string>serve</string></array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <!-- Stop only the coordinator: running jobs' place runners carry on. -->
  <key>AbandonProcessGroup</key><true/>
  <key>StandardOutPath</key><string>/Users/you/Library/Logs/runner-up.log</string>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/runner-up.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.github.runner-up.plist
```

When the coordinator stops, running jobs carry on. Each place's runner runs in
its own process, which holds the place until its job ends. Waiting jobs stay
queued at GitHub, and the next start picks them up again.

## Setup file

Besides what `init` and `machine add` write, you can set:

| Field | Default | Meaning |
| --- | --- | --- |
| `order` | `["waited:60m", "shortest"]` | the [order rules](#order-rules) |
| `allowForkPullRequests` | `false` | let jobs from forks' pull requests run |
| `startTimeoutSeconds` | 600 | how long a runner may wait for its job |
| `bootTimeoutSeconds` | 300 | how long a KVM, tart or Hyper-V place may take to boot |
| `guestCheckSeconds` | 30 | how often a tart place checks its clone has not restarted |
| `maxJobSeconds` | 21600 | the longest a job may run |
| `logKeepDays` | 30 | how long place logs are kept |

Each machine's `cores`, `memoryMB`, `roomCores` and `roomMemoryMB` set its
budget. Each place's `cores` and `memoryMB` are what its container or virtual
machine gets, and also its share of the budget. A host place's job is not held
to them: they are only its share. Places may share a machine
without all fitting at once. They then take turns: `check-config` says which
machines share this way.

Each place may also set:

| Field | Default | Meaning |
| --- | --- | --- |
| `workFolder` | `_work` | where the runner keeps its jobs' work, relative to the runner's folder or absolute; GitHub's Ubuntu runners use `/home/runner/work` |
| `toolCache` | see meaning | the runner's tool cache (`RUNNER_TOOL_CACHE`). A host place keeps it between jobs, in its folder under the agent's by default, so a tool such as `setup-node`'s Node is downloaded once. For a tart place, a folder inside the clone that the image was set up with; by default the runner's own, which every job starts without |

## Macs

A Mac's places are tart places, a fresh clone of a macOS image per job, or a
host place that runs jobs on the Mac itself.

For each job, a tart place's agent clones the place's image as
`runner-up-PLACE`, sizes the clone to the place's cores and memory, and starts
it with no window. The clone sees one folder of the Mac, read-only: the
runner's release archive and, with the fork guard on, runner-up itself. The
agent then starts the runner through tart's guest agent (`tart exec`), handing
it the registration on its input. When the job ends, or its place runner lets
go, the clone is stopped and deleted.

- **Two macOS VMs at once.** Apple's Virtualization framework runs at most two
  macOS virtual machines on a Mac at once. The coordinator counts the Mac's
  tart places whose runners are alive, and asks the Mac's agent every 30
  seconds which macOS VMs run there, so a VM you start by hand takes one of
  the two. A tart place waits while two run. The agent also waits for a free
  one before it clones, in case a VM started since the coordinator looked.
  Linux VMs do not count. `machine add` gives a Mac at most two tart places;
  more, added with `--places`, take turns.
- **A clone that restarts.** A kernel panic restarts the clone's macOS and
  ends its runner, but `tart exec` can go on waiting. While the job runs, the
  agent reads the clone's boot session every `guestCheckSeconds`, and ends the
  job once it changes. A reading that fails counts for nothing.

### macOS images

A tart place's image is any macOS tart image whose guest agent runs, so
`tart exec` works. Cirrus Labs' images have one. Pull the image on the Mac
before you add it, since a clone of an image not yet pulled waits for the
download:

```sh
tart pull ghcr.io/cirruslabs/macos-sequoia-base:latest     # macOS only
tart pull ghcr.io/cirruslabs/macos-sequoia-xcode:latest    # with Xcode
```

To bake your own tools in, clone an image to a local VM, set it up, and use
the VM's name as the place's image:

```sh
tart clone ghcr.io/cirruslabs/macos-sequoia-base:latest ci-base
tart run ci-base        # sign in as admin / admin, install what jobs need
tart stop ci-base
runner-up machine add mac --ssh you@mac --image ci-base
```

The runner runs as the guest agent's user (`admin` in Cirrus Labs' images) in
`~/actions-runner`, so tools installed for that user are found. Every job
starts from the image as it is: nothing a job changes is kept.

## Borrowing a virtual Mac

While the coordinator is running, borrow a tart place from a pool instead of
holding a place by hand:

```sh
runner-up borrow --pool ru-mac --label "native check" --minutes 30 -- sh -c '
  set -eu
  tart clone my-local-image "$BORROWED_VM"
  tart set "$BORROWED_VM" --cpu "$BORROWED_CPUS" --memory "$BORROWED_MEMORY_MB"
  exec tart run --no-graphics "$BORROWED_VM"
'
```

Replace `my-local-image` with an image already on the Mac. The borrow command
never builds or downloads an image and does not start the VM for you. Your
command starts it under `BORROWED_VM`, the name runner-up reserves and cleans.

- `--pool`, `--label`, `--minutes` and a command are required. Minutes must be
  a positive integer. As with `serve`, use `--config FILE` for another setup.
- The request waits beside jobs under the same order rules. The declared
  minutes supply its runtime estimate; the request's queue time breaks ties.
  It has no repository, pull request labels or workflow, so those rules do
  not give it a match. While waiting, it prints its rank and why it waits.
- Only tart places qualify, never host, Docker, KVM or Windows places.
  The command runs on the coordinator's computer with your working directory,
  environment and standard input/output. For a remote Mac, your command must
  reach that Mac itself; `BORROWED_VM` names the VM there, not a local VM.
- The granted command receives `BORROWED_PLACE`, `BORROWED_VM`,
  `BORROWED_CPUS` and `BORROWED_MEMORY_MB`. Keep the command in the foreground
  until its work finishes; the borrow itself reserves the place for that VM.
- From grant through cleanup, the place reserves its share once and one of
  the Mac's two VM slots. A running VM under `BORROWED_VM` is not counted a
  second time. VMs under other names still count as outside runner-up.
- The borrow ends with the command, when the caller stops or disconnects, or
  after **max(1 hour, declared minutes)** from grant. A coordinator restart
  leaves it running and counted. Command descendants are stopped and the
  agent stops and deletes any VM left under the granted name before the place
  frees. As with jobs, an outright supervisor crash is cleaned at the next
  use of the place or coordinator startup.
- The caller returns the command's exit status, `124` for a time limit, `130`
  for cancellation, or `1` for a supervisor/cleanup failure. `status` names
  active borrows and their labels. Separate `borrow placed` and
  `borrow completed` log entries include wait time and borrowed minutes;
  borrowed time is not recorded as a job runtime estimate.

## Order rules

When a place frees and several jobs it can take are waiting, the rules decide,
in order. Each rule puts the jobs it matches first. The next rule orders each
half, and the oldest job breaks a tie.

| Rule | Puts first |
| --- | --- |
| `pool:NAME` | jobs asking for the pool NAME, which must be one of the setup's pools |
| `label:NAME` | jobs from an open pull request labeled NAME, in the same repository (not from forks), including runs on its branch |
| `waited:DURATION` | jobs that have waited at least DURATION, oldest first |
| `workflow:GLOB,...` | jobs whose top-level workflow file (without `.yml`) matches, such as `release` or `deploy-*` |
| `shortest` | jobs with shorter runtimes learned from earlier successful runs; a job never seen counts as 15 minutes |

For example, `["label:urgent", "workflow:release", "waited:60m", "shortest"]`.

A `pool:` rule matters where places serve several pools. To let short jobs that
a merge waits on go ahead of everything else, give them a pool of their own,
have the same places serve it, and list `pool:` first:
`["pool:quick", "label:urgent", "waited:60m", "shortest"]`.

GitHub picks which of a scale set's jobs a new runner takes. So the order is
exact between pools and repositories, but only close within one pool of one
repository.

## Pull requests from forks

Every runner starts with runner-up's fork guard as its job-started hook. The
guard reads the job's event. It refuses a job from another repository's pull
request, a follow-on run of one (`workflow_run`), and a job whose event it
cannot read. It allows every other job from the repository itself. Set
`allowForkPullRequests` to `true` only if you trust every pull request.

## Trade-offs

- **Docker places share the machine's kernel.** A job that escapes its
  container reaches the machine. Use KVM places for code you trust less.
- **The runner image needs `/home/runner/run.sh`.** Docker places use GitHub's
  runner image, `ghcr.io/actions/actions-runner`, by default. Another image
  must keep the runner at that path.
- **KVM places can reach the machine's loopback.** A KVM place uses QEMU's
  user networking. Through it, a job can reach services the machine listens
  on at 127.0.0.1, at the address 10.0.2.2. Keep such services behind their
  own sign-in on machines with KVM places.
- **Every place but a Docker place downloads the runner.** Each machine
  downloads the runner release once, checks it against a pinned checksum, and
  keeps it; a Hyper-V VM downloads and checks it afresh for each job. The
  version is pinned (2.338.1), and runners never update themselves.
- **Host places are not fresh.** A host place runs its job as the SSH user,
  in a folder made for the job and removed after it, and ends every process
  the job started. Its tool cache is kept for the next job. Everything else
  is the machine's own: the job can read the user's home folder and keychain, change installed programs, and reach the
  network as the machine does. Use host places only for jobs you trust as
  you trust yourself, and keep the fork guard on. A machine gets one host
  place by default.
- **A tart clone sees runner-up.** With the fork guard on, a clone's job can
  read the runner-up program it runs as its guard. It holds no secret.
- **Account places share the machine.** A job account is a standard account
  with no network access to private addresses, but it runs on the machine
  itself, beside its owner. Use Hyper-V places for code you trust less.
- **Account places have only what the machine has.** Jobs see the programs
  installed on the machine, not GitHub's runner image.
- **Hyper-V places talk over a serial port.** The registration and the
  runner's output travel over the VM's second serial port, which Hyper-V
  joins to a named pipe on the machine. Hyper-V may let only administrators
  open that pipe; if a Hyper-V place says it could not open the serial port,
  make the SSH account an administrator.
- **The coordinator does not run on Windows.** Run it on Linux or macOS.
- **One coordinator per pool.** A second coordinator for the same pool waits
  until the first lets go of the scale set session.

## Development

```sh
make build          # Build the binary
make test           # Run tests
make lint           # Run gofmt + go vet
make dist           # Cross-compile for all platforms
make install        # Install to $GOPATH/bin or /usr/local/bin
make clean          # Remove build artifacts
```

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## License

[MIT](LICENSE)
