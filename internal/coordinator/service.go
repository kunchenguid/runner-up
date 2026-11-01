// Package coordinator holds the scale set sessions, picks which waiting job
// each free place takes and starts its runner through the place's agent.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// SetClient is what the coordinator needs from one runner scale set.
type SetClient interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	GenerateJIT(ctx context.Context, runnerName, workFolder string) (jit string, runnerID int, err error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// Launcher starts a place's runner process for a job of pool. It is handed
// the place's locked busy.lock and must pass it to the process it starts,
// which keeps the place busy for as long as that process lives.
type Launcher interface {
	Launch(place config.Place, pool string, lock *os.File, jit string) error
	Stop(placeID string) error
}

// PlaceRun is a place's place.json for a job or borrow, written by the coordinator
// just before launch. For jobs, the place's runner updates it as the job starts.
type PlaceRun struct {
	BorrowID   string    `json:"borrowId,omitempty"`
	RunnerName string    `json:"runnerName"`
	RunnerID   int       `json:"runnerId"`
	Target     string    `json:"target"`
	Pool       string    `json:"pool"`
	Repo       string    `json:"repo"`
	RequestID  int64     `json:"requestId,omitempty"`
	JobName    string    `json:"jobName"`
	LaunchedAt time.Time `json:"launchedAt"`
	Started    bool      `json:"started"`
	PID        int       `json:"pid,omitempty"`
	// Unreachable: the place runner could not reach its machine's agent, or
	// lost it (reach.go).
	Unreachable bool `json:"unreachable,omitempty"`
}

type acquired struct {
	Job   plan.Job  `json:"job"`
	Place string    `json:"place"`
	At    time.Time `json:"at"`
}

// SavedState preserves the job queue and learned timings across a restart.
// Places remember themselves through their locks and place.json; local borrow
// requests are re-read by refreshBorrowsLocked.
type SavedState struct {
	Timings  map[string]time.Duration `json:"timings,omitempty"`
	Waiting  []plan.Job               `json:"waiting"`
	Acquired []acquired               `json:"acquired"`
	Unused   []PlaceRun               `json:"unused,omitempty"`
	// Unreachable is when each resting machine was found unreachable, by name.
	Unreachable map[string]time.Time `json:"unreachable,omitempty"`
}

type Service struct {
	Cfg      *config.Config
	Log      *slog.Logger
	now      func() time.Time
	launcher Launcher
	Rules    []plan.Rule

	mu       sync.Mutex
	sets     map[string]SetClient
	waiting  map[string]plan.Job
	acquired map[string]acquired
	cooling  map[string]time.Time
	wasted   map[string]time.Time
	timings  map[string]time.Duration
	// unused are runners that ended before a job reached them, by unusedKey:
	// each is still registered with its scale set until Tidy removes it.
	unused map[string]PlaceRun
	// timedOut are the runners no job reached in time that GitHub has
	// answered for, by name (stopIdleLocked): true once GitHub removed the
	// runner and its place is stopping, false when GitHub said it runs a job.
	timedOut map[string]bool
	// unreachable are the resting machines, by name, with when each was found
	// unreachable, and reachAsked when each was last asked (reach.go).
	unreachable map[string]time.Time
	reachAsked  map[string]time.Time
	// Reach asks a resting machine's agent for an answer; asking counts the
	// questions not yet answered.
	Reach  reacher
	asking sync.WaitGroup

	// The readers the order rules need, set only when the rules use them.
	LabelReader    func(context.Context, string) (map[string]map[string]bool, error)
	WorkflowReader func(context.Context, string, int64) (string, error)
	// RunReader, set whenever GitHub's API can be read, says whether GitHub
	// has closed a workflow run, cancelled or finished, and every job of it
	// with it (closedruns.go).
	RunReader    func(context.Context, string, int64) (bool, error)
	readMu       sync.Mutex
	runsChecked  map[workflowRun]time.Time
	runReads     []time.Time
	labelsRead   map[string]time.Time
	labels       map[string]map[string]map[string]bool
	runWorkflows map[workflowRun]runWorkflow

	// vmReader, set when a place runs in tart, reads each Mac's running macOS
	// VMs (vmlimit.go); vms is the latest reading by machine.
	VmReader vmReader
	vms      map[string][]string
	vmsRead  map[string]time.Time
}

type workflowRun struct {
	repo string
	id   int64
}

type runWorkflow struct {
	path      string
	attempted time.Time
}

func setKey(target, pool string) string { return target + "|" + pool }

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

func NewService(cfg *config.Config, log *slog.Logger, launcher Launcher) (*Service, error) {
	rules, err := plan.ParseOrder(cfg.Order)
	if err != nil {
		return nil, err
	}
	s := &Service{
		Cfg: cfg, Log: log, now: time.Now, launcher: launcher, Rules: rules,
		sets:         map[string]SetClient{},
		waiting:      map[string]plan.Job{},
		acquired:     map[string]acquired{},
		cooling:      map[string]time.Time{},
		wasted:       map[string]time.Time{},
		timings:      map[string]time.Duration{},
		unused:       map[string]PlaceRun{},
		timedOut:     map[string]bool{},
		unreachable:  map[string]time.Time{},
		reachAsked:   map[string]time.Time{},
		Reach:        ReachAgent,
		labelsRead:   map[string]time.Time{},
		labels:       map[string]map[string]map[string]bool{},
		runWorkflows: map[workflowRun]runWorkflow{},
		runsChecked:  map[workflowRun]time.Time{},
		vms:          map[string][]string{},
		vmsRead:      map[string]time.Time{},
	}
	for _, p := range cfg.Places() {
		if err := os.MkdirAll(cfg.PlaceDir(p.ID), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.LogsDir(), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.statePath())
	switch {
	case err == nil:
		var st SavedState
		if err := json.Unmarshal(b, &st); err != nil {
			log.Warn("state file unreadable; starting with no waiting jobs", "err", err)
		}
		if st.Timings != nil {
			s.timings = st.Timings
		}
		for _, j := range st.Waiting {
			s.waiting[j.Key()] = j
		}
		for _, a := range st.Acquired {
			s.acquired[a.Job.Key()] = a
		}
		for _, run := range st.Unused {
			s.unused[unusedKey(run)] = run
		}
		for name, since := range st.Unreachable {
			if slices.ContainsFunc(cfg.Machines, func(m config.Machine) bool { return m.Name == name }) {
				s.unreachable[name] = since
			}
		}
		log.Info("restored state", "waiting", len(s.waiting), "acquired", len(s.acquired), "unused", len(s.unused),
			"resting", len(s.unreachable))
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return s, nil
}

func (s *Service) statePath() string { return filepath.Join(s.Cfg.StateDir, "state.json") }

// AddSet makes a scale set's client available for acquiring and placing.
func (s *Service) AddSet(target, pool string, c SetClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets[setKey(target, pool)] = c
}

func (s *Service) RemoveSet(target, pool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sets, setKey(target, pool))
}

// Handle takes one message from a scale set's listener: it records what
// changed and then places whatever now fits.
func (s *Service) Handle(ctx context.Context, target, pool string, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		s.Place(ctx)
		return nil
	}
	s.mu.Lock()
	now := s.now()
	job := func(m scaleset.JobMessageBase, assigned bool) plan.Job { return jobOf(target, pool, m, assigned, now) }
	for _, m := range msg.JobAvailableMessages {
		s.waitLocked(job(m.JobMessageBase, false))
	}
	// GitHub assigns most jobs to the scale set itself, as many as the
	// capacity each poll announces, and waits for a runner from the set.
	for _, m := range msg.JobAssignedMessages {
		s.waitLocked(job(m.JobMessageBase, true))
	}
	for _, m := range msg.JobStartedMessages {
		keys := keysOf(target, pool, m.JobMessageBase)
		s.handBackLocked(target, pool, m.RunnerName, keys)
		// An assigned job carries no queue time: it is in line from when the
		// coordinator first saw it, or else from its assignment.
		queued := m.QueueTime
		for _, k := range keys {
			if a, ok := s.acquired[k]; ok && queued.IsZero() {
				queued = a.Job.QueueTime
			}
			if j, ok := s.waiting[k]; ok && queued.IsZero() {
				queued = j.QueueTime
			}
			delete(s.acquired, k)
			delete(s.waiting, k)
		}
		if queued.IsZero() {
			queued = m.ScaleSetAssignTime
		}
		s.Log.Info("job started", "repo", repoOf(target, m.JobMessageBase), "job", m.JobDisplayName, "run", m.WorkflowRunID,
			"runner", m.RunnerName, "waited", m.RunnerAssignTime.Sub(queued).Round(time.Second).String())
	}
	cancelledBeforeRunner := false
	for _, m := range msg.JobCompletedMessages {
		// Runner assignment to finish is wall runtime, not queue or boot time.
		// Cancellations, failures and assignment take-backs teach nothing.
		if m.Result == "succeeded" && m.RunnerName != "" && !m.RunnerAssignTime.IsZero() && m.FinishTime.After(m.RunnerAssignTime) {
			j := job(m.JobMessageBase, false)
			s.timings[j.TimingKey()] = m.FinishTime.Sub(m.RunnerAssignTime)
		}
		for _, k := range keysOf(target, pool, m.JobMessageBase) {
			// When no runner reached an assigned job in five minutes, GitHub
			// takes the assignment back, with no finish time, and assigns the
			// job again, a moment later or in this same message. A cancelled
			// job has a finish time.
			if m.RunnerName == "" && m.FinishTime.IsZero() && s.takeBackLocked(k, m.JobID, now) {
				continue
			}
			if _, ok := s.acquired[k]; ok && m.RunnerName == "" {
				cancelledBeforeRunner = true
			}
			delete(s.acquired, k)
			delete(s.waiting, k)
		}
		s.Log.Info("job completed", "repo", repoOf(target, m.JobMessageBase), "job", m.JobDisplayName, "run", m.WorkflowRunID,
			"runner", m.RunnerName, "result", m.Result)
	}
	if msg.Statistics != nil {
		known := 0
		for _, j := range s.waiting {
			if j.Target == target && j.Pool == pool {
				known++
			}
		}
		if msg.Statistics.TotalAvailableJobs > known {
			s.Log.Warn("GitHub counts more waiting jobs than the coordinator knows", "target", target, "pool", pool,
				"github", msg.Statistics.TotalAvailableJobs, "known", known)
		}
	}
	s.saveLocked()
	s.mu.Unlock()

	if cancelledBeforeRunner {
		s.stopSurplus(target, pool)
	}
	s.Place(ctx)
	return nil
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
	placeRun := func(place string) (PlaceRun, bool) {
		var run PlaceRun
		return run, config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(place), "place.json"), &run) == nil
	}
	var freed *acquired
	for _, k := range started {
		if a, ok := s.acquired[k]; ok {
			if run, ok := placeRun(a.Place); ok && run.RunnerName != runner && a.startedBy(a.Place, run) && !run.Started &&
				s.runnerAlive(a.Place) {
				freed = &a
			}
		}
	}
	for k, a := range s.acquired {
		if a.Job.Target != target || a.Job.Pool != pool || slices.Contains(started, k) {
			continue
		}
		if run, ok := placeRun(a.Place); !ok || run.RunnerName != runner {
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

// refreshLabels reads the pull request labels of repositories with waiting
// pull request or branch jobs, at most once a minute each. Network reads
// never hold the queue lock; an unreadable refresh forgets the repository's
// labels rather than keep one that may have been removed.
func (s *Service) refreshLabels(ctx context.Context) {
	if s.LabelReader == nil {
		return
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.mu.Lock()
	now := s.now()
	repos := map[string]bool{}
	for _, j := range s.waiting {
		if !j.TakenBack && j.LabelRef() != "" {
			if last, ok := s.labelsRead[j.Repo]; !ok || now.Sub(last) >= time.Minute {
				repos[j.Repo] = true
			}
		}
	}
	s.mu.Unlock()
	for repo := range repos {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		labels, err := s.LabelReader(readCtx, repo)
		cancel()
		s.mu.Lock()
		s.labelsRead[repo] = now
		if err != nil {
			delete(s.labels, repo)
			s.Log.Warn("pull request labels unreadable; ordering without them", "repo", repo, "err", err)
		} else {
			s.labels[repo] = labels
		}
		s.mu.Unlock()
	}
}

// refreshRunWorkflows reads each waiting run's top-level workflow once, shared
// by all its jobs; a failed read is tried again after a minute. A run no job
// waits for any longer is forgotten. The whole pass has five seconds and
// never holds the queue lock.
func (s *Service) refreshRunWorkflows(ctx context.Context) {
	if s.WorkflowReader == nil {
		return
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.mu.Lock()
	now := s.now()
	queued := map[workflowRun]bool{}
	for _, j := range s.waiting {
		if !j.TakenBack && j.RunID > 0 {
			queued[workflowRun{j.Repo, j.RunID}] = true
		}
	}
	for key := range s.runWorkflows {
		if !queued[key] {
			delete(s.runWorkflows, key)
		}
	}
	var reads []workflowRun
	for key := range queued {
		v, ok := s.runWorkflows[key]
		if !ok || v.path == "" && now.Sub(v.attempted) >= time.Minute {
			reads = append(reads, key)
		}
	}
	s.mu.Unlock()
	ctx, cancelPass := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPass()
	for _, key := range reads {
		if ctx.Err() != nil {
			break
		}
		p, err := s.WorkflowReader(ctx, key.repo, key.id)
		s.mu.Lock()
		if err != nil {
			p = ""
			s.Log.Warn("top-level workflow unreadable; using the job's own", "repo", key.repo, "run", key.id, "err", err)
		}
		s.runWorkflows[key] = runWorkflow{path: p, attempted: now}
		s.mu.Unlock()
	}
}

// Place gives every free place to the job the rules pick, until nothing more
// fits. It starts a runner for a job only once its place is chosen, and
// acquires a job GitHub offered only then, so a job no place can take holds
// nothing.
func (s *Service) Place(ctx context.Context) {
	s.refreshLabels(ctx)
	s.refreshRunWorkflows(ctx)
	s.forgetClosedRuns(ctx)
	s.refreshVMs(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askRestingLocked(ctx)
	s.expireLocked()
	s.refreshBorrowsLocked()
	for range len(s.Cfg.Places()) + 1 {
		assignments := plan.Plan(s.waitingLocked(), s.viewsLocked(), s.headroomLocked(), s.Rules, s.now())
		if len(assignments) == 0 {
			break
		}
		progressed := false
		for _, a := range assignments {
			if s.assignLocked(ctx, a) {
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	s.noteWasteLocked()
	s.saveLocked()
}

// waitingLocked is the jobs a place could take now, with what the rules read
// filled in. A job whose scale set has no live session cannot be acquired
// now, and a job GitHub took back is assigned to nothing until it is assigned
// again.
func (s *Service) waitingLocked() []plan.Job {
	out := make([]plan.Job, 0, len(s.waiting))
	for _, j := range s.waiting {
		if _, ok := s.sets[setKey(j.Target, j.Pool)]; j.BorrowID == "" && (!ok || j.TakenBack) {
			continue
		}
		if j.BorrowID == "" {
			j.Estimate = s.timings[j.TimingKey()]
		}
		j.RunWorkflow = s.runWorkflows[workflowRun{j.Repo, j.RunID}].path
		if ref := j.LabelRef(); ref != "" {
			j.Labels = s.labels[j.Repo][ref]
		}
		out = append(out, j)
	}
	return out
}

// viewsLocked reads every place's state from its lock. A place cooling down
// after a failed launch reads as busy for a minute, and a place on a resting
// machine until the machine's agent answers (reach.go).
func (s *Service) viewsLocked() []plan.PlaceView {
	now := s.now()
	places := s.Cfg.Places()
	out := make([]plan.PlaceView, 0, len(places))
	for _, p := range places {
		busy, _ := s.PlaceBusy(p)
		if !busy && (now.Before(s.cooling[p.ID]) || s.restingLocked(p.Machine)) {
			busy = true
		}
		out = append(out, plan.PlaceView{Place: p, Busy: busy})
	}
	return out
}

func share(p config.Place) config.Share { return config.Share{Cores: p.Cores, MemoryMB: p.MemoryMB} }

// headroomLocked is what each counted machine's places may still reserve.
func (s *Service) headroomLocked() map[string]plan.Headroom {
	out := map[string]plan.Headroom{}
	for _, m := range s.Cfg.Machines {
		if m.Cores == 0 {
			continue
		}
		b, used := m.Budget(), s.inUse(m.Name, "")
		out[m.Name] = plan.Headroom{Cores: b.Cores - used.Cores, MemoryMB: b.MemoryMB - used.MemoryMB, Memory: b.MemoryMB > 0}
	}
	return out
}

// inUse sums what machine's places reserve while their runner or borrow
// supervisor is alive, leaving out the place except. A runner waiting for its
// job keeps its share, since the job will run there.
func (s *Service) inUse(machine, except string) config.Share {
	used := config.Share{}
	for _, q := range s.Cfg.Places() {
		if q.ID != except && q.Machine == machine && s.runnerAlive(q.ID) {
			used.Cores += q.Cores
			used.MemoryMB += q.MemoryMB
		}
	}
	return used
}

// PlaceBusy reads one place. Its own lock says whether its runner or borrow
// supervisor is alive. On a machine with cores, a place is also busy while its
// share does not fit beside the machine's other busy places, within the
// machine's budget less the owner's room. A tart place is
// also busy while its Mac runs as many macOS VMs as it may (vmlimit.go). A
// lock that cannot be read counts busy, so a broken reader never starts a
// second job in a place.
func (s *Service) PlaceBusy(p config.Place) (bool, string) {
	held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock"))
	if err != nil {
		return true, "unreadable"
	}
	if held {
		var run PlaceRun
		if config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "place.json"), &run) == nil && run.BorrowID != "" {
			return true, "borrow"
		}
		return true, "job"
	}
	if why := s.tight(p); why != "" {
		return true, why
	}
	if why := s.vmLimitFull(p); why != "" {
		return true, why
	}
	return false, ""
}

// tight says why place p's share does not fit beside its machine's other
// runs, or "" when it fits. A machine without cores always fits.
func (s *Service) tight(p config.Place) string {
	m := s.Cfg.Machine(p.Machine)
	if m.Cores == 0 {
		return ""
	}
	b, used, sh := m.Budget(), s.inUse(p.Machine, p.ID), share(p)
	room := func(n int, unit string) string {
		if n == 0 {
			return ""
		}
		return fmt.Sprintf(", %d%s kept for the owner", n, unit)
	}
	if used.Cores+sh.Cores > b.Cores {
		return fmt.Sprintf("cores %d of %d in use%s", used.Cores, b.Cores, room(m.RoomCores, ""))
	}
	if b.MemoryMB > 0 && used.MemoryMB+sh.MemoryMB > b.MemoryMB {
		return fmt.Sprintf("memory %d of %d MB in use%s", used.MemoryMB, b.MemoryMB, room(m.RoomMemoryMB, " MB"))
	}
	return ""
}

// runnerAlive reports whether a place's runner or borrow supervisor is alive,
// from its lock alone. A lock that cannot be read counts alive.
func (s *Service) runnerAlive(placeID string) bool {
	held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(placeID), "busy.lock"))
	return err != nil || held
}

func (s *Service) assignLocked(ctx context.Context, a plan.Assignment) bool {
	j, p := a.Job, a.Place
	set := s.sets[setKey(j.Target, j.Pool)]
	if set == nil && j.BorrowID == "" {
		return false
	}
	// A place given in the same plan as another may have to wait for it now.
	if busy, _ := s.PlaceBusy(p); busy {
		return false
	}
	lock, err := lock.Busy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock"))
	if err != nil || lock == nil {
		return false
	}
	defer lock.Close()
	// This place's last run may have ended since the last Tidy, and its
	// place.json is about to be replaced. That run may have found the machine
	// unreachable.
	s.retireLocked(p)
	if s.restingLocked(p.Machine) {
		return true
	}

	if j.BorrowID != "" {
		return s.assignBorrowLocked(p, j, lock)
	}

	if !j.Assigned {
		got, err := set.AcquireJobs(ctx, []int64{j.RequestID})
		if err != nil {
			s.Log.Error("acquire failed", "repo", j.Repo, "job", j.Name, "err", err)
			return false
		}
		if !slices.Contains(got, j.RequestID) {
			// Taken, cancelled or expired at GitHub while it waited here.
			delete(s.waiting, j.Key())
			s.Log.Info("job no longer available", "repo", j.Repo, "job", j.Name, "request", j.RequestID)
			return true
		}
	}
	delete(s.waiting, j.Key())
	now := s.now()
	s.acquired[j.Key()] = acquired{Job: j, Place: p.ID, At: now}

	name := fmt.Sprintf("runner-up-%s-%d", p.ID, now.Unix())
	jit, runnerID, err := set.GenerateJIT(ctx, name, p.RunnerWorkFolder())
	if err == nil {
		run := PlaceRun{RunnerName: name, RunnerID: runnerID, Target: j.Target, Pool: j.Pool, Repo: j.Repo,
			RequestID: j.RequestID, JobName: j.Name, LaunchedAt: now}
		if err = config.WriteJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "place.json"), run); err == nil {
			err = s.launcher.Launch(p, j.Pool, lock, jit)
		}
	}
	if err != nil {
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

// stopSurplus stops runners nobody will use: when a job acquired for this
// scale set was cancelled before a runner took it, the scale set has more
// runners waiting for a job than jobs on their way, and the newest waiting
// runner would otherwise hold its place until the start timeout.
func (s *Service) stopSurplus(target, pool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	for _, a := range s.acquired {
		if a.Job.Target == target && a.Job.Pool == pool {
			pending++
		}
	}
	type idle struct {
		place string
		at    time.Time
	}
	var idles []idle
	for _, p := range s.Cfg.Places() {
		held, _ := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock"))
		if !held {
			continue
		}
		var run PlaceRun
		if config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "place.json"), &run) != nil {
			continue
		}
		if run.Target == target && run.Pool == pool && !run.Started {
			idles = append(idles, idle{p.ID, run.LaunchedAt})
		}
	}
	sort.Slice(idles, func(a, b int) bool { return idles[a].at.After(idles[b].at) })
	for i := 0; i < len(idles)-pending; i++ {
		s.Log.Info("stopping a runner whose job was cancelled before it started", "place", idles[i].place)
		if err := s.launcher.Stop(idles[i].place); err != nil {
			s.Log.Warn("stop failed", "place", idles[i].place, "err", err)
		}
	}
}

// Tidy runs on every tick: it forgets the last run of every free place,
// removes the registration of every runner that ended without a job, and
// gives up on every runner no job reached in time.
func (s *Service) Tidy(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Cfg.Places() {
		if held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(p.ID), "busy.lock")); err == nil && !held {
			s.retireLocked(p)
		}
	}
	removed := false
	for k, run := range s.unused {
		set := s.sets[setKey(run.Target, run.Pool)]
		if set == nil {
			// Its scale set has no live session; once it has, a later Tidy
			// removes the runner.
			continue
		}
		err := set.RemoveRunner(ctx, int64(run.RunnerID))
		if errors.Is(err, scaleset.JobStillRunningError) {
			// Its place has ended, yet GitHub gave the runner a job: no runner
			// is left to run it, and GitHub fails it once it stops hearing
			// from the runner.
			s.Log.Error("a runner whose place has ended is running a job at GitHub; the job is stranded", "runner", run.RunnerName,
				"target", run.Target, "pool", run.Pool)
			continue
		}
		if err != nil && !runnerGone(err) {
			s.Log.Warn("could not remove an unused runner; will retry", "runner", run.RunnerName, "err", err)
			continue
		}
		s.Log.Info("removed an unused runner", "runner", run.RunnerName)
		delete(s.unused, k)
		removed = true
	}
	if removed {
		s.saveLocked()
	}
	s.stopIdleLocked(ctx)
	pruneLogs(s.Cfg.LogsDir(), s.now().Add(-time.Duration(s.Cfg.LogKeepDays)*24*time.Hour))
}

