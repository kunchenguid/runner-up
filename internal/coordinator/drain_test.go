//go:build !windows

package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// enqueueDrain publishes a drain as `runner-up drain` does, holding its live
// lock until the test closes the returned file or ends.
func enqueueDrain(t *testing.T, cfg *config.Config, r drainRequest) *os.File {
	t.Helper()
	dir := drainDir(cfg, r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := lock.Busy(filepath.Join(dir, "live.lock"))
	if err != nil || live == nil {
		t.Fatal("could not hold the drain", err)
	}
	t.Cleanup(func() { live.Close() })
	if err := config.WriteJSON(filepath.Join(dir, "request.json"), r); err != nil {
		t.Fatal(err)
	}
	return live
}

func drainStateOf(t *testing.T, cfg *config.Config, id string) drainState {
	t.Helper()
	var st drainState
	if err := config.ReadJSON(filepath.Join(drainDir(cfg, id), "state.json"), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (h *harness) wantDrain(id, phase, detail string) {
	h.t.Helper()
	if st := drainStateOf(h.t, h.cfg, id); st.Phase != phase || st.Detail != detail {
		h.t.Fatalf("drain %s is %s: %s\nwant %s: %s", id, st.Phase, st.Detail, phase, detail)
	}
}

// jobReached records that the job named job reached place id's runner, as
// the place runner does when the runner prints it.
func jobReached(t *testing.T, cfg *config.Config, id, job string) {
	t.Helper()
	path := filepath.Join(cfg.PlaceDir(id), "place.json")
	var run PlaceRun
	if err := config.ReadJSON(path, &run); err != nil {
		t.Fatal(err)
	}
	run.Started, run.RanJob = true, job
	if err := config.WriteJSON(path, run); err != nil {
		t.Fatal(err)
	}
}

// started is GitHub's message that runner took job, one of its scale set's.
func started(runner string, job scaleset.JobMessageBase) *scaleset.RunnerScaleSetMessage {
	job.MessageType = scaleset.MessageTypeJobStarted
	return &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: runner, JobMessageBase: job}}}
}

func runnerOf(t *testing.T, cfg *config.Config, id string) PlaceRun {
	t.Helper()
	var run PlaceRun
	if err := config.ReadJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// Intent ("drain new job placement, let running jobs
// finish, do the blocked step, then resume placement right away"; its goal:
// "without stopping the coordinator or affecting other places"): a drain
// keeps its places from new jobs while their running jobs finish, then
// admits only the named pair there, and gives the places back at once when
// the pair ends. A place it does not name keeps placing throughout, a named
// job waits for the drained places rather than start there, no running job is
// stopped, and a coordinator restart keeps the drain.
func TestDrainLetsRunningJobsFinishAdmitsThePairThenResumes(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	// Every place also serves ru-proof, the pool of a later recorded job.
	cfg.Pools = append(cfg.Pools, config.Pool{Label: "ru-proof"})
	for i := range cfg.Machines[0].Places {
		cfg.Machines[0].Places[i].Pools = append(cfg.Machines[0].Places[i].Pools, "ru-proof")
	}
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, cfg, launcher, sets)
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": verify, "box-2": pr184})

	enqueueDrain(t, cfg, drainRequest{ID: "release", Label: "release check", Places: []string{"box-1", "box-2"},
		Jobs: []string{"suite", "affected-journeys"}, Minutes: 90, Queued: h.now})
	h.feedRange(f, 1, len(f.Messages))
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	h.wantDrain("release", "draining", `box-1 runs octo-org/app "verify / linux"; box-2 runs octo-org/app "affected-journeys"`)

	// The large place serves ru-linux, and with no drain the hour-old suite
	// would take it next. Named, the suite waits for the drained places.
	h.finishAndPlace("box-large")
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": release})
	jobReached(t, cfg, "box-large", "Release")

	// A drained place stays free while the other still runs its job.
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-2": pr184, "box-large": release})
	h.wantDrain("release", "draining", `box-2 runs octo-org/app "affected-journeys"`)

	at := h.now
	h = newHarness(t, cfg, launcher, sets)
	h.now = at.Add(time.Minute)
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-2": pr184, "box-large": release})

	// Both places are free at once: they take the pair, the suite and PR
	// #186's affected-journeys, and nothing else.
	h.finishAndPlace("box-2")
	h.want(map[string]string{"box-1": suite, "box-2": pr184, "box-large": release})
	if run := runnerOf(t, cfg, "box-2"); run.RequestID != 9104 {
		t.Fatalf("box-2 runs request %d, want PR #186's 9104", run.RequestID)
	}
	h.wantDrain("release", "admitting", `"suite" starting in box-1, "affected-journeys" starting in box-2`)

	jobReached(t, cfg, "box-1", "suite")
	jobReached(t, cfg, "box-2", "affected-journeys")
	h.svc.Place(context.Background())
	h.wantDrain("release", "running", `"suite" runs in box-1, "affected-journeys" runs in box-2`)

	// A later job the drained places serve waits while the pair runs, even
	// once one of the pair has ended.
	later := loadFixture(t, "assigned-started.json")
	h.feedRange(later, 1, 2)
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-2": pr184, "box-large": release})
	h.wantDrain("release", "running", `"suite" ran in box-1, "affected-journeys" runs in box-2`)

	// The pair has ended: the later job takes a freed place in the same pass.
	h.finishAndPlace("box-2")
	h.want(map[string]string{"box-1": "octo-org/app linux", "box-large": release})
	h.wantDrain("release", "done", `"suite" ran in box-1, "affected-journeys" ran in box-2; placement resumed`)
	if len(launcher.stopped) != 0 {
		t.Fatalf("stopped %v, want no running job stopped", launcher.stopped)
	}
}

