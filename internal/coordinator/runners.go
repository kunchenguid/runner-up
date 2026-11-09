package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// runners is what the coordinator owes the runners it started and their
// places, beyond what each place's lock and place.json say.
type runners struct {
	// unused are runners that ended before a job reached them, by unusedKey:
	// each is still registered with its scale set until Tidy removes it.
	unused map[string]PlaceRun
	// givenUp are the runners the coordinator gave up on that GitHub has
	// answered for, by name (giveUp): true once GitHub removed the runner and
	// its place is stopping, false when GitHub said it runs a job.
	givenUp map[string]bool
	// cooling are the places resting after a failed launch, until when.
	cooling map[string]time.Time
	// failedStarts are the places resting after their runner failed to start,
	// by id (failedstart.go).
	failedStarts map[string]PlaceRest
	// finished is when GitHub said each runner's job finished, by runner
	// name (finished.go).
	finished map[string]time.Time
	// stopping are the runners whose place the coordinator stopped because
	// they outlived their finished job, by name (finished.go).
	stopping map[string]bool
	// wasted is when each place was last logged free beside a job that fits
	// it (noteWasteLocked).
	wasted map[string]time.Time
	// departed are the places a setup change removed, or moved to another
	// machine, while their runner ran, by id, as the setup had them where the
	// runner runs (reload.go).
	departed map[string]config.Place
	// held are the places that take no new job, by id, and why: account
	// places while their machine's service updates (update.go).
	held map[string]string
}

func newRunners() runners {
	return runners{
		unused:       map[string]PlaceRun{},
		givenUp:      map[string]bool{},
		cooling:      map[string]time.Time{},
		failedStarts: map[string]PlaceRest{},
		finished:     map[string]time.Time{},
		stopping:     map[string]bool{},
		wasted:       map[string]time.Time{},
		departed:     map[string]config.Place{},
		held:         map[string]string{},
	}
}

func unusedKey(run PlaceRun) string { return fmt.Sprintf("%s|%d", run.Target, run.RunnerID) }

// Hold keeps places from taking new jobs, for why, until Unhold. Jobs they
// run carry on.
func (s *Service) Hold(ids []string, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = map[string]string{}
	}
	for _, id := range ids {
		s.held[id] = why
	}
	s.Log.Info("places take no new jobs for now", "places", strings.Join(ids, ","), "why", why)
}

// Unhold lets places Hold held take jobs again.
func (s *Service) Unhold(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.held, id)
	}
	s.Log.Info("places take jobs again", "places", strings.Join(ids, ","))
}

// Tidy runs on every pass of the serve loop: it forgets the last run of
// every free place, removes the registration of every runner that ended
// without a job, gives up on every runner no job reached in time and every
// spare runner still to go, and stops every runner that outlived its finished
// job (finished.go). It calls GitHub and stops places with mu let go.
func (s *Service) Tidy(ctx context.Context) {
	s.pass.Lock()
	defer s.pass.Unlock()
	s.mu.Lock()
	for _, p := range s.placesLocked() {
		if held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock")); err == nil && !held {
			s.retireLocked(p)
		}
	}
	unused := s.unusedLocked()
	s.mu.Unlock()
	removed := s.removeUnused(ctx, unused)
	s.mu.Lock()
	for _, k := range removed {
		delete(s.unused, k)
	}
	if len(removed) > 0 {
		s.saveLocked()
	}
	s.mu.Unlock()
	s.giveUpIdle(ctx, false)
	s.endFinished()
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.now().Add(-time.Duration(s.Cfg.LogKeepDays) * 24 * time.Hour)
	pruneLogs(s.Cfg.LogsDir(), keep)
	pruneLogs(usageDir(s.Cfg), keep)
}

// removal is a runner to remove from its scale set, through set, which the
// remover lets go of with done.
type removal struct {
	key  string
	run  PlaceRun
	set  SetClient
	done func()
}

// unusedLocked lists the unused runners whose scale set has a live session;
// once the others' has, a later Tidy removes them.
func (s *Service) unusedLocked() []removal {
	var out []removal
	for k, run := range s.unused {
		if set, done := s.setLocked(run.Target, run.Pool); set != nil {
			out = append(out, removal{key: k, run: run, set: set, done: done})
		}
	}
	return out
}

// removeUnused removes unused runners from their scale sets, and names those
// GitHub no longer has.
func (s *Service) removeUnused(ctx context.Context, unused []removal) (removed []string) {
	for _, r := range unused {
		err := r.set.RemoveRunner(ctx, int64(r.run.RunnerID))
		r.done()
		if errors.Is(err, scaleset.JobStillRunningError) {
			// Its place has ended, yet GitHub gave the runner a job: no runner
			// is left to run it, and GitHub fails it once it stops hearing
			// from the runner.
			s.Log.Error("a runner whose place has ended is running a job at GitHub; the job is stranded", "runner", r.run.RunnerName,
				"target", r.run.Target, "pool", r.run.Pool)
			continue
		}
		if err != nil && !runnerGone(err) {
			s.Log.Warn("could not remove an unused runner; will retry", "runner", r.run.RunnerName, "err", err)
			continue
		}
		s.Log.Info("removed an unused runner", "runner", r.run.RunnerName)
		removed = append(removed, r.key)
	}
	return removed
}

