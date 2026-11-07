package coordinator

import (
	"slices"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// queue is the jobs the coordinator knows of: waiting for a place, or
// acquired for a runner that has not yet taken one; and how long each kind
// of job took.
type queue struct {
	waiting  map[string]plan.Job
	acquired map[string]acquired
	timings  map[string]time.Duration
	// spare are the scale sets a job of which was cancelled before a runner
	// took it, while a runner left waiting for nothing is still to go
	// (stopSpare).
	spare map[setName]bool
	// waitWhy is why each waiting job last had no free place, as last
	// logged (logWaitsLocked).
	waitWhy map[string]string
}

func newQueue() queue {
	return queue{
		waiting:  map[string]plan.Job{},
		acquired: map[string]acquired{},
		timings:  map[string]time.Duration{},
		spare:    map[setName]bool{},
		waitWhy:  map[string]string{},
	}
}

type acquired struct {
	Job   plan.Job  `json:"job"`
	Place string    `json:"place"`
	At    time.Time `json:"at"`
}

// startedBy says whether run is the one the coordinator started in place for
// a, which a placement pass launches at the moment it acquires the job.
func (a acquired) startedBy(place string, run PlaceRun) bool {
	return a.Place == place && a.At.Equal(run.LaunchedAt)
}

// keysOf names the job a message is about, both ways.
func keysOf(target, pool string, m scaleset.JobMessageBase) []string {
	keys := []string{plan.RunKey(target, pool, m.WorkflowRunID, m.JobDisplayName)}
	if m.RunnerRequestID != 0 {
		keys = append(keys, plan.RequestKey(target, pool, m.RunnerRequestID))
	}
	return keys
}

// repoOf is the repository a job message belongs to: its own owner and name,
// or the target's for a repository scale set that left them out.
func repoOf(target string, m scaleset.JobMessageBase) string {
	if m.OwnerName != "" && m.RepositoryName != "" {
		return m.OwnerName + "/" + m.RepositoryName
	}
	return target
}

// jobOf is the job a message of target's scale set for pool is about, as seen
// at now.
func jobOf(target, pool string, m scaleset.JobMessageBase, assigned bool, now time.Time) plan.Job {
	return plan.Job{Target: target, Pool: pool, Repo: repoOf(target, m), RequestID: m.RunnerRequestID, QueueTime: m.QueueTime,
		Name: m.JobDisplayName, Workflow: m.JobWorkflowRef, RunID: m.WorkflowRunID, SeenAt: now, HeardAt: now,
		Assigned: assigned, JobID: m.JobID}
}

// waitLocked records a job GitHub offered or assigned. Offered or assigned
// again, it keeps its place in line.
func (s *Service) waitLocked(j plan.Job) {
	k := j.Key()
	if j.QueueTime.IsZero() {
		j.QueueTime = j.SeenAt
	}
	if old, ok := s.waiting[k]; ok {
		j.SeenAt, j.QueueTime = old.SeenAt, old.QueueTime
	}
	if a, ok := s.acquired[k]; ok {
		// Offered or assigned again after its runner was started. While that
		// runner is alive this is a redelivery, or GitHub assigning again a
		// job it took back before the runner was up; the runner takes it, and
		// a second runner would wait for nothing. Once that runner is gone,
		// GitHub has given the job back.
		if s.runnerAlive(a.Place) {
			a.Job.TakenBack, a.Job.JobID, a.Job.HeardAt = false, j.JobID, j.HeardAt
			s.acquired[k] = a
			return
		}
		delete(s.acquired, k)
		j.SeenAt, j.QueueTime = a.Job.SeenAt, a.Job.QueueTime
	}
	s.waiting[k] = j
	s.Log.Info("job waiting", "repo", j.Repo, "pool", j.Pool, "job", j.Name, "request", j.RequestID,
		"assigned", j.Assigned, "queued", j.QueueTime.Format(time.RFC3339))
}

// takeBackLocked marks an assigned job whose assignment GitHub took back, and
// reports whether the message was a take-back. The job keeps its place in
// line, and its starting runner, until GitHub assigns it again. GitHub can
// send the take-back of an assignment in the same message as the next
// assignment, whose job id is new, and a message's assignments are read before
// its completions: a take-back of an assignment other than the job's latest
// changes nothing.
func (s *Service) takeBackLocked(k, jobID string, now time.Time) bool {
	latest := func(j plan.Job) bool { return j.JobID == "" || j.JobID == jobID }
	if j, ok := s.waiting[k]; ok && j.Assigned {
		if latest(j) {
			j.TakenBack = true
		}
		j.HeardAt = now
		s.waiting[k] = j
		return true
	}
	if a, ok := s.acquired[k]; ok && a.Job.Assigned {
		if latest(a.Job) {
			a.Job.TakenBack = true
		}
		a.Job.HeardAt = now
		s.acquired[k] = a
		return true
	}
	return false
}

// handBackLocked finds the job a runner was started for when GitHub gave that
// runner another job of the same scale set: GitHub gives a set's runner
// whichever of the set's jobs it picks, and the job the runner was started
// for still needs one. When the job the runner took had a runner of its own
// still waiting, that runner is free now and the job handed back is its own,
// so no further runner starts only to wait out the start timeout with
// nothing to run. Otherwise the job waits again.
func (s *Service) handBackLocked(target, pool, runner string, started []string) {
	var freed *acquired
	for _, k := range started {
		if a, ok := s.acquired[k]; ok {
			if run, ok := readPlaceRun(s.Cfg, a.Place); ok && run.RunnerName != runner && a.startedBy(a.Place, run) && !run.Started &&
				s.runnerAlive(a.Place) {
				freed = &a
			}
		}
	}
	for k, a := range s.acquired {
		if a.Job.Target != target || a.Job.Pool != pool || slices.Contains(started, k) {
			continue
		}
		if run, ok := readPlaceRun(s.Cfg, a.Place); !ok || run.RunnerName != runner {
			continue
		}
		if freed != nil {
			s.acquired[k] = acquired{Job: a.Job, Place: freed.Place, At: freed.At}
			s.Log.Info("the runner took another job of its scale set; its own job goes to the runner started for that one",
				"place", a.Place, "job", a.Job.Name, "to", freed.Place)
			freed = nil
			continue
		}
		delete(s.acquired, k)
		s.waiting[k] = a.Job
		s.Log.Info("the runner took another job of its scale set; its own job waits again", "place", a.Place, "job", a.Job.Name)
	}
}

// waitingLocked is the jobs a place could take now, with what the rules read
// filled in. A job whose scale set has no live session cannot be acquired
// now, and a job GitHub took back is assigned to nothing until it is assigned
// again. A job a drain names may take only that drain's places.
func (s *Service) waitingLocked() []plan.Job {
	out := make([]plan.Job, 0, len(s.waiting))
	for _, j := range s.waiting {
		if _, ok := s.sets[setKey(j.Target, j.Pool)]; j.BorrowID == "" && (!ok || j.TakenBack) {
			continue
		}
		j.Size = s.Cfg.PoolSize(j.Pool)
		if j.BorrowID == "" {
			j.Estimate = s.timings[j.TimingKey()]
		}
		j.RunWorkflow = s.runWorkflows[workflowRun{j.Repo, j.RunID}].path
		if ref := j.LabelRef(); ref != "" {
			j.Labels = s.labels[j.Repo][ref]
		}
		j.Drain = s.drainFor(j)
		out = append(out, j)
	}
	return out
}

// expireLocked forgets jobs GitHub has certainly given up on: a job that
// waited over a day, and an acquired job no runner took within the start and
// boot allowance. A job whose own runner has not started stays acquired while
// that runner lives, and once it has ended waits again, in line from where it
// was, as retireLocked does.
func (s *Service) expireLocked() {
	now := s.now()
	for k, j := range s.waiting {
		if now.Sub(j.QueueTime) > 25*time.Hour {
			delete(s.waiting, k)
		}
	}
	limit := s.Cfg.Seconds(s.Cfg.StartTimeoutSeconds + s.Cfg.BootTimeoutSeconds)
	for k, a := range s.acquired {
		if now.Sub(a.At) <= limit {
			continue
		}
		run, ok := readPlaceRun(s.Cfg, a.Place)
		own := ok && a.startedBy(a.Place, run) && !run.Started
		if own && s.runnerAlive(a.Place) {
			continue
		}
		delete(s.acquired, k)
		if own {
			s.waiting[k] = a.Job
		}
	}
}
