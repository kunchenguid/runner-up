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
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (the design: "unit tests replaying recorded scale set messages"):
// GitHub sends most jobs as JobAssigned messages with runner request 0 and no
// JobAvailable message at all. A job GitHub assigns to a scale set gets a
// runner in a free place, keeps its place in line and its starting runner
// while GitHub takes the assignment back and assigns it again, lets go of both
// once it is cancelled, and goes back in line when GitHub gives its runner
// another of the set's jobs or its runner ends without it.

// The recorded job, as the place runs it.
const proofJob = "octo-org/app linux"

// proofBox is one machine whose places serve the scale set the messages were
// recorded from.
func proofBox(dir string, ids ...string) *config.Config {
	m := config.Machine{Name: "box", SSH: []string{"ci@box"}}
	for _, id := range ids {
		m.Places = append(m.Places, config.Place{ID: id, Provider: config.ProviderDocker, Pools: []string{"ru-proof"}, Cores: 2, MemoryMB: 4096})
	}
	return &config.Config{StateDir: dir, GitHub: config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool"}},
		Pools: []config.Pool{{Label: "ru-proof"}}, Machines: []config.Machine{m}}
}

// occupy holds a place's lock, as another job's runner would.
func occupy(t *testing.T, cfg *config.Config, id string) *os.File {
	t.Helper()
	if err := os.MkdirAll(cfg.PlaceDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := lock.Busy(filepath.Join(cfg.PlaceDir(id), "busy.lock"))
	if err != nil || f == nil {
		t.Fatal("could not occupy", id, err)
	}
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), PlaceRun{Repo: "octo-user/tool", JobName: "suite", Started: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestAssignedJobGetsARunner(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	h := newHarness(t, proofBox(t.TempDir(), "box-1", "box-2"), newFakeLauncher(), map[string]*fakeSet{})
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": proofJob})
	// GitHub already gave the job to the scale set: there is nothing to
	// acquire, and request 0 names no job.
	if got := h.sets[setKey("octo-org/app", "ru-proof")].acquired; len(got) != 0 {
		t.Fatalf("acquired %v", got)
	}
}

func TestReassignedJobKeepsItsPlaceInLine(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	cfg := proofBox(t.TempDir(), "box-1", "box-2")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	one := occupy(t, cfg, "box-1")
	occupy(t, cfg, "box-2")

	// Every place is busy for the ten minutes GitHub assigns the job three
	// times: it stays one waiting job, in line from its first assignment.
	h.feedRange(f, 0, 5)
	if len(h.svc.waiting) != 1 {
		t.Fatalf("%d waiting jobs, want one: %v", len(h.svc.waiting), h.svc.waiting)
	}
	for _, j := range h.svc.waiting {
		if !j.QueueTime.Equal(f.Messages[0].At) {
			t.Fatalf("the job is in line from %s, want its first assignment at %s", j.QueueTime, f.Messages[0].At)
		}
	}

	// A place frees: the job gets its runner.
	one.Close()
	h.now = h.now.Add(time.Minute)
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": proofJob, "box-2": suite})
}

func TestReassignmentKeepsTheStartingRunner(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	h := newHarness(t, proofBox(t.TempDir(), "box-1", "box-2"), newFakeLauncher(), map[string]*fakeSet{})
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": proofJob})

	// Its place still starting, GitHub takes the assignment back and assigns
	// the job again, twice. The starting runner will take it: no second
	// runner starts, and the first one is not stopped.
	h.feedRange(f, 1, 5)
	h.now = h.now.Add(time.Minute)
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": proofJob})
	if len(h.launcher.stopped) != 0 {
		t.Fatalf("stopped %v", h.launcher.stopped)
	}
}

func TestCancelledAssignedJobLetsGoOfItsPlace(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	h := newHarness(t, proofBox(t.TempDir(), "box-1", "box-2"), newFakeLauncher(), map[string]*fakeSet{})

	// The job gets a runner, is taken back and assigned again twice, then
	// the run is cancelled before the runner took it. The spare runner is
	// stopped.
	h.feed(f)
	if len(h.launcher.stopped) != 1 || h.launcher.stopped[0] != "box-1" {
		t.Fatalf("stopped %v, want the spare runner in box-1", h.launcher.stopped)
	}

	// Cancelled while it waited for a place, it is forgotten: a freed place
	// starts nothing for it.
	cfg2 := proofBox(t.TempDir(), "box-1", "box-2")
	h2 := newHarness(t, cfg2, newFakeLauncher(), map[string]*fakeSet{})
	one := occupy(t, cfg2, "box-1")
	occupy(t, cfg2, "box-2")
	h2.feed(f)
	one.Close()
	h2.now = h2.now.Add(time.Minute)
	h2.svc.Place(context.Background())
	h2.want(map[string]string{"box-2": suite})
	if len(h2.svc.waiting) != 0 {
		t.Fatalf("still waiting: %v", h2.svc.waiting)
	}
}

func TestRunnerThatTakesAnotherJobHandsItsOwnBack(t *testing.T) {
	takebacks := loadFixture(t, "assigned-takebacks.json")
	proof := loadFixture(t, "assigned-started.json")
	cfg := proofBox(t.TempDir(), "proof-a")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	first := takebacks.Messages[0].Message.JobAssignedMessages[0]
	firstKey := plan.RunKey("octo-org/app", "ru-proof", first.WorkflowRunID, first.JobDisplayName)
	started := proof.Messages[2].Message.JobStartedMessages[0]

	// The first recorded job and the second run's job wait for the one
	// place, the first one first in line. The place frees the moment the
	// second run's job is assigned, and its runner is started for the first.
	busy := occupy(t, cfg, "proof-a")
	h.feedRange(takebacks, 0, 1)
	h.feedRange(proof, 1, 2)
	busy.Close()
	h.svc.Place(context.Background())
	var run PlaceRun
	if err := config.ReadJSON(filepath.Join(cfg.PlaceDir("proof-a"), "place.json"), &run); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.svc.acquired[firstKey]; !ok || run.RunnerName != started.RunnerName {
		t.Fatalf("runner %s started for %v, want %s for the first job", run.RunnerName, h.svc.acquired, started.RunnerName)
	}

	// GitHub gives that runner the second run's job, as it recorded: the
	// first job waits again, still first in line.
	h.feedRange(proof, 2, 3)
	markStarted(t, cfg, "proof-a")
	j, ok := h.svc.waiting[firstKey]
	if !ok || !j.QueueTime.Equal(takebacks.Messages[0].At) {
		t.Fatalf("waiting %v, want the first job in line from %s", h.svc.waiting, takebacks.Messages[0].At)
	}

	// The second run's job ends; the first job gets the place.
	h.feedRange(proof, 3, 4)
	h.finishAndPlace("proof-a")
	if a, ok := h.svc.acquired[firstKey]; !ok || a.Place != "proof-a" {
		t.Fatalf("acquired %v, want the first job in proof-a", h.svc.acquired)
	}
	h.want(map[string]string{"proof-a": proofJob})
}

// Intent (DOP-200: "a runner whose job another took counts as free at
// hand-back so no surplus runner idles to the timeout"): the runner started
// for verify takes PR #184's job, whose own runner is still waiting. That
// waiting runner is free now and takes verify: no third runner starts in the
// free large place, and once it takes verify nothing waits.
func TestRunnerWhoseJobAnotherTookServesTheJobHandedBack(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": verify, "box-2": pr184})
	runner := func(id string) string {
		var run PlaceRun
		if err := config.ReadJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), &run); err != nil {
			t.Fatal(err)
		}
		return run.RunnerName
	}
	jobs := f.Messages[0].Message.JobAvailableMessages
	start := func(job int, place string) {
		t.Helper()
		base := jobs[job].JobMessageBase
		base.MessageType = scaleset.MessageTypeJobStarted
		h.now = h.now.Add(time.Second)
		if err := h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", &scaleset.RunnerScaleSetMessage{
			JobStartedMessages: []*scaleset.JobStarted{{RunnerName: runner(place), JobMessageBase: base}}}); err != nil {
			t.Fatal(err)
		}
		markStarted(t, cfg, place)
	}

	// GitHub gives box-1's runner PR #184's job.
	start(1, "box-1")
	verifyKey := plan.RequestKey("octo-org/app", "ru-linux", jobs[0].RunnerRequestID)
	if a, ok := h.svc.acquired[verifyKey]; !ok || a.Place != "box-2" || len(h.svc.waiting) != 0 {
		t.Fatalf("acquired %v, waiting %v; want verify on box-2's runner and nothing waiting", h.svc.acquired, h.svc.waiting)
	}
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": verify, "box-2": pr184})

	// box-2's runner takes verify: no job is left without a runner.
	start(0, "box-2")
	if len(h.svc.acquired) != 0 || len(h.svc.waiting) != 0 {
		t.Fatalf("acquired %v, waiting %v; want both jobs running", h.svc.acquired, h.svc.waiting)
	}
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": verify, "box-2": pr184})
}

