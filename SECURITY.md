# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/kunchenguid/runner-up/security/advisories/new)
for this repository. Do not open a public issue for a security problem.
Include the version, how you run runner-up, and the steps to reproduce.

## What to know when you run runner-up

- **The Windows service runs as SYSTEM.** It makes and deletes job accounts and
  installs updates. Anyone who can make it install a program can run code as
  SYSTEM on that machine.
- **The release signing key controls that service.** The service installs only a
  release whose `checksums.txt` the release signing key signed. The public half
  is in `internal/release/key.go`; the private half lives only in the `release`
  environment's secret. If you suspect the key leaked, report it as above.
- **A runner's one-time registration** reaches it only through paths documented
  in `AGENTS.md` and the README; the coordinator stores no GitHub token and asks
  `gh auth token` for each session.
- **Pull requests from forks** are blocked from running jobs by the fork guard
  unless the setup file sets `allowForkPullRequests`.

## Supported versions

Only the latest release receives security fixes.