// Intent ("then resume placement right away"): a drain whose
// caller stops, or reaches its time limit, gives its places back at the
// next placement, even mid-drain, and the jobs running there carry on.
func TestDrainEndedByItsCallerResumesPlacementAtOnce(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 2)
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	live := enqueueDrain(t, h.cfg, drainRequest{ID: "release", Label: "release check", Places: []string{"box-1", "box-2"},
		Jobs: []string{"suite"}, Minutes: 90, Queued: h.now})
	h.feedRange(f, 2, 3)
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-2": pr184, "box-large": publish})

	live.Close()
	h.now = h.now.Add(5 * time.Second)
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": suite, "box-2": pr184, "box-large": publish})
	if _, err := os.Stat(drainDir(h.cfg, "release")); !os.IsNotExist(err) {
		t.Fatalf("the ended drain is still kept: %v", err)
	}
	if len(h.launcher.stopped) != 0 {
		t.Fatalf("stopped %v, want no running job stopped", h.launcher.stopped)
	}
}

// Intent ("admit one named pair of jobs"; README: GitHub picks which
// of a scale set's jobs a runner takes): each name admits one job at a time,
// a runner the drain started may take the other of the pair, and the pair
// counts as run wherever in the drained places GitHub put each job.
func TestDrainAdmitsOneJobPerNameAndCountsGitHubsPick(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 1, 3)
	h.want(map[string]string{"box-1": suite, "box-large": publish})
	enqueueDrain(t, h.cfg, drainRequest{ID: "release", Label: "release check", Places: []string{"box-1", "box-2"},
		Jobs: []string{"verify / linux", "affected-journeys"}, Repo: "octo-org/app", Minutes: 90, Queued: h.now})
	// PR #184's and PR #186's jobs share a name: one is admitted at a time.
	h.feedRange(f, 0, 1)
	h.feedRange(f, 4, 5)
	h.want(map[string]string{"box-1": suite, "box-large": publish})

	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	if run := runnerOf(t, h.cfg, "box-2"); run.RequestID != 9102 {
		t.Fatalf("box-2 runs request %d, want PR #184's 9102", run.RequestID)
	}

	// GitHub gives box-2's runner the verify job, and box-1's runner PR
	// #184's job.
	jobs := loadFixture(t, "waiting-jobs.json").Messages[0].Message.JobAvailableMessages
	box1, box2 := runnerOf(t, h.cfg, "box-1"), runnerOf(t, h.cfg, "box-2")
	h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", started(box2.RunnerName, jobs[0].JobMessageBase))
	h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", started(box1.RunnerName, jobs[1].JobMessageBase))
	h.wantDrain("release", "running", `"verify / linux" runs in box-2, "affected-journeys" runs in box-1`)

	// PR #186's job, named the same, waits for the drain to end.
	h.launcher.finish(t, "box-1")
	h.finishAndPlace("box-2")
	h.want(map[string]string{"box-1": pr184, "box-large": publish})
	if run := runnerOf(t, h.cfg, "box-1"); run.RequestID != 9104 {
		t.Fatalf("box-1 runs request %d, want PR #186's 9104 once the drain ended", run.RequestID)
	}
}

// Intent ("then resume placement right away"): a named job GitHub
// gives a runner outside the drain will not wait again, so the drain stops
// waiting for it rather than hold its places to its time limit.
func TestDrainStopsWaitingForANamedJobThatStartedElsewhere(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.feedRange(f, 4, 5)
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": pr184})
	enqueueDrain(t, h.cfg, drainRequest{ID: "release", Label: "release check", Places: []string{"box-1"},
		Jobs: []string{"affected-journeys"}, Repo: "octo-org/app", Minutes: 90, Queued: h.now})
	h.svc.Place(context.Background())

	// GitHub gives the large place's runner PR #184's job.
	jobs := f.Messages[0].Message.JobAvailableMessages
	h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", started(runnerOf(t, h.cfg, "box-large").RunnerName, jobs[1].JobMessageBase))
	h.wantDrain("release", "draining", `box-1 runs octo-org/app "verify / linux"`)
	h.finishAndPlace("box-1")
	h.wantDrain("release", "done", `"affected-journeys" started in box-large, outside the drain; placement resumed`)
}

// Intent ("do not wait for a quiet hour"; README: on a machine whose
// places share its cores and memory, the named jobs keep room ahead of other
// work): a drained place's named job takes the room its drain freed, even
// when an older job elsewhere on the machine waits for more room.
func TestDrainedJobsTakeTheRoomAheadOfOtherWork(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	// Room for the two small places, or for the large one.
	cfg.Machines[0].Cores, cfg.Machines[0].RoomCores = 5, 1
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 2)
	h.want(map[string]string{"box-1": verify, "box-2": pr184})
	enqueueDrain(t, h.cfg, drainRequest{ID: "release", Label: "release check", Places: []string{"box-1"},
		Jobs: []string{"suite"}, Minutes: 90, Queued: h.now})
	h.feedRange(f, 2, 3)

	// The publish job, older, waits for the large place's 4 cores.
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-1": suite, "box-2": pr184})
}
