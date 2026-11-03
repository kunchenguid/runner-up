# Vision

`runner-up` exists so that the spare machines a person already owns - a Linux box, a Mac, a Windows PC - run their GitHub Actions jobs, each in a fresh place, with nothing more than SSH and a GitHub sign-in.
It serves the owner of a handful of machines of different kinds, and it turns GitHub's line of waiting jobs into one-job runners started where they fit best.
It owns exactly one thing: deciding which waiting job runs in which free place on the owner's machines, and making that place fresh for it.

## One job, one fresh place

Every job gets a place made for it and removed after it: a container, a virtual machine or an account.
A place kind that cannot be fresh, such as a host place, ships only with its exposure written beside it in plain words.
Every trade-off of a place kind - a shared kernel, a reachable loopback, a shared machine - is stated in the README, not left for a user to discover.
Jobs from forks' pull requests are refused by default on every provider, and allowing them is one explicit setting.
A runner's registration travels to its place only on standard input or a serial port, never in a file, an argument, an image or a container's or virtual machine's settings.
A new provider earns its place by meeting these rules exactly as every existing provider does.

## A running job is never the price of a change

A job that has started runs to its end: a coordinator restart, a setup change, an update or a down machine never stops it.
Stopping a running job happens only by the owner's explicit, opt-in choice, such as a drain told to stop what still runs after a grace period, never by default and never by runner-up's own judgment.
Changes apply to the next job a place takes, and a running job keeps the setup and reservation it started with.
GitHub owns a job's fate: runner-up removes a runner from its scale set before it stops the runner, and a runner GitHub says is busy keeps its job.
Rerunning a failed job is left to GitHub and the repository, even when the place under it died.
When runner-up cannot know what GitHub or a machine is doing, it waits and asks again instead of guessing.
A machine that cannot be reached rests instead of churning registrations, and takes jobs again once its agent answers.

## The owner's machines stay the owner's

A machine keeps room for its owner, and places share only what is left.
The owner's idle resources are not runner-up's to take: anything that holds cores, memory or disk while no job needs it, such as a warm place or a cache server, is an opt-in setting and never on by default.
runner-up counts what the owner runs by hand, such as macOS virtual machines it did not start, and gives way to it.
It runs the virtualization tools and images the owner installs and keeps up to date, and never bundles, builds or refreshes them.
Readings of what jobs use are advisory: they never change a size on their own, and a missing or impossible reading shows as unavailable, never as zero.
Sizes, room and order are the owner's choices in the setup, and runner-up does not override them from its own measurements.
The owner can watch runner-up from the monitoring they already run: status and usage on its command line, and metrics a collector can read.

## One line across every machine

Waiting jobs from every repository the setup serves stand in one line, ordered by the setup's rules.
A job is acquired only when a place for it is free, so until then it waits at GitHub holding nothing.
The first job held back only by cores or memory keeps room on one machine, so smaller jobs cannot starve it.
Order is exact where runner-up decides, and documented as only close where GitHub decides, within one pool of one repository.
The owner's own commands may wait in the same line for any kind of place, as a borrow does for a virtual Mac, instead of bypassing it.

## The setup file is the contract

One JSON setup file names the repositories, pools, machines, places, order rules and the release to run, and it may live in a checkout.
A setup or command that would silently do less than it says - a rule naming no pool, a size that never fits, an argument nobody reads - is refused with the reason.
A field runner-up no longer honors is refused like any other mistake; old setups are not kept loading with warnings or shims.
A refused setup never reaches a job: the running setup carries on until the file is fixed.
The coordinator signs in only through the GitHub command line and stores no token, App key or token file of its own, and machines hold no GitHub credential at all.

## Trust is checked, never assumed

Every program runner-up installs is checked against a SHA-256 that the setup or the release names before it runs.
Anything that runs as SYSTEM takes only a release the release signing key signed.
A new version runs on trial, and one that stops within a minute is refused and replaced by the version before it.
Every privilege runner-up asks of an operating system is the least the job needs.

## Scope

runner-up is not a CI system, not a Kubernetes operator, not a cloud autoscaler, not a provisioning tool and not a hosted service.
It serves one owner's repositories on that owner's machines; it is not a network that pools or lends capacity between owners.
It does not create machines; it uses the ones the owner adds, owned or rented.
It serves GitHub Actions through runner scale sets, and its order is only as exact as that contract allows.
A place kind or feature not yet proven with real jobs is marked experimental until it is.
Coordination beyond placing jobs - rules that stop jobs for others, commands taken over the network - is not against this vision, but it is built only when real users clearly ask for it, never ahead of that demand.
This repository holds itself to the gate it asks of others: every pull request passes no-mistakes and CI, and recorded GitHub payloads in its tests carry no real names.

A change aligns when it runs more of the owner's jobs on the owner's machines in fresh places, makes the line fairer or more exact, carries running jobs safely through one more kind of change or failure, refuses one more silent misconfiguration, lets the owner see more of what runner-up sees, or states one more trade-off plainly.
A change should be resisted when it stops or strands a running job without the owner's explicit choice, puts a registration or token where it can be read, spends the owner's idle resources by default, decides for the owner or for GitHub from guesses or readings, needs infrastructure beyond SSH and the GitHub sign-in, shares capacity beyond one owner, adds coordination no real user has asked for, or grows runner-up into a CI system, a provisioning tool or a hosted service.
