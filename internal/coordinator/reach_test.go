//go:build !windows

package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (RUU-15, found planning rented Macs: "When a machine's agent cannot
// be reached (SSH refused or timing out), runner-up stops giving that
// machine's places jobs until it answers again, makes no new runner
// registration in the meantime, and the job it held goes to a healthy
// place"). Before, the coordinator ran Tidy and Place again the moment a
// place runner ended, and the dead machine, counting no busy place, took the
// oldest waiting job straight back: 50 refusals made 50 registrations with no
// time passing, while a younger job took a freed healthy place.

// cannotReach ends place id's runner as one whose ssh to its machine failed,
// which the place runner records in place.json before it exits.
func (l *fakeLauncher) cannotReach(t *testing.T, id string) {
	t.Helper()
	l.mu.Lock()
	path := filepath.Join(l.dirs[id], "place.json")
	l.mu.Unlock()
	var run PlaceRun
	if err := config.ReadJSON(path, &run); err != nil {
		t.Fatal(err)
	}
	run.Unreachable = true
	config.WriteJSON(path, run)
	l.giveUp(t, id)
}

// farAgent stands in for the far machine's agent answering over SSH.
type farAgent struct {
	mu      sync.Mutex
	answers bool
	asked   int
}

func (a *farAgent) reach(_ context.Context, m config.Machine) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.Name != "far" {
		return errors.New("asked " + m.Name + ", which never failed")
	}
	a.asked++
	if !a.answers {
		return errors.New("ssh: connect to host far port 22: Connection refused")
	}
	return nil
}

func (a *farAgent) times() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// withFar adds a machine reached over SSH with one small place, listed last.
func withFar(cfg *config.Config) *config.Config {
	cfg.Machines = append(cfg.Machines, config.Machine{Name: "far", SSH: []string{"m1@far"}, Places: []config.Place{
		{ID: "far-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}})
	return cfg
}

// registrations counts the runners registered with every scale set.
func (h *harness) registrations() int {
	n := 0
	for _, set := range h.sets {
		set.mu.Lock()
		n += set.nextID
		set.mu.Unlock()
	}
	return n
}

// round is the main loop's answer to a place runner ending (cmd/main.go):
// Tidy and Place at once, then any answer from a resting machine's agent.
func (h *harness) round() {
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
	h.svc.asking.Wait()
}

func TestUnreachableMachineRestsUntilItsAgentAnswers(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	dir := t.TempDir()
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, withFar(oneBox(dir)), launcher, sets)
	far := &farAgent{}
	h.svc.Reach = far.reach
	h.now = f.Now
	h.feed(f)
	// The box's three places are busy, so far, idle, takes PR #184's job.
	h.want(map[string]string{"box-1": verify, "box-2": suite, "box-large": publish, "far-1": pr184})

	// Far refuses SSH. Its runner ends, unused; then 50 rounds pass with no
	// time passing, as fast as the main loop runs.
	registered := h.registrations()
	launcher.cannotReach(t, "far-1")
	for range 50 {
		h.round()
	}
	h.want(map[string]string{"box-1": verify, "box-2": suite, "box-large": publish})
	if n := h.registrations() - registered; n != 0 {
		t.Fatalf("made %d runner registrations while far rests, want none", n)
	}
	app := h.sets[setKey("octo-org/app", "ru-linux")]
	if !slices.Equal(app.acquired, []int64{9101, 9102}) || len(app.removed) != 1 {
		t.Fatalf("app acquired %v and removed %d runners, want 9101 and 9102 once each and far's runner removed", app.acquired, len(app.removed))
	}
	if far.times() != 0 {
		t.Fatalf("asked far's agent %d times within a minute of the refusal, want none", far.times())
	}

	// A healthy place frees: the job far held, older, takes it before PR
	// #186's.
	h.launcher.finish(t, "box-1")
	h.round()
	var run PlaceRun
	config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &run)
	if run.RequestID != 9102 {
		t.Fatalf("box-1 runs request %d, want 9102, the job far held", run.RequestID)
	}

	// A minute on, far is asked once, still refuses, and keeps resting.
	h.now = h.now.Add(reachAskEvery)
	h.round()
	h.round()
	if far.times() != 1 || h.placed()["far-1"] != "" {
		t.Fatalf("asked far %d times and far-1 runs %q, want one question and far-1 empty", far.times(), h.placed()["far-1"])
	}

	// The coordinator restarts while far rests: it still rests, and is
	// asked at once.
	h2 := newHarness(t, withFar(oneBox(dir)), launcher, sets)
	h2.svc.Reach = far.reach
	h2.now = h.now.Add(10 * time.Second)
	registered = h2.registrations()
	h2.round()
	if far.times() != 2 || h2.placed()["far-1"] != "" || h2.registrations() != registered {
		t.Fatalf("after the restart far was asked %d times in all and far-1 runs %q, want two and empty", far.times(), h2.placed()["far-1"])
	}

	// Far comes back. A minute on its agent answers, and far-1 takes PR
	// #186's job with one new registration.
	far.mu.Lock()
	far.answers = true
	far.mu.Unlock()
	h2.now = h2.now.Add(reachAskEvery)
	h2.round()
	h2.svc.Place(context.Background())
	if got := h2.placed()["far-1"]; got != pr184 || h2.registrations() != registered+1 {
		t.Fatalf("far-1 runs %q after %d new registrations, want PR #186's job after one", got, h2.registrations()-registered)
	}
}