// runnerGone says a removal found no such runner at GitHub: GitHub named
// the runner missing, or answered 404 Not Found.
func runnerGone(err error) bool {
	return errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
}

// idleRunner is a place's waiting runner to give up on, and why. spare names
// the scale set whose spare runner it is, if it is one. set is its scale
// set's client, nil when its session is closed, which giveUp lets go of with
// done.
type idleRunner struct {
	place string
	run   PlaceRun
	why   string
	spare *setName
	set   SetClient
	done  func()
}

// giveUpAnswer is what came of giving up on an idle runner: whether GitHub
// answered, and if so whether the runner was removed (true) or runs a job
// (false).
type giveUpAnswer struct {
	answered, removed bool
}

// giveUpIdle gives up on the idle runners (idleLocked), calling GitHub and
// stopping places with mu let go; spareOnly leaves the runners past the
// start timeout for Tidy. It is called with pass held.
func (s *Service) giveUpIdle(ctx context.Context, spareOnly bool) {
	s.mu.Lock()
	idle := s.idleLocked(spareOnly)
	s.mu.Unlock()
	answers := s.giveUp(ctx, idle)
	s.mu.Lock()
	s.recordGiveUpsLocked(idle, answers)
	s.mu.Unlock()
}

// idleLocked lists the waiting runners to give up on: every runner that has
// waited the start timeout with no job, unless spareOnly, and the spare runners of each scale
// set a job of which was cancelled before a runner took it. A set has then
// more runners waiting for a job than jobs on their way, and its newest
// waiting runners would otherwise hold their places until the start timeout.
// A runner given up on before, which is stopping or which GitHub says is
// running a job, is no longer waiting.
func (s *Service) idleLocked(spareOnly bool) []idleRunner {
	var out []idleRunner
	picked := map[string]bool{}
	add := func(place string, run PlaceRun, why string, spare *setName) {
		r := idleRunner{place: place, run: run, why: why, spare: spare, done: func() {}}
		if run.RunnerID > 0 {
			r.set, r.done = s.setLocked(run.Target, run.Pool)
		}
		picked[run.RunnerName] = true
		out = append(out, r)
	}
	type waitingRunner struct {
		place string
		run   PlaceRun
	}
	var waiting []waitingRunner
	for _, p := range s.placesLocked() {
		dir := s.Cfg.PlaceDir(p.ID)
		if held, err := lock.HeldBy(filepath.Join(dir, "busy.lock")); err != nil || !held {
			continue
		}
		run, ok := readPlaceRun(s.Cfg, p.ID)
		if !ok || run.BorrowID != "" || run.Started {
			continue
		}
		if _, answered := s.givenUp[run.RunnerName]; !answered {
			waiting = append(waiting, waitingRunner{p.ID, run})
		}
	}
	limit, now := s.Cfg.Seconds(s.Cfg.StartTimeoutSeconds), s.now()
	for _, w := range waiting {
		if !spareOnly && now.Sub(w.run.LaunchedAt) >= limit {
			add(w.place, w.run, "no job reached the runner in time", nil)
		}
	}
	for _, set := range sortedSets(s.spare) {
		pending := 0
		for _, a := range s.acquired {
			if a.Job.Target == set.target && a.Job.Pool == set.pool {
				pending++
			}
		}
		var idles []waitingRunner
		for _, w := range waiting {
			if w.run.Target == set.target && w.run.Pool == set.pool && !picked[w.run.RunnerName] {
				idles = append(idles, w)
			}
		}
		sort.Slice(idles, func(a, b int) bool { return idles[a].run.LaunchedAt.After(idles[b].run.LaunchedAt) })
		spare := set
		for _, w := range idles[:max(len(idles)-pending, 0)] {
			add(w.place, w.run, "the runner's job was cancelled before it started", &spare)
		}
	}
	return out
}

func sortedSets(sets map[setName]bool) []setName {
	out := make([]setName, 0, len(sets))
	for set := range sets {
		out = append(out, set)
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].target < out[b].target || out[a].target == out[b].target && out[a].pool < out[b].pool
	})
	return out
}

// giveUp gives up on each idle runner. GitHub can give a scale set's waiting
// runner a job at any moment until the runner is removed, and a runner
// stopped first would leave that job to fail when GitHub stops hearing from
// it. So the runner's registration goes first, and its place is stopped only
// once GitHub has accepted that. A runner GitHub will not remove because it
// is running a job keeps running it. A runner whose removal GitHub did not
// answer, or whose scale set has no live session, keeps its place, to be
// asked about again at a later tick.
func (s *Service) giveUp(ctx context.Context, idle []idleRunner) []giveUpAnswer {
	answers := make([]giveUpAnswer, len(idle))
	for i, r := range idle {
		answers[i] = s.giveUpOne(ctx, r)
		r.done()
	}
	return answers
}

