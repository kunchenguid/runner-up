package coordinator

import (
	"path/filepath"
	"time"

	"github.com/kunchenguid/runner-up/internal/lock"
)

// A runner whose job GitHub has finished has nothing left to do: GitHub gives
// a one-job runner no second job, and takes nothing more from it for the
// finished one, whatever its result. It normally ends within seconds and its
// place frees. A runner that lost GitHub may not: GitHub fails its job once it
// stops hearing from the runner, while the runner keeps trying to reach
// GitHub and would hold its place until the job limit. So the coordinator
// keeps when GitHub said each runner's job finished, and stops the place of a
// runner still holding it finishedGrace later, which leaves a runner ending
// in the ordinary way, and its agent cleaning up, ample time. Its place then
// frees as any stopped place does. The times are saved, so a coordinator
// restart keeps them.

// finishedGrace is how long after GitHub finished a runner's job the runner
// may still hold its place.
const finishedGrace = 10 * time.Minute

// noteFinishedLocked keeps when GitHub says runner's job finished: at, its
// finish time, or now when GitHub gave none.
func (s *Service) noteFinishedLocked(runner string, at, now time.Time) {
	if at.IsZero() {
		at = now
	}
	s.finished[runner] = at
}

// finishedPlace is a place whose runner outlived its job by finishedGrace.
type finishedPlace struct {
	place string
	run   PlaceRun
	at    time.Time
}

// endFinished stops the place of each runner still holding one finishedGrace
// after GitHub finished its job, once, and forgets the finished runners that
// hold no place. It is called with pass held, and stops places with mu let go.
func (s *Service) endFinished() {
	s.mu.Lock()
	now := s.now()
	live := map[string]bool{}
	var due []finishedPlace
	for _, p := range s.placesLocked() {
		// A lock that cannot be read counts held, as runnerAlive reads it.
		if held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock")); err == nil && !held {
			continue
		}
		run, ok := readPlaceRun(s.Cfg, p.ID)
		if !ok || run.BorrowID != "" {
			continue
		}
		at, finished := s.finished[run.RunnerName]
		if !finished {
			continue
		}
		live[run.RunnerName] = true
		if now.Sub(at) >= finishedGrace && !s.stopping[run.RunnerName] {
			due = append(due, finishedPlace{place: p.ID, run: run, at: at})
		}
	}
	forgot := false
	for name := range s.finished {
		if !live[name] {
			delete(s.finished, name)
			delete(s.stopping, name)
			forgot = true
		}
	}
	if forgot {
		s.saveLocked()
	}
	s.mu.Unlock()
	var stopped []string
	for _, d := range due {
		job := d.run.RanJob
		if job == "" {
			job = d.run.JobName
		}
		s.Log.Warn("GitHub finished the runner's job, yet the runner still holds its place; stopping it", "place", d.place,
			"runner", d.run.RunnerName, "repo", d.run.Repo, "job", job, "finished", now.Sub(d.at).Round(time.Second).String()+" ago")
		if err := s.launcher.Stop(d.place); err != nil {
			s.Log.Warn("stop failed", "place", d.place, "err", err)
			continue
		}
		stopped = append(stopped, d.run.RunnerName)
	}
	s.mu.Lock()
	for _, name := range stopped {
		s.stopping[name] = true
	}
	s.mu.Unlock()
}
