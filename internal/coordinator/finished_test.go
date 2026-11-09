//go:build !windows

package coordinator

import (
	"slices"
	"testing"
	"time"
)

// Intent (README, "When GitHub closes a job", and the fixed bug: a Mac's
// virtual machine network stopped under a running job; GitHub failed the job
// for lost communication ten minutes later, while its runner kept trying to
// reach GitHub and held its place, five cores, toward the six-hour job
// limit): a runner still holding its place ten minutes after GitHub finished
// its job is stopped, once, and a coordinator restart keeps the finish time; a
// runner that ends on its own is never stopped, and its finish is forgotten.
// The messages are the ones GitHub sent for a job a runner in place proof-a
// took and failed (assigned-started.json).
func TestRunnerThatOutlivesItsFinishedJobIsStopped(t *testing.T) {
	proof := loadFixture(t, "assigned-started.json")
	failed := proof.Messages[3].Message.JobCompletedMessages[0].FinishTime

	// The runner ends on its own a second after GitHub failed its job.
	ordinary := newHarness(t, proofBox(t.TempDir(), "proof-a"), newFakeLauncher(), map[string]*fakeSet{})
	ordinary.feedRange(proof, 0, 3)
	markStarted(t, ordinary.cfg, "proof-a")
	ordinary.feedRange(proof, 3, 4)
	ordinary.now = failed.Add(time.Second)
	ordinary.launcher.finish(t, "proof-a")
	ordinary.round()
	ordinary.now = failed.Add(finishedGrace)
	ordinary.round()
	if len(ordinary.launcher.stopped) != 0 || len(ordinary.svc.finished) != 0 {
		t.Fatalf("stopped %v and kept finishes %v for a runner that ended on its own, want neither",
			ordinary.launcher.stopped, ordinary.svc.finished)
	}

	// The runner keeps its place after GitHub failed its job.
	dir := t.TempDir()
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, proofBox(dir, "proof-a"), launcher, sets)
	h.feedRange(proof, 0, 3)
	markStarted(t, h.cfg, "proof-a")
	h.feedRange(proof, 3, 4)
	h.now = failed.Add(finishedGrace - time.Second)
	h.round()
	if len(launcher.stopped) != 0 {
		t.Fatalf("stopped %v a second before the grace after the job's finish ends, want nothing", launcher.stopped)
	}

	// The coordinator restarts; once the grace ends it stops the place, once.
	h = newHarness(t, proofBox(dir, "proof-a"), launcher, sets)
	h.now = failed.Add(finishedGrace)
	h.round()
	h.now = h.now.Add(time.Minute)
	h.round()
	if !slices.Equal(launcher.stopped, []string{"proof-a"}) {
		t.Fatalf("stopped %v, want proof-a once", launcher.stopped)
	}

	// The runner stops and the place frees; its finish is forgotten.
	launcher.finish(t, "proof-a")
	h.round()
	h.want(map[string]string{})
	if len(h.svc.finished) != 0 {
		t.Fatalf("kept finishes %v after the place freed, want none", h.svc.finished)
	}
}