// GitHub assigns a job again and takes back its previous assignment in one
// message; the take-back names the earlier assignment's job id and must not
// hold the new one, or the job is never placed when its place frees.
func TestTakeBackOfAnEarlierAssignmentLeavesTheNewOne(t *testing.T) {
	f := loadFixture(t, "shared-reassigned.json")
	cfg := proofBox(t.TempDir(), "proof-b")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	history := f.Messages[2].Message.JobAssignedMessages[0]
	historyKey := plan.RunKey("octo-org/app", "ru-proof", history.WorkflowRunID, history.JobDisplayName)

	// tool's job takes the one place.
	h.feedRange(f, 0, 2)
	markStarted(t, cfg, "proof-b")
	h.want(map[string]string{"proof-b": "octo-user/tool suite / Full test suite"})

	// The history job is assigned, then assigned again twice, each time
	// with the take-back of the assignment before it: it waits, in line
	// from its first assignment, held by none of those take-backs.
	h.feedRange(f, 2, 5)
	j, ok := h.svc.waiting[historyKey]
	if !ok || j.TakenBack || !j.QueueTime.Equal(f.Messages[2].At) {
		t.Fatalf("waiting %v, want the history job in line from %s and not taken back", h.svc.waiting, f.Messages[2].At)
	}

	// tool's job ends: the history job gets the place, and the job's
	// successful run teaches its estimate.
	h.feedRange(f, 5, 6)
	h.finishAndPlace("proof-b")
	h.want(map[string]string{"proof-b": "octo-org/app history"})
	learned := h.svc.timings["octo-user/tool|ru-proof|octo-user/tool/.github/workflows/suite.yml|suite / Full test suite"]
	if learned < time.Hour || learned > 70*time.Minute {
		t.Fatalf("learned %s for the suite, want its 68-minute run", learned)
	}
}

