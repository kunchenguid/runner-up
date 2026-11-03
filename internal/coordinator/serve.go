package coordinator

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
	"github.com/kunchenguid/runner-up/internal/version"
)

// ShutdownTimeout leaves room inside systemd's default 90-second stop
// allowance. Session acknowledgements and closes each have their own shorter
// allowance.
const ShutdownTimeout = 15 * time.Second

// ServeOptions are what Serve needs beside the setup.
type ServeOptions struct {
	// ConfigPath is the setup file, applied in place as it changes (setup.go).
	ConfigPath string
	// Self is the program place runners run as.
	Self string
	// Clients makes each scale set's client; API reads GitHub's API, or is
	// nil to read nothing there.
	Clients ClientFactory
	API     *GithubAPI
	Log     *slog.Logger
}

// Serve runs the coordinator under cfg until ctx ends, and returns the
// program's exit code. It opens a scale set session for each repository or
// organization and pool, and runs one loop that alone places jobs and tidies
// places, with ctx's serving context: at every tick, message, ended place
// runner, setup change and agent update. When the setup names a release, it
// runs that version of runner-up and copies it to every machine (program.go,
// update.go); it restarts as a new version by replacing itself, and returns
// only if that fails.
func Serve(ctx context.Context, cfg *config.Config, o ServeOptions) int {
	log := o.Log
	program, err := os.Executable()
	if err != nil {
		log.Error("could not find runner-up's own program", "err", err)
		return 1
	}
	up := NewUpdater(NewProgram(cfg.StateDir, o.ConfigPath, program, version.Version, log), o.Self, log)
	// A new version that stopped on trial goes back to the version before.
	if r := up.Program.Begin(); r != nil {
		return restart(up.Program, r, log)
	}
	base := cfg
	cfg = up.Setup(base)
	launcher := NewProcLauncher(cfg, o.Self, log)
	svc, err := NewService(cfg, log, launcher)
	if err != nil {
		log.Error("service", "err", err)
		return 1
	}
	up.Service = svc
	// Each reader is used only while the setup needs it: label and workflow
	// reads while an order rule does, VM reads of the Macs with tart places.
	if o.API != nil {
		svc.RunReader = o.API.RunClosed
		svc.LabelReader = o.API.PullLabels
		svc.WorkflowReader = o.API.RunWorkflow
	}
	svc.VmReader = ReadVMs
	// serving ends when ctx does, or to restart as a new version.
	serving, stopServing := context.WithCancel(ctx)
	defer stopServing()
	cleanLeftovers(serving, cfg, log)
	log.Info("runner-up starting", "version", version.Version, "targets", len(cfg.Targets()), "pools", len(cfg.Pools),
		"machines", len(cfg.Machines), "places", len(cfg.Places()))
	sessions := NewSessions(serving, svc, log, o.Clients)
	sessions.Match(cfg)
	setups := WatchSetup(serving, o.ConfigPath, SetupPoll, SetupQuiet)
	up.Tick(serving, base)
	var next *Restart
	refused := false
	tick := time.NewTicker(cfg.Seconds(cfg.TickSeconds))
	defer tick.Stop()
	for {
		select {
		case <-serving.Done():
			// Running jobs keep their places: their place runners are in their
			// own sessions and hold their own locks. Waiting jobs stay at GitHub.
			if next == nil {
				log.Info("runner-up stopping; running jobs carry on")
				up.Program.Stopped()
				return waitForSessions(sessions.Done(), ShutdownTimeout, log)
			}
			log.Info("runner-up restarting as a new version; running jobs carry on", "version", next.Version)
			waitForSessions(sessions.Done(), ShutdownTimeout, log)
			return restart(up.Program, next, log)
		case r := <-up.Ready:
			next = r
			stopServing()
			continue
		case <-tick.C:
			up.Program.Keep()
		case <-svc.Wakes():
		case <-launcher.Exited:
		case <-up.Changed:
			// A machine's agent changed: place runners start with it from now.
			rules, err := plan.ParseOrder(base)
			if err == nil {
				_, err = svc.Reload(up.Setup(base), rules)
			}
			if err != nil {
				log.Error("could not use a machine's new agent", "err", err)
			}
			cfg = svc.Setup()
		case read := <-setups:
			nextBase, err := applySetup(svc, sessions, up, read)
			switch {
			case err != nil:
				log.Error("the setup file was not applied; keeping the running setup", "err", err)
				refused = true
				continue
			case nextBase != nil:
				if nextBase.TickSeconds != cfg.TickSeconds {
					tick.Reset(nextBase.Seconds(nextBase.TickSeconds))
				}
				base, cfg = nextBase, svc.Setup()
			case refused:
				log.Info("the setup file holds the running setup again")
			}
			refused = false
		}
		svc.Tidy(serving)
		svc.Place(serving)
		up.Tick(serving, base)
	}
}

// restart replaces the coordinator with r's program. It returns only when
// that could not be done, to stop, and let the coordinator's supervisor start
// it again.
func restart(p *Program, r *Restart, log *slog.Logger) int {
	log.Info("runner-up runs another version", "version", r.Version, "path", r.Path)
	err := p.Exec(r)
	log.Error("could not run the other version; runner-up stops for its supervisor to start it again", "version", r.Version, "err", err)
	return 1
}

// applySetup checks what the setup file holds, as check-config does, and
// applies it in place, with each machine's agent the release's (Updater).
// It returns the setup file's setup when it applied it, nil when the file
// holds the running setup, or why it kept the running setup.
func applySetup(svc *Service, sessions *Sessions, up *Updater, read SetupRead) (*config.Config, error) {
	if read.Err != nil {
		return nil, read.Err
	}
	next, rules, err := CheckSetup(read.Data)
	if err != nil {
		return nil, err
	}
	changed, err := svc.Reload(up.Setup(next), rules)
	if err != nil || !changed {
		return nil, err
	}
	sessions.Match(next)
	return next, nil
}

// waitForSessions joins the scale set sessions, including their deferred
// closes, but never lets an unreachable endpoint hold shutdown indefinitely.
func waitForSessions(done <-chan struct{}, allowance time.Duration, log *slog.Logger) int {
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
			argv := AgentCommand(cfg.Machine(p.Machine), "agent", "clean", "--provider", p.Provider, "--place", p.ID)
			out, err := exec.CommandContext(c, argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				log.Warn("could not clean a free place; its next job cleans it", "place", p.ID, "machine", p.Machine,
					"err", agent.FirstLine(string(out), err))
			}
		}()
	}
	wg.Wait()
}
