package coordinator

import (
	"slices"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// waitWhyAfter is how long a job waits for a place before the coordinator
// logs why it has none. A job that waits less is ordinary queueing.
const waitWhyAfter = time.Minute

// logWaitsLocked logs, for every job that has waited past waitWhyAfter with
// no free place after a placement pass, why each place that could take it
// does not: once when it is first seen, and again each time that reason
// changes. So a long wait shows its cause in the log, such as macOS VMs
// runner-up did not start filling a Mac's two (vmlimit.go).
func (s *Service) logWaitsLocked() {
	now := s.now()
	waiting := s.waitingLocked()
	v := s.snapshotLocked()
	seen := map[string]bool{}
	for _, j := range waiting {
		k := j.Key()
		seen[k] = true
		if now.Sub(j.SeenAt) <= waitWhyAfter {
			continue
		}
		why := v.waitWhy(j)
		if why == s.waitWhy[k] {
			continue
		}
		s.waitWhy[k] = why
		attrs := []any{"repo", j.Repo, "pool", j.Pool, "job", j.Name, "request", j.RequestID}
		if j.BorrowID != "" {
			attrs = []any{"borrow", j.BorrowID}
		}
		attrs = append(attrs, "waiting", now.Sub(j.QueueTime).Round(time.Second).String(), "why", why)
		s.Log.Info("no free place for a waiting job", attrs...)
	}
	for k := range s.waitWhy {
		if !seen[k] {
			delete(s.waitWhy, k)
		}
	}
}

// waitWhy says why no place takes job j now: each place serving its pool
// and why it does not, in the setup's order, such as "mini-vm1: macOS VM
// limit: 2 of 2 running, outside runner-up: proof-a, proof-b".
func (v *snapshot) waitWhy(j plan.Job) string {
	s := v.s
	if j.Drain == heldForDrain {
		return heldForDrain
	}
	now := s.now()
	var parts []string
	for _, p := range s.Cfg.Places() {
		if !slices.Contains(p.Pools, j.Pool) || j.BorrowID != "" && p.Provider != config.ProviderTart {
			continue
		}
		parts = append(parts, p.ID+": "+v.placeWhy(p, j, now))
	}
	if len(parts) == 0 {
		if j.BorrowID != "" {
			return "no tart place serves " + j.Pool
		}
		return "no place serves " + j.Pool
	}
	return strings.Join(parts, "; ")
}

// placeWhy says why place p does not take job j now, in the order the
// planner reads it (views, busy, plan.Plan).
func (v *snapshot) placeWhy(p config.Place, j plan.Job, now time.Time) string {
	s := v.s
	if busy, why := v.busy(p, false); busy {
		switch why {
		case BusyJob:
			return "runs a job"
		case BusyBorrow:
			return "lent for a borrow"
		case BusyUnreadable:
			return "its lock cannot be read"
		}
		return string(why)
	}
	switch {
	case s.restingLocked(p.Machine):
		return "its machine does not answer"
	case now.Before(s.cooling[p.ID]):
		return "resting after a failed start"
	case s.held[p.ID] != "":
		return "held: " + s.held[p.ID]
	}
	d := s.drainOf(p.ID)
	switch {
	case d != nil && d.Drained.IsZero():
		return "held while its drain empties"
	case d != nil && d.ID != j.Drain:
		return "held for a drain"
	case d == nil && j.Drain != "":
		return "outside the job's drain"
	}
	sh := j.Size
	if sh.Cores == 0 {
		sh = share(p)
	}
	if why := v.tight(p, sh); why != "" {
		return why
	}
	return "free, kept for a job ahead in line"
}
