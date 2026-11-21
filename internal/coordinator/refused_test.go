//go:build !windows

package coordinator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// Intent (README, "When GitHub refuses a start", and the fixed bug: while the
// sign-in's allowance was used up, GitHub refused 12 runner registrations,
// each refused job stayed acquired with no runner until it was dropped, and
// eight jobs waited 476 minutes in all for GitHub to offer them anew): when
// GitHub refuses a runner start and names how long to wait, the job waits
// again, in line from where it was, no further start is tried until that
// time, and at that time the jobs start.

// rateLimited is GitHub's refusal of a runner registration while the
// sign-in's allowance is used up, wrapped as the scale set client wraps it,
// naming the wait GitHub gave.
func rateLimited(wait time.Duration) error {
	return fmt.Errorf("failed to create new actions service request: failed to issue update token if needed: "+
		"failed to get runner registration token on refresh: %w",
		&scaleset.RateLimitError{Kind: scaleset.RateLimitPrimary, StatusCode: 403, RetryAfter: wait})
}

func TestRefusedStartWaitsForGitHubsNamedTime(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	dir := t.TempDir()
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, oneBox(dir), launcher, sets)
	h.now = f.Now

	// GitHub refuses every registration until the time it names, as it did
	// with 14m0.743s left on the used-up allowance.
	wait := 14*time.Minute + 743*time.Millisecond
	until := f.Now.Add(wait)
	refused := func() int {
		n := 0
		for _, set := range sets {
			set.mu.Lock()
			n += set.refused
			set.mu.Unlock()
		}
		return n
	}
	for _, set := range sets {
		set.refuse = func() error {
			if h.now.Before(until) {
				return rateLimited(until.Sub(h.now))
			}
			return nil
		}
	}

	// The first start is refused, and no other is tried while GitHub
	// refuses, through 50 rounds and up to a second before the named time.
	h.feed(f)
	for range 50 {
		h.round()
	}
	h.now = until.Add(-time.Second)
	h.round()
	h.want(map[string]string{})
	if n := refused(); n != 1 {
		t.Fatalf("GitHub refused %d runner starts, want 1: no start is tried while GitHub refuses", n)
	}
	if n := h.registrations(); n != 0 {
		t.Fatalf("registered %d runners while GitHub refuses, want none", n)
	}

	// At the named time the jobs start, the refused one included, as they
	// would have with no refusal.
	h.now = until
	h.round()
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	if n := refused(); n != 1 {
		t.Fatalf("GitHub refused %d runner starts, want 1", n)
	}
}

// Intent (as above): a refusal that names no wait, as a secondary limit may
// not, holds starts for a minute, the least GitHub asks for; one that names
// longer than an hour holds them an hour.
func TestRefusalWaitIsAMinuteToAnHour(t *testing.T) {
	for _, c := range []struct {
		named, want time.Duration
	}{
		{0, time.Minute},
		{4*time.Minute + 56*time.Second, 4*time.Minute + 56*time.Second},
		{3 * time.Hour, time.Hour},
	} {
		got, ok := githubWait(rateLimited(c.named))
		if !ok || got != c.want {
			t.Errorf("GitHub named %s: waits %s (refusal %v), want %s", c.named, got, ok, c.want)
		}
	}
	if _, ok := githubWait(context.DeadlineExceeded); ok {
		t.Error("a request that timed out counts as GitHub's refusal")
	}
}
