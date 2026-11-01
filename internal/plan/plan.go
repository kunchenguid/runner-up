// Package plan orders waiting jobs by the configured rules.
package plan

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Job is waiting work: a job offered or assigned by a scale set, or a local borrow.
type Job struct {
	// BorrowID identifies a local borrow; it competes with jobs but takes only tart places.
	BorrowID string `json:"borrowId,omitempty"`
	// Target is the repository or organization whose scale set has the job;
	// Repo is the repository the job belongs to, the same for a repository's
	// scale set.
	Target    string    `json:"target"`
	Pool      string    `json:"pool"`
	Repo      string    `json:"repo"`
	RequestID int64     `json:"requestId,omitempty"`
	QueueTime time.Time `json:"queueTime"`
	Name      string    `json:"name"`
	Workflow  string    `json:"workflow"`
	RunID     int64     `json:"runId"`
	SeenAt    time.Time `json:"seenAt"`
	// Assigned: GitHub assigned the job to its scale set itself, so there is
	// nothing to acquire; any runner the set starts may take it.
	Assigned bool `json:"assigned,omitempty"`
	// TakenBack: GitHub took the job's assignment back, no runner having
	// reached it, and has not assigned it again yet. Until it does, no runner
	// the set starts would get it.
	TakenBack bool `json:"takenBack,omitempty"`
	// JobID names GitHub's latest offer or assignment of the job: each
	// assignment has a new one, and a take-back names the one it takes back.
	JobID string `json:"jobId,omitempty"`
	// HeardAt is when GitHub last sent anything about the job: an offer, an
	// assignment or a take-back. GitHub takes an assigned job no runner took
	// back after five minutes, so a job GitHub has long stopped mentioning
	// may belong to a run it has closed.
	HeardAt time.Time `json:"heardAt,omitempty"`

	// Filled in before planning, never saved.
	// Estimate is a borrow's declared duration, or a job's last successful wall
	// time from runner assignment to finish; zero for a job with no successful run.
	Estimate time.Duration `json:"-"`
	// RunWorkflow is the top-level workflow path read from the run's metadata.
	RunWorkflow string `json:"-"`
	// Labels are the labels of the open pull request the job's ref belongs to.
	Labels map[string]bool `json:"-"`
}

// A job GitHub offers (JobAvailable) is named by its runner request. A job it
// assigns to the scale set itself (JobAssigned) comes with request 0, and with
// a new job id each time it assigns the job again, so it is named by its
// workflow run and name, which every assignment of it shares.
func RequestKey(target, pool string, requestID int64) string {
	return fmt.Sprintf("%s|%s|%d", target, pool, requestID)
}

func RunKey(target, pool string, runID int64, name string) string {
	return fmt.Sprintf("%s|%s|run %d|%s", target, pool, runID, name)
}

// Heard is when GitHub last sent anything about the job; a job saved before
// HeardAt was kept counts from when it was first seen.
func (j Job) Heard() time.Time {
	if j.HeardAt.IsZero() {
		return j.SeenAt
	}
	return j.HeardAt
}

func (j Job) Key() string {
	if j.BorrowID != "" {
		return "borrow|" + j.BorrowID
	}
	if j.RequestID != 0 {
		return RequestKey(j.Target, j.Pool, j.RequestID)
	}
	return RunKey(j.Target, j.Pool, j.RunID, j.Name)
}

// WorkflowPath drops the branch or tag from a workflow ref, so a successful
// run teaches every branch's runs.
func WorkflowPath(ref string) string {
	p, _, _ := strings.Cut(ref, "@")
	return p
}

// TimingKey scopes a learned estimate by repository, pool, workflow file and
// the job's full display name, matrix values included.
func (j Job) TimingKey() string {
	return j.Repo + "|" + j.Pool + "|" + WorkflowPath(j.Workflow) + "|" + j.Name
}

// PlaceView is what the planner needs to know about a place right now.
type PlaceView struct {
	config.Place
	Busy bool
}

func (v PlaceView) share() config.Share { return config.Share{Cores: v.Cores, MemoryMB: v.MemoryMB} }

// Headroom is what a counted machine's places may still reserve right now:
// its budget less the shares of places with held locks. A machine that counts
// no memory has Memory false.
type Headroom struct {
	Cores, MemoryMB int
	Memory          bool
}

func (h Headroom) fits(sh config.Share) bool {
	return sh.Cores <= h.Cores && (!h.Memory || sh.MemoryMB <= h.MemoryMB)
}

func (h Headroom) less(sh config.Share) Headroom {
	h.Cores -= sh.Cores
	h.MemoryMB -= sh.MemoryMB
	return h
}

type Assignment struct {
	Job   Job
	Place config.Place
}

// Rank orders waiting jobs by the order rules (rules.go); first-in, first-out
// breaks every tie.
func Rank(waiting []Job, rules []Rule, now time.Time) []Job {
	jobs := append([]Job(nil), waiting...)
	sort.SliceStable(jobs, func(a, b int) bool { return before(jobs[a], jobs[b], rules, now) })
	return jobs
}

func fifo(a, b Job) bool {
	if !a.QueueTime.Equal(b.QueueTime) {
		return a.QueueTime.Before(b.QueueTime)
	}
	return a.Key() < b.Key()
}

// Plan gives free places to waiting jobs. Jobs are taken in rank order; each
// takes a free place serving its pool whose share fits what its machine has
// left, preferring the least-busy machine, then the order places are listed
// in. No running job is interrupted, and a job no place can take holds
// nothing, so a smaller job behind it may take a place it cannot.
func Plan(waiting []Job, places []PlaceView, headroom map[string]Headroom, rules []Rule, now time.Time) []Assignment {
	jobs := Rank(waiting, rules, now)
	left := map[string]Headroom{}
	for m, h := range headroom {
		left[m] = h
	}
	busy := map[string]int{}
	var free []PlaceView
	for _, p := range places {
		if p.Busy {
			busy[p.Machine]++
		} else {
			free = append(free, p)
		}
	}
	var out []Assignment
	for _, j := range jobs {
		best := -1
		for i, p := range free {
			if !slices.Contains(p.Pools, j.Pool) || j.BorrowID != "" && p.Provider != config.ProviderTart {
				continue
			}
			if h, ok := left[p.Machine]; ok && !h.fits(p.share()) {
				continue
			}
			if best < 0 || busy[p.Machine] < busy[free[best].Machine] {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		p := free[best]
		free = append(free[:best:best], free[best+1:]...)
		busy[p.Machine]++
		if h, ok := left[p.Machine]; ok {
			left[p.Machine] = h.less(p.share())
		}
		out = append(out, Assignment{Job: j, Place: p.Place})
	}
	return out
}
