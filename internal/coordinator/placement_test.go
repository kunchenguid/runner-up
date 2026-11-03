//go:build !windows

package coordinator

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the coordinator's design, service.go): a placement pass never
// holds the coordinator's lock through a call to GitHub, so a scale set's
// messages and every other reader carry on while GitHub is slow to answer;
// and a scale set's session never closes while a pass acquires a job or
// registers a runner through it.

// slowSet is a scale set whose GitHub answers acquire calls only once the
// test releases them.
type slowSet struct {
	*fakeSet
	asked   chan struct{}
	once    sync.Once
	release chan struct{}
}

func newSlowSet() *slowSet {
	return &slowSet{fakeSet: &fakeSet{gone: map[int64]bool{}}, asked: make(chan struct{}), release: make(chan struct{})}
}

func (s *slowSet) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	s.once.Do(func() { close(s.asked) })
	<-s.release
	return s.fakeSet.AcquireJobs(ctx, ids)
}

// within fails the test unless f returns within d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s", what, d)
	}
}

func TestPlacementLetsGoOfTheLockWhileGitHubAnswers(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	slow := newSlowSet()
	h.svc.AddSet("octo-org/app", "ru-linux", slow)
	first, rest := f.Messages[0], f.Messages[1:]
	h.svc.Handle(first.Repo, first.Label, &first.Message)

	placed := make(chan struct{})
	go func() { h.svc.Place(context.Background()); close(placed) }()
	select {
	case <-slow.asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the placement pass never asked GitHub for the job")
	}

	// While GitHub has not answered, the setup reads and the next messages
	// are recorded.
	within(t, 5*time.Second, "reading the setup", func() { h.svc.Setup() })
	for _, m := range rest {
		within(t, 5*time.Second, "handling a message", func() { h.svc.Handle(m.Repo, m.Label, &m.Message) })
	}

	// The session closing waits for the call it made.
	removed := make(chan struct{})
	go func() { h.svc.RemoveSet("octo-org/app", "ru-linux", slow); close(removed) }()
	select {
	case <-removed:
		t.Fatal("the scale set's session closed while a placement pass acquired a job through it")
	case <-time.After(200 * time.Millisecond):
	}
	close(slow.release)
	<-placed
	<-removed
	if len(slow.acquired) == 0 {
		t.Fatal("the pass did not acquire the job GitHub answered for")
	}
	if len(h.placed()) == 0 {
		t.Fatal("no place took the acquired job")
	}
}

// Intent (the coordinator's design, place.go): place.json has two writers,
// the coordinator recording its place runner's process and the place runner
// recording its job starting; neither loses the other's change.
func TestPlaceRunKeepsBothWritersChanges(t *testing.T) {
	cfg := oneBox(t.TempDir())
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Places() {
		if err := os.MkdirAll(cfg.PlaceDir(p.ID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	launched := time.Date(2026, 10, 20, 17, 10, 14, 0, time.UTC)
	for i := range 100 {
		run := PlaceRun{RunnerName: "runner-up-box-1-1792602614", RunnerID: 41 + i, Target: "octo-org/app", Pool: "ru-linux",
			Repo: "octo-org/app", RequestID: 9101, JobName: "verify / linux", LaunchedAt: launched, Reservation: config.Share{Cores: 2, MemoryMB: 4096}}
		if err := writePlaceRun(cfg, "box-1", run); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- updatePlaceRun(cfg, "box-1", func(r *PlaceRun) { r.PID = 48213 })
		}()
		go func() {
			defer wg.Done()
			errs <- updatePlaceRun(cfg, "box-1", func(r *PlaceRun) { r.Started, r.RanJob = true, "verify / linux" })
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		got, ok := readPlaceRun(cfg, "box-1")
		if !ok || got.PID != 48213 || !got.Started || got.RanJob != "verify / linux" {
			t.Fatalf("write %d: place.json holds pid %d, started %v; want both writers' changes", i, got.PID, got.Started)
		}
	}
}

// Intent (the design, vmlimit.go): a placement pass reads the Macs' running
// macOS VMs at once, within one time budget, so a Mac slow to answer delays
// placement by at most that budget and no other Mac's reading waits on it.

// twoMacs is two Macs, each with one tart place for ru-mac jobs.
func twoMacs(dir string) *config.Config {
	mac := func(name string) config.Machine {
		return config.Machine{Name: name, SSH: []string{"you@" + name}, Places: []config.Place{{ID: name + "-1", Provider: config.ProviderTart,
			Pools: []string{"ru-mac"}, Cores: 4, MemoryMB: 8192, Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest"}}}
	}
	return &config.Config{StateDir: dir, GitHub: config.GitHub{Repos: []string{"octo-org/app"}}, Pools: []config.Pool{{Label: "ru-mac"}},
		Machines: []config.Machine{mac("mac-a"), mac("mac-b")}}
}

func shortVMReadBudget(t *testing.T, d time.Duration) {
	old := vmReadBudget
	vmReadBudget = d
	t.Cleanup(func() { vmReadBudget = old })
}

func TestMacsAreReadAtOnce(t *testing.T) {
	shortVMReadBudget(t, 5*time.Second)
	h := newHarness(t, twoMacs(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = time.Date(2026, 10, 20, 17, 10, 14, 0, time.UTC)
	// Each Mac answers only once both were asked: read one after the other,
	// the first would wait out the budget.
	var arrived sync.WaitGroup
	arrived.Add(2)
	h.svc.VmReader = func(ctx context.Context, m config.Machine) ([]string, error) {
		arrived.Done()
		both := make(chan struct{})
		go func() { arrived.Wait(); close(both) }()
		select {
		case <-both:
			return []string{"sequoia-dev"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h.svc.refreshVMs(context.Background())
	for _, name := range []string{"mac-a", "mac-b"} {
		if got := h.svc.vms[name]; !slices.Equal(got, []string{"sequoia-dev"}) {
			t.Fatalf("%s's VMs read as %v, want its reading", name, got)
		}
	}
}

func TestSlowMacDelaysPlacementByTheBudgetAtMost(t *testing.T) {
	shortVMReadBudget(t, 300*time.Millisecond)
	h := newHarness(t, twoMacs(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = time.Date(2026, 10, 20, 17, 10, 14, 0, time.UTC)
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	h.svc.VmReader = func(_ context.Context, m config.Machine) ([]string, error) {
		if m.Name == "mac-b" {
			// An agent that never answers, and ignores being given up on.
			<-hung
			return nil, nil
		}
		return []string{"sequoia-dev"}, nil
	}
	within(t, 5*time.Second, "reading the Macs", func() { h.svc.refreshVMs(context.Background()) })
	if got := h.svc.vms["mac-a"]; !slices.Equal(got, []string{"sequoia-dev"}) {
		t.Fatalf("mac-a's VMs read as %v, want its reading", got)
	}
	if got, ok := h.svc.vms["mac-b"]; ok {
		t.Fatalf("mac-b's VMs read as %v, want no reading from a Mac that did not answer", got)
	}
}