// runnerGone says a removal found no such runner at GitHub.
func runnerGone(err error) bool {
	return errors.Is(err, scaleset.RunnerNotFoundError) || strings.Contains(err.Error(), "404")
}

// stopIdleLocked gives up on every runner that has waited the start timeout
// with no job. GitHub can give a scale set's idle runner a job at any moment
// until the runner is removed, and a runner stopped first would leave that
// job to fail when GitHub stops hearing from it. So the runner's registration
// goes first, and its place is stopped only once GitHub has accepted that. A
// runner GitHub will not remove because it is running a job keeps running
// it. One whose removal GitHub did not answer, or whose scale set has no live
// session, keeps its place until a later tick removes it.
func (s *Service) stopIdleLocked(ctx context.Context) {
	limit := s.Cfg.Seconds(s.Cfg.StartTimeoutSeconds)
	now := s.now()
	for _, p := range s.Cfg.Places() {
		dir := s.Cfg.PlaceDir(p.ID)
		if held, err := lock.HeldBy(filepath.Join(dir, "busy.lock")); err != nil || !held {
			continue
		}
		var run PlaceRun
		if config.ReadJSON(filepath.Join(dir, "place.json"), &run) != nil || run.BorrowID != "" || run.Started ||
			now.Sub(run.LaunchedAt) < limit {
			continue
		}
		if _, answered := s.timedOut[run.RunnerName]; answered {
			continue
		}
		if run.RunnerID > 0 {
			set := s.sets[setKey(run.Target, run.Pool)]
			if set == nil {
				continue
			}
			err := set.RemoveRunner(ctx, int64(run.RunnerID))
			if errors.Is(err, scaleset.JobStillRunningError) {
				s.timedOut[run.RunnerName] = false
				s.Log.Info("no job reached the runner in time, but GitHub says it is running one; keeping it",
					"place", p.ID, "runner", run.RunnerName)
				continue
			}
			if err != nil && !runnerGone(err) {
				s.Log.Warn("could not remove a runner no job reached in time; it keeps its place until GitHub answers",
					"place", p.ID, "runner", run.RunnerName, "err", err)
				continue
			}
		}
		s.Log.Info("no job reached the runner in time; removed it from its scale set, stopping it", "place", p.ID,
			"runner", run.RunnerName)
		if err := s.launcher.Stop(p.ID); err != nil {
			s.Log.Warn("stop failed", "place", p.ID, "err", err)
			continue
		}
		s.timedOut[run.RunnerName] = true
	}
}

