//go:build !windows

package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "When a place fails to start", and the fixed bug: a Mac's
// virtual machine network broke, every clone stopped while booting, and the
// coordinator gave the same place the same job again every few seconds, 777
// times in 50 minutes, registering and removing a runner at GitHub each
// time): a place whose runner failed to start takes no job for a minute, twice
// as long after each further failure in a row up to fifteen minutes, and its
// job goes to another place meanwhile; a run there that starts ends the
// count; a coordinator restart keeps the rest.

// failStart ends place id's runner as one whose agent said the place failed
// before a job reached the runner, which the place runner records in
// place.json before it exits.
func (l *fakeLauncher) failStart(t *testing.T, id string) {
	t.Helper()
	l.mu.Lock()
	path := filepath.Join(l.dirs[id], "place.json")
	l.mu.Unlock()
	var run PlaceRun
	if err := config.ReadJSON(path, &run); err != nil {
		t.Fatal(err)
	}
	run.StartFailed = true
	config.WriteJSON(path, run)
	l.giveUp(t, id)
}

func TestPlaceWhoseRunnerFailsToStartRests(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	dir := t.TempDir()
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, oneBox(dir), launcher, sets)
	h.now = f.Now
	h.feed(f)
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})

	// box-1's runner fails to start. A placement pass reaches box-1 before
	// any Tidy does, then 50 rounds pass with no time passing, as fast as the
	// serve loop runs: box-1 takes no job and nothing is registered.
	registered := h.registrations()
	launcher.failStart(t, "box-1")
	h.svc.Place(context.Background())
	for range 50 {
		h.round()
	}
	h.want(map[string]string{"box-2": pr184, "box-large": publish})
	if n := h.registrations() - registered; n != 0 {
		t.Fatalf("made %d runner registrations while box-1 rests, want none", n)
	}

	// Its job waits again, first in line: box-2 frees and verify takes it.
	launcher.finish(t, "box-2")
	h.round()
	h.want(map[string]string{"box-2": verify, "box-large": publish})

	// Each further failure in a row rests box-1 twice as long, up to fifteen
	// minutes. A second before each rest ends box-1 is still free; once it
	// ends box-1 takes the next job with one registration.
	failed := h.now
	for _, rest := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		h.now = failed.Add(rest - time.Second)
		h.round()
		if got := h.placed()["box-1"]; got != "" {
			t.Fatalf("box-1 runs %q a second before its %s rest ends, want it free", got, rest)
		}
		registered = h.registrations()
		h.now = failed.Add(rest)
		h.round()
		if got := h.placed()["box-1"]; got != suite || h.registrations() != registered+1 {
			t.Fatalf("box-1 runs %q after %d registrations once its %s rest ends, want the suite after one",
				got, h.registrations()-registered, rest)
		}
		launcher.failStart(t, "box-1")
		h.round()
		failed = h.now
	}

	// The coordinator restarts while box-1 rests fifteen minutes: box-1 still
	// rests.
	h = newHarness(t, oneBox(dir), launcher, sets)
	h.now = failed.Add(15*time.Minute - time.Second)
	h.round()
	if got := h.placed()["box-1"]; got != "" {
		t.Fatalf("after a restart box-1 runs %q before its rest ends, want it free", got)
	}

	// Its next runner starts its job: the count ends, and the failure after
	// it rests box-1 a minute again.
	h.now = failed.Add(15 * time.Minute)
	h.round()
	launcher.finish(t, "box-1")
	h.round()
	launcher.failStart(t, "box-1")
	h.round()
	failed = h.now
	h.now = failed.Add(time.Minute)
	h.round()
	if got := h.placed()["box-1"]; got == "" {
		t.Fatal("box-1 is still free a minute after its first failure since a runner started there")
	}
}
