// runner-up runs GitHub Actions jobs on your own machines through GitHub's
// runner scale sets, one fresh container or virtual machine per job, or on
// the machine itself.
//
// One coordinator holds a scale set session per repository or organization
// and pool, and hears of every waiting job. Jobs wait at GitHub and hold
// nothing. When a place is free, the coordinator picks the job its order
// rules pick (internal/plan), and only then acquires it and starts a runner
// for it in that place: it runs the place's machine's agent over SSH
// (internal/coordinator), which makes the container or virtual machine, runs
// the one job and removes it (internal/agent). A place's share of its
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
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
  status        show each place and the waiting jobs

On each machine (run by the coordinator):
  agent probe|vms|run|clean
  fork-guard    the runner's job-started hook
  version

On a Windows machine:
  setup         set it up for places, as an administrator, once
  service       the Windows service that setup installs

Every command but agent, fork-guard, setup, service and version takes --config FILE;
the default is ` + "%s\n"

// Main runs the runner-up command line named by os.Args.
func Main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, config.DefaultConfigPath())
		os.Exit(2)
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
	case "serve", "status", "check-config", "run-place", "init", "machine":
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
	case "init":
		os.Exit(initMain(os.Args[2:]))
	case "machine":
		if len(os.Args) < 3 || os.Args[2] != "add" {
			fmt.Fprintln(os.Stderr, "usage: runner-up machine add NAME --ssh DESTINATION [flags]")
			os.Exit(2)
		}
		os.Exit(machineAddMain(os.Args[3:]))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	configPath := fs.String("config", config.DefaultConfigPath(), "setup file")
	placeID := fs.String("place", "", "place to run (run-place)")
	fs.Parse(os.Args[2:])
	cfg, err := config.LoadConfig(*configPath)
	if err == nil {
		_, err = plan.ParseOrder(cfg.Order)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(cfg, *configPath))
	case "run-place":
		os.Exit(coordinator.RunPlace(cfg, *placeID))
	case "status":
		os.Exit(status(cfg, os.Stdout))
	case "check-config":
		os.Exit(checkConfig(cfg, os.Stdout))
	}
}

// Leave room inside systemd's default 90-second stop allowance. Session
// acknowledgements and closes each have their own shorter allowance.
const shutdownTimeout = 15 * time.Second

func serve(cfg *config.Config, configPath string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	token := coordinator.GhToken(cfg.Gh())
	return serveWith(cfg, configPath, self, coordinator.GhClients(token), coordinator.NewGitHubAPI(token))
}

// serveWith lets a test serve a local stand-in for GitHub's Actions service,
// with no sign-in and no reads of GitHub's API. self is the program place
// runners run as.
func serveWith(cfg *config.Config, configPath, self string, clients coordinator.ClientFactory, api *coordinator.GithubAPI) int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	launcher := coordinator.NewProcLauncher(cfg, configPath, self, log)
	svc, err := coordinator.NewService(cfg, log, launcher)
	if err != nil {
		log.Error("service", "err", err)
		return 1
	}
	if labels, workflows := plan.Needs(svc.Rules); api != nil {
		if labels {
			svc.LabelReader = api.PullLabels
		}
		if workflows {
			svc.WorkflowReader = api.RunWorkflow
		}
	}
	if cfg.HasTart() {
		svc.VmReader = coordinator.ReadVMs
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cleanLeftovers(ctx, cfg, log)
	log.Info("runner-up starting", "version", version.Version, "targets", len(cfg.Targets()), "pools", len(cfg.Pools),
		"machines", len(cfg.Machines), "places", len(cfg.Places()))
	var listeners sync.WaitGroup
	for _, target := range cfg.Targets() {
		for _, label := range cfg.Labels() {
			listeners.Add(1)
			go func() {
				defer listeners.Done()
				coordinator.RunSet(ctx, svc, log, target, label, clients)
			}()
		}
	}
	listenersDone := make(chan struct{})
	go func() {
		listeners.Wait()
		close(listenersDone)
	}()
	tick := time.NewTicker(cfg.Seconds(cfg.TickSeconds))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Running jobs keep their places: their place runners are in their
			// own sessions and hold their own locks. Waiting jobs stay at GitHub.
			log.Info("runner-up stopping; running jobs carry on")
			return waitForListeners(listenersDone, shutdownTimeout, log)
		case <-tick.C:
		case <-launcher.Exited:
		}
		svc.Tidy(ctx)
		svc.Place(ctx)
	}
}

// waitForListeners joins the listeners, including their deferred session
// closes, but never lets an unreachable endpoint hold shutdown indefinitely.
func waitForListeners(done <-chan struct{}, allowance time.Duration, log *slog.Logger) int {
	timer := time.NewTimer(allowance)
	defer timer.Stop()
	select {
	case <-done:
		log.Info("scale set listeners stopped")
		return 0
	case <-timer.C:
		log.Error("timed out waiting for scale set listeners; sessions may remain active", "allowance", allowance)
		return 1
	}
}

// cleanLeftovers asks each free place's agent to remove anything left of the
// place: a place runner or agent killed outright, or a machine that went down
// mid-job, cannot clean up after itself. Busy places keep theirs. Every
// agent's own run cleans first too, so a machine unreachable now is cleaned
// at its next job.
func cleanLeftovers(ctx context.Context, cfg *config.Config, log *slog.Logger) {
	var wg sync.WaitGroup
	for _, p := range cfg.Places() {
		held, err := lock.HeldBy(filepath.Join(cfg.PlaceDir(p.ID), "busy.lock"))
		if err != nil || held {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			argv := coordinator.AgentCommand(cfg.Machine(p.Machine), "agent", "clean", "--provider", p.Provider, "--place", p.ID)
			out, err := exec.CommandContext(c, argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				log.Warn("could not clean a free place; its next job cleans it", "place", p.ID, "machine", p.Machine,
					"err", agent.FirstLine(string(out), err))
			}
		}()
	}
	wg.Wait()
}

// status prints each place and the waiting jobs, for a person or a check.
func status(cfg *config.Config, w io.Writer) int {
	svc := &coordinator.Service{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, p := range cfg.Places() {
		busy, why := svc.PlaceBusy(p)
		state := "free"
		if busy {
			state = "busy (" + why + ")"
		}
		job := ""
		var run coordinator.PlaceRun
		if why == "job" && config.ReadJSON(filepath.Join(cfg.PlaceDir(p.ID), "place.json"), &run) == nil {
			started := "waiting for its job"
			if run.Started {
				started = "running"
			}
			job = fmt.Sprintf(": %s %q %s", run.Repo, run.JobName, started)
		}
		fmt.Fprintf(w, "place %s on %s (%s, %d cores, %d MB) %s%s\n", p.ID, p.Machine, p.Provider, p.Cores, p.MemoryMB, state, job)
	}
	var st coordinator.SavedState
	if config.ReadJSON(filepath.Join(cfg.StateDir, "state.json"), &st) == nil {
		for _, j := range st.Waiting {
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
		shares = append(shares, fmt.Sprintf("%s %s %d cores %d MB", p.ID, p.Provider, p.Cores, p.MemoryMB))
		total.Cores += p.Cores
		total.MemoryMB += p.MemoryMB
	}
	line := fmt.Sprintf("%s (%s): %s", m.Name, where, strings.Join(shares, ", "))
	if m.Cores == 0 {
		return line + "; nothing counted"
	}
	b := m.Budget()
	fit := "all at once"
	if total.Cores > b.Cores || b.MemoryMB > 0 && total.MemoryMB > b.MemoryMB {
		fit = "not all at once"
	}
	if tart := countProvider(m, config.ProviderTart); tart > agent.MacOSVMLimit {
		fit += fmt.Sprintf("; its %d tart places take turns, since macOS runs at most %d VMs at once", tart, agent.MacOSVMLimit)
	}
	line += fmt.Sprintf("; places may reserve %d of %d cores", b.Cores, m.Cores)
	if m.MemoryMB > 0 {
		line += fmt.Sprintf(" and %d of %d MB", b.MemoryMB, m.MemoryMB)
	}
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
