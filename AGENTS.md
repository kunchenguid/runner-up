# runner-up - Agent Guide

runner-up is a Go program that runs GitHub Actions jobs on your own machines: one coordinator holds GitHub runner scale set sessions and starts a one-job runner in a free place on a machine, through that machine's agent over SSH.
User-facing behavior: [README.md](README.md). Each test's intent is in the comment above it - read it before changing what the test covers.

## Layout

- `main.go` calls `cmd.Main`; `cmd/` parses the command line, `init` and `machine add`, and holds the end-to-end tests
- `internal/coordinator/` - scale set sessions, the planner, place runners, the macOS VM count
- `internal/agent/` - the agent and its providers (`docker.go`, `kvm_linux.go`, `tart.go`, `host.go`, `hyperv*.go`, `account_*.go`), the fork guard, and the Windows service and job host
- `internal/config/` (the setup file), `internal/plan/` (order rules), `internal/lock/`, `internal/proc/`, `internal/quote/`, `internal/version/`
- `internal/testbin/` builds runner-up once per test binary for tests that run it as a program

## Commands

- `make build`, `make test` (`go test ./...`), `make lint`
- `GOOS=windows go vet ./...` and `GOOS=linux go vet ./...` before shipping; CI tests on Linux, macOS, and Windows
- `go test -short ./...` skips the tests that run runner-up as a program
- `nix build .#default` after changing `go.mod` or `go.sum`; `vendorHash` in `package.nix` must match

## Invariants

- A runner's one-time registration reaches it only on standard input or a VM's serial port, never in a file, a command line or an environment variable.
- Every runner starts with the fork guard as its job-started hook unless `allowForkPullRequests` is set.
- A place is busy exactly while some process holds its lock; running jobs outlive a coordinator stop.
- A place's share of its machine's cores and memory counts while its runner lives, within the machine's budget less the owner's room.
- The coordinator stores no token; it asks `gh auth token` for each session.
- Recorded GitHub payloads in `testdata/` are renamed; keep them free of real repository, account and machine names.

## Windows compatibility

- The agent runs on Windows; the coordinator runs only on Linux and macOS. Every package must still build for Windows.
- Isolate OS-specific code behind `_unix.go`/`_windows.go`/`_other.go` files (see `internal/lock/`, `internal/proc/`).
- Command lines run over SSH on Windows must read the same in cmd and PowerShell (`internal/quote`).

## Contribution gate

- PRs to `main` must be raised through no-mistakes (required check `PR must be raised via no-mistakes`); see [CONTRIBUTING.md](CONTRIBUTING.md).
- A workflow backing a required check must never use `paths`/`paths-ignore`. The gate action pin and `.github/scripts/no-mistakes-gate.sh` are load-bearing, and `no_mistakes_gate_test.go` and `release_ci_exclusions_test.go` guard them.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
