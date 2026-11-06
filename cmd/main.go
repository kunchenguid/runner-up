// runner-up runs GitHub Actions jobs on your own machines through GitHub's
// runner scale sets, one fresh container or virtual machine per job, or on
// the machine itself.
//
// One coordinator holds a scale set session per repository or organization
// and pool, and hears of every waiting job. Jobs wait at GitHub without a
// runner; the first job waiting only for resources keeps room in the plan.
// When a place is free, the coordinator picks the job its order
// rules pick (internal/plan), and only then acquires it and starts a runner
// for it in that place: it runs the place's machine's agent over SSH
// (internal/coordinator), which makes the container or virtual machine, runs
// the one job and removes it (internal/agent). The job's reservation of its
// machine's cores and memory counts while its runner lives, within the
// machine's budget less the room kept for its owner.
//
// Package cmd parses the command line and starts each of those parts.
package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/cli"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/plan"
	"github.com/kunchenguid/runner-up/internal/proc"
	"github.com/kunchenguid/runner-up/internal/version"
)

const usage = `usage: runner-up <command> [flags]

Setting up:
  init          start a setup: what to serve and the pool label
  machine add   add a machine, reached over SSH, and its places
  check-config  check the setup and say how each machine is shared

Running:
  serve         run the coordinator
  update        fetch the setup's release for a supervisor to start (program/current)
  status        show each place and the waiting jobs
  usage         show finished jobs' sampled use beside their pool sizes
  borrow        borrow a tart place: --pool LABEL --label TEXT --minutes N -- COMMAND
  drain         pause places until named jobs ran there:
                --place ID --job NAME [--repo OWNER/NAME] --label TEXT --minutes N

On each machine (run by the coordinator):
  agent probe            show the machine's cores, memory and providers
  agent vms              list the macOS virtual machines running here
  agent run|clean        run one job in a place, or clean up after one
  agent measure          run a tart guest's runner and sample its use
  agent service-version  show the version of the Windows service here
  agent update-service   hand the Windows service here a signed release
  fork-guard             the runner's job-started hook
  version                show this program's version

On a Windows machine:
  setup         set it up for places, as an administrator, once
  service       the Windows service that setup installs

Every command but agent, fork-guard, setup, service and version takes --config FILE;
the default is ` + "%s\n"

var aboutCommand = map[string]string{
	"fork-guard": "the runner's job-started hook: it stops a job from another repository's pull request",
	"service":    "the Windows service that runner-up setup installs; Windows' service manager starts it",
	"version":    "show this program's version",
}

