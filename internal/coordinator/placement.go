package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Place gives every free place to the job the rules pick, until nothing more
// fits. It starts a runner for a job only once its place is chosen, and
// acquires a job GitHub offered only then. The first-ranked job waiting only
// for resources keeps room in the plan, without acquiring a runner or lock.
// It plans under mu, and acquires the job, registers its runner and launches
// it with mu let go (assignLocked). ctx is the serving context: placement
// carries on while one scale set's session restarts.
func (s *Service) Place(ctx context.Context) {
	s.pass.Lock()
	defer s.pass.Unlock()
	s.refreshLabels(ctx)
	s.refreshRunWorkflows(ctx)
	s.forgetClosedRuns(ctx)
	s.refreshVMs(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askRestingLocked(ctx)
	s.expireLocked()
	s.refreshDrainsLocked()
	s.refreshBorrowsLocked()
	for range len(s.Cfg.Places()) + 1 {
		cfg, v := s.Cfg, s.snapshotLocked()
		assignments := plan.Plan(s.waitingLocked(), v.views(), v.headroom(), s.Rules, s.now())
		if len(assignments) == 0 {
			break
		}
		progressed := false
		for _, a := range assignments {
			// A setup change applied while a runner started makes the rest
			// of this plan stale: plan again.
			if s.Cfg != cfg {
				progressed = true
				break
			}
			if s.assignLocked(ctx, v, a) {
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	s.advanceDrainsLocked()
	s.noteWasteLocked()
	s.logWaitsLocked()
	s.saveLocked()
}

// assignLocked gives place a.Place to job a.Job, and reports whether that
// changed anything worth planning again for. It is called with mu held, and
// takes the place's lock and the job out of line before it lets go of mu to
// acquire the job, register the job's runner and launch it, so nothing done
// meanwhile places the job or the place twice. It takes mu again to record
// what came of it.
func (s *Service) assignLocked(ctx context.Context, v *snapshot, a plan.Assignment) bool {
	p := a.Place
	j, ok := s.waiting[a.Job.Key()]
	// The job may have started, ended or been taken back while the runner
	// before it in the plan started.
	if !ok || j.TakenBack && j.BorrowID == "" {
		return false
	}
	if _, ok := s.sets[setKey(j.Target, j.Pool)]; !ok && j.BorrowID == "" {
		return false
	}
	// A place given in the same plan as another may have to wait for it now.
	if busy, _ := v.busy(p, true); busy {
		return false
	}
	busyLock, err := lock.Busy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock"))
	if err != nil || busyLock == nil {
		return false
	}
	defer busyLock.Close()
	v.forget(p.ID)
	// This place's last run may have ended since the last Tidy, and its
	// place.json is about to be replaced. That run may have found the machine
	// unreachable.
	s.retireLocked(p)
	if s.restingLocked(p.Machine) {
		return true
	}
	if j.BorrowID != "" {
		return s.assignBorrowLocked(p, a.Job, busyLock)
	}
	set, done := s.setLocked(j.Target, j.Pool)
	defer done()
	k, now, cfg := j.Key(), s.now(), s.Cfg
	delete(s.waiting, k)
	s.acquired[k] = acquired{Job: j, Place: p.ID, At: now}
	ours := func() bool { a, ok := s.acquired[k]; return ok && a.Place == p.ID && a.At.Equal(now) }

	s.mu.Unlock()
	name, err := s.start(ctx, cfg, set, p, j, now, busyLock)
	s.mu.Lock()

	switch {
	case errors.Is(err, errAcquire):
		// GitHub did not answer: the job waits on, in line from where it was.
		if a, ok := s.acquired[k]; ok && ours() {
			delete(s.acquired, k)
			s.waiting[k] = a.Job
		}
		s.Log.Error("acquire failed", "repo", j.Repo, "job", j.Name, "err", err)
		return false
	case errors.Is(err, errGone):
		// Taken, cancelled or expired at GitHub while it waited here.
		if ours() {
			delete(s.acquired, k)
		}
		s.Log.Info("job no longer available", "repo", j.Repo, "job", j.Name, "request", j.RequestID)
		return true
	case err != nil:
		// GitHub gives back an acquired job no runner took.
		s.Log.Error("could not start a runner; the place rests a minute", "place", p.ID, "repo", j.Repo, "job", j.Name, "err", err)
		s.cooling[p.ID] = now.Add(time.Minute)
		return true
	}
	s.Log.Info("placed", "place", p.ID, "machine", p.Machine, "repo", j.Repo, "pool", j.Pool, "job", j.Name,
		"request", j.RequestID, "runner", name, "waited", now.Sub(j.QueueTime).Round(time.Second).String())
	delete(s.wasted, p.ID)
	return true
}

var (
	errAcquire = errors.New("could not acquire the job")
	errGone    = errors.New("the job is no longer available")
)

// start acquires job j for place p, unless GitHub assigned it to the scale
// set already, registers a runner for it and launches the runner with the
// place's lock. It is called with mu let go, and names the runner.
func (s *Service) start(ctx context.Context, cfg *config.Config, set SetClient, p config.Place, j plan.Job, at time.Time,
	busyLock *os.File) (string, error) {
	if !j.Assigned {
		got, err := set.AcquireJobs(ctx, []int64{j.RequestID})
		if err != nil {
			return "", fmt.Errorf("%w: %w", errAcquire, err)
		}
		if !slices.Contains(got, j.RequestID) {
			return "", errGone
		}
	}
	name := fmt.Sprintf("runner-up-%s-%d", p.ID, at.Unix())
	jit, runnerID, err := set.GenerateJIT(ctx, name, p.RunnerWorkFolder())
	if err != nil {
		return name, err
	}
	os.Remove(filepath.Join(cfg.PlaceDir(p.ID), "usage.json"))
	run := PlaceRun{RunnerName: name, RunnerID: runnerID, Target: j.Target, Pool: j.Pool, Repo: j.Repo,
		RequestID: j.RequestID, JobName: j.Name, LaunchedAt: at, Reservation: share(p)}
	if err := writePlaceRun(cfg, p.ID, run); err != nil {
		return name, err
	}
	return name, s.launcher.Launch(p, j.Pool, busyLock, jit)
}