// GitHub keeps a job queued and assigned to the scale set once the runner
// started for it fails to come up, and sends nothing more. A job whose runner
// ends without starting it waits again, in line from its first assignment,
// and the next free place starts a runner for it: when its place gives up
// at the boot timeout, and when its runner outlives the start and boot
// allowance and a placement runs before Tidy.
func TestJobWhoseRunnerNeverStartedWaitsAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lives time.Duration
		tidy  bool
	}{
		{"place gave up, Tidy first", 5 * time.Minute, true},
		{"runner outlives the allowance, placement first", 16 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := loadFixture(t, "assigned-takebacks.json")
			cfg := proofBox(t.TempDir(), "box-1", "box-2")
			h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
			occupy(t, cfg, "box-2")
			h.feedRange(f, 0, 1)
			h.want(map[string]string{"box-1": proofJob, "box-2": suite})
			var first PlaceRun
			if err := config.ReadJSON(filepath.Join(cfg.PlaceDir("box-1"), "place.json"), &first); err != nil {
				t.Fatal(err)
			}

			// The runner ends without a job; GitHub sends nothing more.
			h.now = h.now.Add(tc.lives)
			if !tc.tidy {
				// Its allowance passes while it still lives.
				h.svc.Place(context.Background())
			}
			h.launcher.giveUp(t, "box-1")
			if tc.tidy {
				h.svc.Tidy(context.Background())
			}
			h.svc.Place(context.Background())
			h.want(map[string]string{"box-1": proofJob, "box-2": suite})

			assigned := f.Messages[0].Message.JobAssignedMessages[0]
			a, ok := h.svc.acquired[plan.RunKey("octo-org/app", "ru-proof", assigned.WorkflowRunID, assigned.JobDisplayName)]
			if !ok || a.Place != "box-1" || !a.Job.QueueTime.Equal(f.Messages[0].At) {
				t.Fatalf("acquired %v waiting %v, want the job placed again in box-1, in line from %s",
					h.svc.acquired, h.svc.waiting, f.Messages[0].At)
			}
			var again PlaceRun
			if err := config.ReadJSON(filepath.Join(cfg.PlaceDir("box-1"), "place.json"), &again); err != nil {
				t.Fatal(err)
			}
			if again.RunnerName == first.RunnerName {
				t.Fatalf("box-1 still names the first runner %s", first.RunnerName)
			}
		})
	}
}