// Main runs the runner-up command line named by os.Args.
func Main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, config.DefaultConfigPath())
		os.Exit(2)
	}
	// fork-guard, service and version take nothing: they say what they are
	// on -h and refuse anything else.
	switch os.Args[1] {
	case "fork-guard", "service", "version":
		fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		fs.Usage = func() { fmt.Fprintf(fs.Output(), "usage: runner-up %s\n%s\n", os.Args[1], aboutCommand[os.Args[1]]) }
		if code, stop := cli.Parse(fs, os.Args[2:], false); stop {
			os.Exit(code)
		}
	}
	switch os.Args[1] {
	case "agent":
		os.Exit(agent.AgentMain(os.Args[2:]))
	case "fork-guard":
		os.Exit(agent.RunForkGuard(os.Stdout, os.Stderr))
	case "setup":
		os.Exit(agent.SetupMain(os.Args[2:]))
	case "service":
		if err := agent.RunService(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "version":
		fmt.Println(version.Version)
		return
	case "serve", "update", "status", "usage", "check-config", "run-place", "run-borrow", "borrow", "drain", "init", "machine":
		if err := proc.CoordinatorRuns(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	case "-h", "--help", "help":
		fmt.Printf(usage, config.DefaultConfigPath())
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Fprintf(os.Stderr, usage, config.DefaultConfigPath())
		os.Exit(2)
	}
	switch os.Args[1] {
	case "borrow":
		os.Exit(coordinator.BorrowMain(os.Args[2:]))
	case "drain":
		os.Exit(coordinator.DrainMain(os.Args[2:]))
	case "init":
		os.Exit(initMain(os.Args[2:]))
	case "machine":
		switch {
		case cli.Help(os.Args[2:]):
			fmt.Println(machineAddUsage)
			return
		case len(os.Args) < 3 || os.Args[2] != "add":
			fmt.Fprintln(os.Stderr, machineAddUsage)
			os.Exit(2)
		}
		os.Exit(machineAddMain(os.Args[3:]))
	}
	name, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultConfigPath(), "setup file")
	// Place runners and borrows are started by the coordinator, never by a
	// person; only they take these.
	var borrowID, placeID, pool *string
	switch name {
	case "run-place":
		placeID = fs.String("place", "", "place to run")
		pool = fs.String("pool", "", "pool of the job the place runs")
	case "run-borrow":
		placeID = fs.String("place", "", "place to run the borrow in")
		borrowID = fs.String("borrow", "", "borrow request")
	}
	if code, stop := cli.Parse(fs, args, false); stop {
		os.Exit(code)
	}
	cfg, err := config.LoadConfig(*configPath)
	if err == nil {
		_, err = plan.ParseOrder(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(cfg, *configPath))
	case "update":
		os.Exit(updateProgram(cfg, *configPath, os.Stdout, os.Stderr))
	case "run-borrow":
		os.Exit(coordinator.RunBorrow(cfg, *placeID, *borrowID))
	case "run-place":
		os.Exit(coordinator.RunPlace(cfg, *placeID, *pool))
	case "status":
		os.Exit(status(cfg, os.Stdout))
	case "usage":
		os.Exit(usageReport(cfg, os.Stdout, os.Stderr))
	case "check-config":
		os.Exit(checkConfig(cfg, os.Stdout))
	}
}

func serve(cfg *config.Config, configPath string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	token := coordinator.GhToken(cfg.Gh())
	return serveWith(cfg, configPath, self, coordinator.GhClients(token), coordinator.NewGitHubAPI(token), nil)
}

// serveWith runs the coordinator (coordinator.Serve) until a stop signal. It
// lets a test serve a local stand-in for GitHub's Actions service, with no
// sign-in and no reads of GitHub's API. self is the program place runners
// run as.
func serveWith(cfg *config.Config, configPath, self string, clients coordinator.ClientFactory, api *coordinator.GithubAPI, passed func()) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return coordinator.Serve(ctx, cfg, coordinator.ServeOptions{ConfigPath: configPath, Self: self, Clients: clients, API: api,
		Log: slog.New(slog.NewTextHandler(os.Stdout, nil)), Passed: passed})
}

