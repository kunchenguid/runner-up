package coordinator

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/kunchenguid/runner-up/internal/plan"
)

// GitHub may never tell the scale set that a job it assigned is gone: when a
// run is cancelled while one of its jobs is taken back, or its cancellation
// is lost with a session, the job stays waiting here for a day, and while it
// reads as assigned every free place starts a runner for it that waits for
// nothing. So the coordinator asks GitHub about the run of a waiting job it
// has heard nothing about for a while, and forgets the job once the run is
// closed.
//
// An assigned job no runner takes is taken back after five minutes, so one
// quiet for quietAssigned is suspect, and its run is asked about again every
// recheckAssigned while it stays quiet. A taken-back job may wait quietly
// for its next assignment for as long as the queue is long, and no runner
// starts for it meanwhile, so it is asked about only after quietTakenBack,
// and again every recheckTakenBack. However many runs are quiet, at most
// runReadsPerPass are read in one pass and runReadsPerHour in an hour.
const (
	quietAssigned    = 10 * time.Minute
	recheckAssigned  = 15 * time.Minute
	quietTakenBack   = time.Hour
	recheckTakenBack = time.Hour
	runReadsPerPass  = 10
	runReadsPerHour  = 60
)

// quietFor is how long GitHub must have said nothing about a waiting job
// before its run is asked about, and how often it is asked again; zero for a
// job that is never asked about: a borrow, or one GitHub offered, which
// acquiring checks.
func quietFor(j plan.Job) (quiet, recheck time.Duration) {
	switch {
	case j.BorrowID != "" || !j.Assigned || j.RunID <= 0:
		return 0, 0
	case j.TakenBack:
		return quietTakenBack, recheckTakenBack
	default:
		return quietAssigned, recheckAssigned
	}
}

// quietSince reports whether GitHub has said nothing about j since before
// at less its quiet time.
func quietSince(j plan.Job, at time.Time) bool {
	quiet, _ := quietFor(j)
	return quiet > 0 && at.Sub(j.Heard()) >= quiet
}

// forgetClosedRuns asks GitHub about the runs of waiting assigned jobs it
// has gone quiet about, and forgets those jobs of a run GitHub has closed. A
// job is never forgotten on a run GitHub calls open, on an answer it could
// not give, or once GitHub has sent anything about it since the pass began:
// the run may have been run again. A job with a runner is not waiting, and
// is never asked about. The pass has five seconds and never holds the queue
// lock while it reads.
func (s *Service) forgetClosedRuns(ctx context.Context) {
	if s.RunReader == nil {
		return
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.mu.Lock()
	now := s.now()
	// Each quiet run is asked about again after the shortest recheck of its
	// quiet jobs.
	recheck := map[workflowRun]time.Duration{}
	for _, j := range s.waiting {
		if !quietSince(j, now) {
			continue
		}
		key := workflowRun{j.Repo, j.RunID}
		if _, every := quietFor(j); recheck[key] == 0 || every < recheck[key] {
			recheck[key] = every
		}
	}
	for key := range s.runsChecked {
		if recheck[key] == 0 {
			delete(s.runsChecked, key)
		}
	}
	s.runReads = slices.DeleteFunc(s.runReads, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	var reads []workflowRun
	for key, every := range recheck {
		if last, ok := s.runsChecked[key]; !ok || now.Sub(last) >= every {
			reads = append(reads, key)
		}
	}
	// The runs asked about longest ago, or never, first; then by run.
	slices.SortFunc(reads, func(a, b workflowRun) int {
		return cmp.Or(s.runsChecked[a].Compare(s.runsChecked[b]), cmp.Compare(a.repo, b.repo), cmp.Compare(a.id, b.id))
	})
	reads = reads[:min(len(reads), runReadsPerPass, max(runReadsPerHour-len(s.runReads), 0))]
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, key := range reads {
		if ctx.Err() != nil {
			break
		}
		closed, err := s.RunReader(ctx, key.repo, key.id)
		s.mu.Lock()
		s.runsChecked[key] = now
		s.runReads = append(s.runReads, now)
		if err != nil {
			s.Log.Warn("could not ask GitHub whether a run is closed; its waiting jobs stay", "repo", key.repo, "run", key.id, "err", err)
		} else if closed {
			for k, j := range s.waiting {
				// Quiet since before the question: GitHub has assigned it
				// again since, by a new attempt of the run, if not.
				if j.Repo == key.repo && j.RunID == key.id && quietSince(j, now) {
					delete(s.waiting, k)
					s.Log.Info("GitHub closed the job's run; forgot the job", "repo", j.Repo, "pool", j.Pool, "job", j.Name,
						"run", j.RunID, "takenBack", j.TakenBack, "queued", j.QueueTime.Format(time.RFC3339))
				}
			}
		}
		s.mu.Unlock()
	}
}
