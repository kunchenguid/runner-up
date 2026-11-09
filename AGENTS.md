# runner-up - Agent Guide

runner-up is a Go program that runs GitHub Actions jobs on your own machines: one coordinator holds GitHub runner scale set sessions and starts a one-job runner in a free place on a machine, through that machine's agent over SSH.
User-facing behavior: [README.md](README.md). Each test's intent is in the comment above it - read it before changing what the test covers.

## Layout

- `main.go` calls `cmd.Main`; `cmd/` parses the command line, `init` and `machine add`, and holds the end-to-end tests; `serve` hands off to `coordinator.Serve`
- `internal/coordinator/` - the serve loop (`serve.go`), scale set sessions, placement and the `Service` state split by part (see its doc comment), place runners, the macOS VM count, updating to the setup's release (`program.go`, `update.go`)
- `internal/agent/` - the agent and its providers (`docker.go`, `kvm_linux.go`, `tart.go`, `container.go`, `host.go`, `hyperv*.go`, `account_*.go`), the registration and fork-guard launch they all render (`launch.go`), the fork guard, and the Windows service and job host
- `internal/cli/` (how every command reads its flags), `internal/config/` (the setup file), `internal/plan/` (order rules), `internal/release/` (release archives and their signature), `internal/lock/`, `internal/proc/`, `internal/quote/`, `internal/version/`
- `internal/ciguard/` - tests that guard the CI, release and install files
- `internal/testbin/` builds runner-up once per test binary for tests that run it as a program
- `internal/testrun/` - tests start every program with `testrun.Command` and give every wait a deadline, such as `testrun.Receive` or `testrun.Wait`, so a hang fails with its cause and nothing a test started outlives it

## Commands

- `make build`, `make test` (`go test ./...`), `make lint`
- `GOOS=windows go vet ./...` and `GOOS=linux go vet ./...` before shipping; CI tests on Linux, macOS, and Windows
- `go test -short ./...` skips the tests that run runner-up as a program
- `make test-real-tart` on a Mac with Tart before merging a release PR: the real-Tart tests, which CI skips (CONTRIBUTING.md, "Before a release")
- `nix build .#default` after changing `go.mod` or `go.sum`; `vendorHash` in `package.nix` must match

## Invariants

- A runner's one-time registration travels to its place only on standard input or a VM's serial port, never in a file, a command line, a container's or VM's settings, or any process's environment but the runner's own, where the runner reads `ACTIONS_RUNNER_INPUT_JITCONFIG` and removes it.
- Every runner starts with the fork guard as its job-started hook unless `allowForkPullRequests` is set.
- A place is busy exactly while some process holds its lock; running jobs outlive a coordinator stop.
- Resource reservations: see README.md, "Setup file" and "Borrowing a virtual Mac".
- The coordinator stores no token; it asks `gh auth token` for each session, and again when GitHub refuses the session's token.
- The Windows service installs only a release whose `checksums.txt` the release signing key signed; its public half is in `internal/release/key.go`, its private half only in the `release` environment's secret.
- Recorded GitHub payloads in `testdata/` are renamed; keep them free of real repository, account and machine names.

## Windows compatibility

- The agent runs on Windows; the coordinator runs only on Linux and macOS. Every package must still build for Windows.
- Isolate OS-specific code behind `_unix.go`/`_windows.go`/`_other.go` files (see `internal/lock/`, `internal/proc/`).
- Command lines run over SSH on Windows must read the same in cmd and PowerShell (`internal/quote`).

## Contribution gate

- PRs to `main` must be raised through no-mistakes and pass CI (required checks `PR must be raised via no-mistakes` and `CI passed`); see [CONTRIBUTING.md](CONTRIBUTING.md). Release PRs get both stamped by `release.yml`.
- A workflow backing a required check must never use `paths`/`paths-ignore`. The gate action pin and `.github/scripts/no-mistakes-gate.sh` are load-bearing, and the tests in `internal/ciguard` guard them.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