// updateProgram is `runner-up update`: it fetches and checks the setup's
// release and points program/current under the state folder at it, for a
// supervisor to start, as a running coordinator would.
func updateProgram(cfg *config.Config, configPath string, w, errw io.Writer) int {
	if cfg.Release == nil {
		fmt.Fprintln(errw, "the setup names no release; add one (see the README's Updating runner-up)")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := coordinator.NewProgram(cfg.StateDir, configPath, self, version.Version, log)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	current, err := p.Ready(ctx, cfg.Release)
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	fmt.Fprintf(w, "runner-up %s is ready; a supervisor starts it as %s serve --config %s\n", cfg.Release.Version, current, configPath)
	return 0
}

// status prints each place, the drains and the waiting jobs, for a person or
// a check.
func status(cfg *config.Config, w io.Writer) int {
	svc := &coordinator.Service{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var st coordinator.SavedState
	saved := config.ReadJSON(filepath.Join(cfg.StateDir, "state.json"), &st) == nil
	drains := coordinator.Drains(cfg)
	drainedFor := map[string]string{}
	for _, d := range slices.Backward(drains) {
		if d.Phase != coordinator.DrainDone {
			for _, id := range d.Places {
				drainedFor[id] = d.Label
			}
		}
	}
	for _, p := range cfg.Places() {
		busy, why := svc.PlaceBusy(p)
		if len(p.Pools) > 0 {
			busy, why = svc.PlaceBusy(cfg.SizePlace(p, p.Pools[0]))
		}
		if busy {
			for _, pool := range p.Pools {
				if occupied, _ := svc.PlaceBusy(cfg.SizePlace(p, pool)); !occupied {
					busy, why = false, ""
					break
				}
			}
		}
		state := "free"
		if busy {
			state = "busy (" + string(why) + ")"
		} else if since, ok := st.Unreachable[p.Machine]; ok {
			state = "resting (agent unreachable since " + since.Local().Format(time.RFC3339) + ")"
		} else if label, ok := drainedFor[p.ID]; ok {
			state = fmt.Sprintf("drained (for %q)", label)
		}
		job := ""
		var run coordinator.PlaceRun
		if why.Runs() && config.ReadJSON(filepath.Join(cfg.PlaceDir(p.ID), "place.json"), &run) == nil {
			started := "waiting for its job"
			if run.Started {
				started = "running"
			}
			job = fmt.Sprintf(": %s %q %s", run.Repo, run.JobName, started)
			if run.BorrowID != "" {
				job = fmt.Sprintf(": borrow %q since %s", run.JobName, run.LaunchedAt.Local().Format(time.RFC3339))
			}
		}
		if why.Runs() {
			sh := svc.Reservation(p)
			p.Cores, p.MemoryMB = sh.Cores, sh.MemoryMB
		}
		fmt.Fprintf(w, "place %s on %s (%s, %d cores, %d MB) %s%s\n", p.ID, p.Machine, p.Provider, p.Cores, p.MemoryMB, state, job)
	}
	for _, d := range drains {
		jobs := make([]string, len(d.Jobs))
		for i, name := range d.Jobs {
			jobs[i] = strconv.Quote(name)
		}
		if d.Repo != "" {
			jobs[0] = d.Repo + " " + jobs[0]
		}
		phase := "waiting for the coordinator"
		if d.Phase != "" {
			phase = string(d.Phase)
			if d.Detail != "" {
				phase += ": " + d.Detail
			}
		}
		fmt.Fprintf(w, "drain %q of %s for %s since %s: %s\n", d.Label, strings.Join(d.Places, ", "), strings.Join(jobs, ", "),
			d.Queued.Local().Format(time.RFC3339), phase)
	}
	if saved {
		for _, j := range st.Waiting {
			if j.BorrowID != "" {
				fmt.Fprintf(w, "waiting borrow %s %q since %s\n", j.Pool, j.Name, j.QueueTime.Local().Format(time.RFC3339))
				continue
			}
			fmt.Fprintf(w, "waiting %s %s %q since %s\n", j.Repo, j.Pool, j.Name, j.QueueTime.Local().Format(time.RFC3339))
		}
		for _, a := range st.Acquired {
			fmt.Fprintf(w, "starting %s %s %q in %s\n", a.Job.Repo, a.Job.Pool, a.Job.Name, a.Place)
		}
	}
	return 0
}

// checkConfig says the setup is valid and how each machine is shared.
func checkConfig(cfg *config.Config, w io.Writer) int {
	fmt.Fprintf(w, "ok: %d targets, %d pools, %d machines, %d places\n", len(cfg.Targets()), len(cfg.Pools), len(cfg.Machines), len(cfg.Places()))
	for _, m := range cfg.Machines {
		fmt.Fprintln(w, machineLine(m))
	}
	for _, p := range cfg.Pools {
		if p.Cores > 0 {
			fmt.Fprintf(w, "pool %s: each job reserves %d cores, %d MB; overrides place sizes\n", p.Label, p.Cores, p.MemoryMB)
		}
		if levels := cfg.PoolLevels(p.Label); len(levels) > 1 {
			var tried []string
			for _, places := range levels {
				var ids []string
				for _, q := range places {
					ids = append(ids, q.ID)
				}
				tried = append(tried, fmt.Sprintf("priority %d: %s", places[0].Level, strings.Join(ids, ", ")))
			}
			fmt.Fprintf(w, "pool %s tries %s\n", p.Label, strings.Join(tried, "; then "))
		}
	}
	return 0
}

func usageReport(cfg *config.Config, w, errw io.Writer) int {
	records, err := coordinator.ReadUsage(cfg)
	if err != nil {
		fmt.Fprintln(errw, "could not read resource readings:", err)
		return 1
	}
	fmt.Fprintln(w, "Sampled peaks, not guaranteed maxima; CPU is cores, memory is MB.")
	fmt.Fprintln(w, "process-group: a host job's processes, memory as summed RSS; it can miss detached work.")
	fmt.Fprintln(w, "guest: a whole tart guest, read from its kernel.")
	fmt.Fprintln(w, "A guest value above the guest's size cannot be its use and is not shown. Allow headroom before changing a size.")
	for _, p := range cfg.Pools {
		if p.Cores > 0 {
			fmt.Fprintf(w, "pool %s: configured %d cores, %d MB\n", p.Label, p.Cores, p.MemoryMB)
		} else {
			fmt.Fprintf(w, "pool %s: uses each place's size\n", p.Label)
		}
		n := 0
		for _, r := range records {
			if r.Pool != p.Label {
				continue
			}
			n++
			fmt.Fprintf(w, "  %s %s/%s reserved %d cores, %d MB: ", r.StartedAt.Local().Format(time.RFC3339), r.Machine, r.Provider, r.Reservation.Cores, r.Reservation.MemoryMB)
			if r.Usage == nil {
				fmt.Fprintln(w, "readings unavailable")
				continue
			}
			u := r.Usage
			cores, memory := fmt.Sprintf("%.2f cores", u.PeakCores), fmt.Sprintf("%.1f MB", u.PeakMemoryMB)
			// tart sizes a guest to its reservation, so a guest cannot use
			// more; a reading above it is not a measurement of the guest.
			above := false
			if u.Scope == "guest" {
				if c := r.Reservation.Cores; c > 0 && u.PeakCores > float64(c) {
					cores, above = "cores unavailable", true
				}
				if m := r.Reservation.MemoryMB; m > 0 && u.PeakMemoryMB > float64(m) {
					memory, above = "memory unavailable", true
				}
			}
			fmt.Fprintf(w, "peak %s, %s (%s, %d samples, %.1fs", cores, memory, u.Scope, u.Samples, u.Seconds)
			if u.Incomplete {
				fmt.Fprint(w, ", incomplete")
			}
			if above {
				fmt.Fprint(w, "; read above the guest's size")
			}
			fmt.Fprintln(w, ")")
		}
		if n == 0 {
			fmt.Fprintln(w, "  no finished-job readings yet")
		}
	}
	return 0
}

// machineLine says how a machine's budget is shared out, and whether all its
// places fit at once.
func machineLine(m config.Machine) string {
	where := "this machine"
	if !m.Local() {
		where = strings.Join(m.SSH, " ")
	}
	var shares []string
	total := config.Share{}
	for _, p := range m.Places {
		share := fmt.Sprintf("%s %s %d cores %d MB", p.ID, p.Provider, p.Cores, p.MemoryMB)
		if p.Priority != nil || p.Level != 0 {
			share += fmt.Sprintf(" priority %d", p.Level)
		}
		shares = append(shares, share)
		total.Cores += p.Cores
		total.MemoryMB += p.MemoryMB
	}
	line := fmt.Sprintf("%s (%s): %s", m.Name, where, strings.Join(shares, ", "))
	if m.Cores == 0 && m.MemoryMB == 0 {
		return line + "; nothing counted"
	}
	b := m.Budget()
	fit := "all at once"
	if b.Cores > 0 && total.Cores > b.Cores || b.MemoryMB > 0 && total.MemoryMB > b.MemoryMB {
		fit = "not all at once"
	}
	if tart := countProvider(m, config.ProviderTart); tart > agent.MacOSVMLimit {
		fit += fmt.Sprintf("; its %d tart places take turns, since macOS runs at most %d VMs at once", tart, agent.MacOSVMLimit)
	}
	var counted []string
	if m.Cores > 0 {
		counted = append(counted, fmt.Sprintf("%d of %d cores", b.Cores, m.Cores))
	}
	if m.MemoryMB > 0 {
		counted = append(counted, fmt.Sprintf("%d of %d MB", b.MemoryMB, m.MemoryMB))
	}
	line += "; places may reserve " + strings.Join(counted, " and ")
	return line + ", " + fit
}

func countProvider(m config.Machine, provider string) int {
	n := 0
	for _, p := range m.Places {
		if p.Provider == provider {
			n++
		}
	}
	return n
}
