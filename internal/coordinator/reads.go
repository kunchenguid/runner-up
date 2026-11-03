package coordinator

import (
	"context"
	"time"

	"github.com/kunchenguid/runner-up/internal/plan"
)

// reads is what the coordinator read from GitHub's API: the order rules'
// pull request labels and top-level workflows, and which quiet runs GitHub
// has closed (closedruns.go). Only placement passes read, one at a time, and
// never with mu held.
type reads struct {
	// The readers the order rules need, set only when the rules use them.
	LabelReader    func(context.Context, string) (map[string]map[string]bool, error)
	WorkflowReader func(context.Context, string, int64) (string, error)
	// RunReader, set whenever GitHub's API can be read, says whether GitHub
	// has closed a workflow run, cancelled or finished, and every job of it
	// with it (closedruns.go).
	RunReader func(context.Context, string, int64) (bool, error)

	runsChecked  map[workflowRun]time.Time
	runReads     []time.Time
	labelsRead   map[string]time.Time
	labels       map[string]map[string]map[string]bool
	runWorkflows map[workflowRun]runWorkflow
}

func newReads() reads {
	return reads{
		runsChecked:  map[workflowRun]time.Time{},
		labelsRead:   map[string]time.Time{},
		labels:       map[string]map[string]map[string]bool{},
		runWorkflows: map[workflowRun]runWorkflow{},
	}
}

type workflowRun struct {
	repo string
	id   int64
}

type runWorkflow struct {
	path      string
	attempted time.Time
}

// refreshLabels reads the pull request labels of repositories with waiting
// pull request or branch jobs, at most once a minute each. Network reads
// never hold the queue lock; an unreadable refresh forgets the repository's
// labels rather than keep one that may have been removed.
func (s *Service) refreshLabels(ctx context.Context) {
	if s.LabelReader == nil {
		return
	}
	s.mu.Lock()
	if labels, _ := plan.Needs(s.Rules); !labels {
		// A setup change may have dropped the rules that read labels; labels
		// read before it would be stale once such a rule came back.
		clear(s.labels)
		clear(s.labelsRead)
		s.mu.Unlock()
		return
	}
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
	s.mu.Lock()
	if _, workflows := plan.Needs(s.Rules); !workflows {
		s.mu.Unlock()
		return
	}
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
