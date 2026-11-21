package coordinator

import (
	"errors"
	"time"

	"github.com/actions/scaleset"
)

// GitHub refuses to acquire a job or register a runner while the sign-in has
// used up its allowance, or makes requests too fast, and names how long to
// wait. Every scale set signs in the same way, so the refusal holds for all
// of them: until the time GitHub named, no job GitHub offered is acquired and
// no runner registered, and the job whose start GitHub refused waits again,
// in line from where it was. No place rests for it, since no place failed.
// Once the time passes, the next placement pass tries again. Before, a job
// whose runner GitHub refused to register stayed acquired with no runner and
// was dropped once its start allowance passed, until GitHub offered it anew,
// hours later. Borrows call no GitHub and are not held.

const (
	// refusalMin is the least a refusal holds starts for, when GitHub names
	// no time or one shorter: GitHub asks for at least a minute after a
	// secondary limit.
	refusalMin = time.Minute
	// refusalMax is the most a refusal holds starts for: a used-up allowance
	// comes back within the hour.
	refusalMax = time.Hour
)

// githubWait says how long GitHub asks the coordinator to wait before it tries
// a refused request again, and whether err is such a refusal.
func githubWait(err error) (time.Duration, bool) {
	var rl *scaleset.RateLimitError
	if !errors.As(err, &rl) {
		return 0, false
	}
	return min(max(rl.RetryAfter, refusalMin), refusalMax), true
}

// refuseStartsLocked holds every start GitHub needs until wait from now, or
// longer if an earlier refusal holds them longer.
func (s *Service) refuseStartsLocked(wait time.Duration) time.Time {
	if until := s.now().Add(wait); until.After(s.startsRefused) {
		s.startsRefused = until
	}
	return s.startsRefused
}

// startsRefusedLocked says whether GitHub refuses starts at now.
func (s *Service) startsRefusedLocked(now time.Time) bool {
	return now.Before(s.startsRefused)
}