// retireLocked forgets the last run of a free place, which Tidy or the next
// placement there may reach first. A runner that ended before its job reached
// it is still registered with its scale set, unless stopIdleLocked removed it
// there, so it joins the unused runners, saved before its place.json goes.
// The job it was started for waits again, in line from where it was: GitHub
// keeps an assigned job assigned to the scale set and sends nothing more, so
// a job dropped here would never be placed again. A run that could not reach
// its machine's agent rests the machine.
func (s *Service) retireLocked(p config.Place) {
	dir := s.Cfg.PlaceDir(p.ID)
	path := filepath.Join(dir, "place.json")
	var run PlaceRun
	if config.ReadJSON(path, &run) != nil {
		return
	}
	if run.Unreachable {
		s.restLocked(p.Machine)
	}
	removed := s.timedOut[run.RunnerName]
	delete(s.timedOut, run.RunnerName)
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
	os.Rename(path, filepath.Join(dir, "last-place.json"))
}

func unusedKey(run PlaceRun) string { return fmt.Sprintf("%s|%d", run.Target, run.RunnerID) }

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
		var run PlaceRun
		own := config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(a.Place), "place.json"), &run) == nil && a.startedBy(a.Place, run) && !run.Started
		if own && s.runnerAlive(a.Place) {
			continue
		}
		delete(s.acquired, k)
		if own {
			s.waiting[k] = a.Job
		}
	}
}

