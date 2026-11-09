package coordinator

import (
	"strconv"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// A place whose runner fails to start rests. Its place runner records it when
// the place's agent does not start, or says the place failed before a job
// reached the runner and the place runner had not asked it to stop: a virtual
// machine that stopped while booting or did not come up in time, a container
// docker would not start. The same place given the same job again at once
// most likely fails the same way, and each try registers and removes a runner
// at GitHub. So the place takes no job for restFirst, twice as long
// after each further failure in a row, up to restMax. The job its runner was
// started for waits again, in line from where it was, and goes to another
// place meanwhile. A job's run there that does not fail to start ends the
// count. The rests are saved, so a coordinator restart keeps them. An agent
// killed outright says nothing, and its place takes a job at once; an agent
// lost with its machine rests the machine instead (reach.go).

// PlaceRest is a place resting after its runner failed to start Failures
// times in a row: it takes no job until Until.
type PlaceRest struct {
	Failures int       `json:"failures"`
	Until    time.Time `json:"until"`
}

const (
	// restFirst is how long a place rests after its runner first fails to
	// start.
	restFirst = time.Minute
	// restMax is the longest a place rests after failures in a row.
	restMax = 15 * time.Minute
)

// restFor is how long a place rests after its runner failed to start
// failures times in a row: restFirst, doubling with each further failure, up
// to restMax.
func restFor(failures int) time.Duration {
	rest := restFirst
	for range failures - 1 {
		rest *= 2
		if rest >= restMax {
			return restMax
		}
	}
	return rest
}

// noteStartLocked keeps what the last run of place p, a job's, says of its
// start, as retireLocked finds it: a runner that failed to start rests the
// place; any other run there ends the count of failures in a row.
func (s *Service) noteStartLocked(p config.Place, run PlaceRun) {
	if run.BorrowID != "" {
		return
	}
	if _, ok := s.Cfg.Place(p.ID); !run.StartFailed || !ok {
		if _, resting := s.failedStarts[p.ID]; resting {
			delete(s.failedStarts, p.ID)
			s.saveLocked()
		}
		return
	}
	r := s.failedStarts[p.ID]
	r.Failures++
	rest := restFor(r.Failures)
	r.Until = s.now().Add(rest)
	s.failedStarts[p.ID] = r
	s.Log.Warn("the place's runner failed to start; the place rests", "place", p.ID, "failures", r.Failures,
		"rest", rest.String(), "runner", run.RunnerName)
	s.saveLocked()
}

// FailedStarts says how many times in a row a place's runner failed to
// start, in words: "1 failed start", "5 failed starts".
func (r PlaceRest) FailedStarts() string {
	if r.Failures == 1 {
		return "1 failed start"
	}
	return strconv.Itoa(r.Failures) + " failed starts"
}

// placeRestsLocked says whether place id takes no job at now because its
// last launch failed (assignLocked) or its runner failed to start.
func (s *Service) placeRestsLocked(id string, now time.Time) bool {
	return now.Before(s.cooling[id]) || now.Before(s.failedStarts[id].Until)
}
