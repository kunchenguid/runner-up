// Package coordinator holds the scale set sessions, picks which waiting job
// each free place takes and starts its runner through the place's agent.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
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
	// Departed are the places a setup change removed, or moved to another
	// machine, while their runner ran (runners.departed).
	Departed []departedPlace `json:"departed,omitempty"`
	// FailedStarts are the places resting after their runner failed to start,
	// by id (failedstart.go).
	FailedStarts map[string]PlaceRest `json:"failedStarts,omitempty"`
	// Finished is when GitHub said each runner's job finished, by runner
	// name, for the runners still holding a place (finished.go).
	Finished map[string]time.Time `json:"finished,omitempty"`
	// Memory is the latest free-memory reading of each machine with a
	// floor, by name, for runner-up status (memfloor.go).
	Memory map[string]MemoryReading `json:"memory,omitempty"`
}

// departedPlace is a departed place as the setup had it, with its machine,
// which a place's own JSON leaves out.
type departedPlace struct {
	Machine string       `json:"machine"`
	Place   config.Place `json:"place"`
}

// Service is the coordinator's state, in four parts:
//   - queue (queue.go): the jobs waiting for a place, and those acquired for
//     a runner that has not yet taken one
//   - runners (runners.go, failedstart.go, finished.go): the runners it
//     started, and what it still owes them and their places
//   - machines (reach.go, vmlimit.go, memfloor.go): which machines rest,
//     which macOS VMs each Mac runs, and the free memory of each machine
//     with a floor
//   - reads (reads.go, closedruns.go): what it read from GitHub's API for the
//     order rules and about quiet runs
//
// and, within each placement pass, a snapshot of the places (snapshot.go).
//
// mu guards every part, and is never held through a call to GitHub, a
// machine or a launch: a pass reads under mu, lets go of it to call out, and
// takes it again to record the answer. pass makes the placement and tidying
// passes take turns, so only one acts on a place at a time. Only the serve
// loop (serve.go) runs them, with the serving context; a scale set's session
// hands its messages to Handle, which records them and wakes the loop.
type Service struct {
	Cfg      *config.Config
	Log      *slog.Logger
	now      func() time.Time
	launcher Launcher
	Rules    []plan.Rule

	pass sync.Mutex
	mu   sync.Mutex
	sets map[string]*liveSet
	wake chan struct{}

	queue
	runners
	machines
	reads

	// drains are the live drains, reread on every placement (drain.go).
	drains []*drain
}

// liveSet is a scale set's client while its session is open. calls counts
// the calls to GitHub a pass makes through it after letting go of mu: its
// session closes only once they are done (RemoveSet).
type liveSet struct {
	SetClient
	calls sync.WaitGroup
}

// setName names one runner scale set: a target's, for one pool.
type setName struct{ target, pool string }

func setKey(target, pool string) string { return target + "|" + pool }

func NewService(cfg *config.Config, log *slog.Logger, launcher Launcher) (*Service, error) {
	rules, err := plan.ParseOrder(cfg)
	if err != nil {
		return nil, err
	}
	s := &Service{
		Cfg: cfg, Log: log, now: time.Now, launcher: launcher, Rules: rules,
		sets:     map[string]*liveSet{},
		wake:     make(chan struct{}, 1),
		queue:    newQueue(),
		runners:  newRunners(),
		machines: newMachines(),
		reads:    newReads(),
	}
	if err := s.prepare(cfg); err != nil {
		return nil, err
	}
	if err := s.useSetup(cfg); err != nil {
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
		for _, d := range st.Departed {
			d.Place.Machine = d.Machine
			if q, ok := cfg.Place(d.Place.ID); !ok || q.Machine != d.Machine {
				s.departed[d.Place.ID] = d.Place
			}
		}
		for id, r := range st.FailedStarts {
			if _, ok := cfg.Place(id); ok {
				s.failedStarts[id] = r
			}
		}
		maps.Copy(s.finished, st.Finished)
		log.Info("restored state", "waiting", len(s.waiting), "acquired", len(s.acquired), "unused", len(s.unused),
			"resting", len(s.unreachable), "resting places", len(s.failedStarts))
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return s, nil
}

func (s *Service) statePath() string { return filepath.Join(s.Cfg.StateDir, "state.json") }

// Setup is the setup the coordinator runs now. A setup change replaces it
// whole and never changes it in place.
func (s *Service) Setup() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cfg
}

// Wakes says when a message or a newly opened session may have given a place
// work: the serve loop then runs a pass.
func (s *Service) Wakes() <-chan struct{} { return s.wake }

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// AddSet makes a scale set's client available for acquiring and placing.
func (s *Service) AddSet(target, pool string, c SetClient) {
	s.mu.Lock()
	s.sets[setKey(target, pool)] = &liveSet{SetClient: c}
	s.mu.Unlock()
	s.poke()
}

// RemoveSet makes c, a scale set's client, unavailable once its session
// ends, and returns once no pass calls GitHub through it any more, so no job
// is acquired or runner registered through a closing session. A newer
// session's client for the same scale set stays.
func (s *Service) RemoveSet(target, pool string, c SetClient) {
	s.mu.Lock()
	ls := s.sets[setKey(target, pool)]
	if ls != nil && ls.SetClient == c {
		delete(s.sets, setKey(target, pool))
	} else {
		ls = nil
	}
	s.mu.Unlock()
	if ls != nil {
		ls.calls.Wait()
	}
}

// setLocked hands out the client of target's scale set for pool, while its
// session is open, for calls made after letting go of mu; done must be
// called once they are made.
func (s *Service) setLocked(target, pool string) (c SetClient, done func()) {
	ls := s.sets[setKey(target, pool)]
	if ls == nil {
		return nil, func() {}
	}
	ls.calls.Add(1)
	return ls.SetClient, ls.calls.Done
}

// Handle takes one message from a scale set's session: it records what
// changed and wakes the serve loop to place whatever now fits. It calls
// nothing outside the coordinator, so the session acknowledges the message
// at once.
func (s *Service) Handle(target, pool string, msg *scaleset.RunnerScaleSetMessage) {
	if msg == nil {
		return
	}
	s.mu.Lock()
	defer s.poke()
	defer s.mu.Unlock()
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
		s.drainStartedLocked(target, pool, m)
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
				// Cancelled before a runner took it: the next Tidy stops the
				// runner left waiting for nothing (stopSpare).
				s.spare[setName{target, pool}] = true
			}
			delete(s.acquired, k)
			delete(s.waiting, k)
		}
		if m.RunnerName != "" {
			s.noteFinishedLocked(m.RunnerName, m.FinishTime, now)
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
}

func (s *Service) saveLocked() {
	st := SavedState{Timings: s.timings, Unreachable: s.unreachable, FailedStarts: s.failedStarts, Finished: s.finished, Memory: s.Memory}
	for _, j := range s.waiting {
		st.Waiting = append(st.Waiting, j)
	}
	for _, a := range s.acquired {
		st.Acquired = append(st.Acquired, a)
	}
	for _, run := range s.unused {
		st.Unused = append(st.Unused, run)
	}
	for _, id := range slices.Sorted(maps.Keys(s.departed)) {
		st.Departed = append(st.Departed, departedPlace{Machine: s.departed[id].Machine, Place: s.departed[id]})
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