// startedBy says whether run is the one the coordinator started in place for
// a, which assignLocked launches at the moment it acquires the job.
func (a acquired) startedBy(place string, run PlaceRun) bool {
	return a.Place == place && a.At.Equal(run.LaunchedAt)
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
	for _, a := range plan.Plan(old, s.viewsLocked(), s.headroomLocked(), s.Rules, now) {
		if t, ok := s.wasted[a.Place.ID]; !ok || now.Sub(t) > 10*time.Minute {
			s.Log.Warn("waste: a place is free while a job that fits it waits", "place", a.Place.ID,
				"repo", a.Job.Repo, "job", a.Job.Name, "waiting", now.Sub(a.Job.QueueTime).Round(time.Second).String())
			s.wasted[a.Place.ID] = now
		}
	}
}

func (s *Service) saveLocked() {
	st := SavedState{Timings: s.timings, Unreachable: s.unreachable}
	for _, j := range s.waiting {
		st.Waiting = append(st.Waiting, j)
	}
	for _, a := range s.acquired {
		st.Acquired = append(st.Acquired, a)
	}
	for _, run := range s.unused {
		st.Unused = append(st.Unused, run)
	}
	sort.Slice(st.Waiting, func(a, b int) bool { return st.Waiting[a].Key() < st.Waiting[b].Key() })
	sort.Slice(st.Acquired, func(a, b int) bool { return st.Acquired[a].Job.Key() < st.Acquired[b].Job.Key() })
	sort.Slice(st.Unused, func(a, b int) bool { return unusedKey(st.Unused[a]) < unusedKey(st.Unused[b]) })
	if err := config.WriteJSON(s.statePath(), st); err != nil {
		s.Log.Error("could not save state", "err", err)
	}
}

// pruneLogs removes place logs older than cutoff.
func pruneLogs(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