func (s *Service) giveUpOne(ctx context.Context, r idleRunner) giveUpAnswer {
	if r.run.RunnerID > 0 {
		if r.set == nil {
			return giveUpAnswer{}
		}
		err := r.set.RemoveRunner(ctx, int64(r.run.RunnerID))
		if errors.Is(err, scaleset.JobStillRunningError) {
			s.Log.Info(r.why+", but GitHub says it is running a job; keeping it", "place", r.place, "runner", r.run.RunnerName)
			return giveUpAnswer{answered: true}
		}
		if err != nil && !runnerGone(err) {
			s.Log.Warn(r.why+"; could not remove it from its scale set, so it keeps its place until GitHub answers",
				"place", r.place, "runner", r.run.RunnerName, "err", err)
			return giveUpAnswer{}
		}
	}
	s.Log.Info(r.why+"; removed it from its scale set, stopping it", "place", r.place, "runner", r.run.RunnerName)
	if err := s.launcher.Stop(r.place); err != nil {
		s.Log.Warn("stop failed", "place", r.place, "err", err)
		return giveUpAnswer{}
	}
	return giveUpAnswer{answered: true, removed: true}
}

// recordGiveUpsLocked keeps what GitHub answered for each runner given up
// on. A scale set's spare runners are all gone once GitHub answered for
// each; while a removal it did not answer leaves one, the next Tidy asks
// again.
func (s *Service) recordGiveUpsLocked(idle []idleRunner, answers []giveUpAnswer) {
	unanswered := map[setName]bool{}
	for i, r := range idle {
		if a := answers[i]; a.answered {
			s.givenUp[r.run.RunnerName] = a.removed
		} else if r.spare != nil {
			unanswered[*r.spare] = true
		}
	}
	for set := range s.spare {
		if !unanswered[set] {
			delete(s.spare, set)
		}
	}
}

// retireLocked forgets the last run of a free place, which Tidy or the next
// placement there may reach first. A runner that ended before its job reached
// it is still registered with its scale set, unless the coordinator removed it
// there when it gave up on it (giveUp), so it joins the unused runners,
// saved before its place.json goes. The job it was started for waits again,
// in line from where it was: GitHub keeps an assigned job assigned to the
// scale set and sends nothing more, so a job dropped here would never be
// placed again. A run that could not reach its machine's agent rests the
// machine, and one whose runner failed to start rests the place
// (failedstart.go). A departed place's run ran as the setup had the place
// then; once it is retired the place is forgotten or, where the setup still
// has it, takes jobs as the setup has it now.
func (s *Service) retireLocked(p config.Place) {
	if d, ok := s.departed[p.ID]; ok {
		p = d
		delete(s.departed, p.ID)
		s.saveLocked()
	}
	run, ok := readPlaceRun(s.Cfg, p.ID)
	if !ok {
		return
	}
	if run.Unreachable && slices.ContainsFunc(s.Cfg.Machines, func(m config.Machine) bool { return m.Name == p.Machine }) {
		s.restLocked(p.Machine)
	}
	s.noteStartLocked(p, run)
	removed := s.givenUp[run.RunnerName]
	delete(s.givenUp, run.RunnerName)
	if !run.Started {
		if run.RunnerID > 0 && !removed {
			s.unused[unusedKey(run)] = run
		}
		for k, a := range s.acquired {
			if a.startedBy(p.ID, run) {
				delete(s.acquired, k)
				s.waiting[k] = a.Job
				s.Log.Info("the runner ended before its job reached it; the job waits again", "place", p.ID,
					"repo", a.Job.Repo, "job", a.Job.Name, "runner", run.RunnerName)
			}
		}
		s.saveLocked()
	}
	if err := s.recordUsage(p, run); err != nil {
		s.Log.Warn("could not retain resource readings", "place", p.ID, "err", err)
	}
	if err := retirePlaceRun(s.Cfg, p.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Log.Warn("could not retire the place's last run", "place", p.ID, "err", err)
	}
}

// noteWasteLocked logs any place left free while a job that fits it has
// waited over a minute. It should never fire; when it does, the line names
// the place and the job.
func (s *Service) noteWasteLocked() {
	now := s.now()
	var old []plan.Job
	for _, j := range s.waitingLocked() {
		if now.Sub(j.SeenAt) > time.Minute {
			old = append(old, j)
		}
	}
	v := s.snapshotLocked()
	for _, a := range plan.Plan(old, v.views(), v.headroom(), s.Rules, now) {
		if t, ok := s.wasted[a.Place.ID]; !ok || now.Sub(t) > 10*time.Minute {
			s.Log.Warn("waste: a place is free while a job that fits it waits", "place", a.Place.ID,
				"repo", a.Job.Repo, "job", a.Job.Name, "waiting", now.Sub(a.Job.QueueTime).Round(time.Second).String())
			s.wasted[a.Place.ID] = now
		}
	}
}
