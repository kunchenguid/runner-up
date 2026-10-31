//go:build !windows

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent (the design): the planner keeps
// pool compatibility, so a job runs only in a place serving its pool; it keeps
// each machine within its core and memory budget less the room kept for its
// owner; it gives a freed place to the job the order rules pick, across
// repositories; and a coordinator restart loses no waiting job and leaves
// running jobs in their places. Fixtures are scale set messages runner-up's
// predecessor recorded from GitHub, renamed (testdata/messages).

type fixture struct {
	Now      time.Time `json:"now"`
	Messages []struct {
		// At, when set, is when the message arrived.
		At      time.Time                      `json:"at"`
		Repo    string                         `json:"repo"`
		Label   string                         `json:"label"`
		Message scaleset.RunnerScaleSetMessage `json:"message"`
	} `json:"messages"`
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "messages", name))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// fakeSet stands in for one scale set at GitHub.
type fakeSet struct {
	mu       sync.Mutex
	gone     map[int64]bool // jobs GitHub no longer offers
	acquired []int64
	removed  []int64
	nextID   int
	// workFolders is the work folder each runner was registered with.
	workFolders map[string]string
	// unreachable is how many removals fail before GitHub answers again.
	unreachable int
	// running are the runners GitHub gave a job, which it refuses to remove.
	running map[int64]bool
}

func (f *fakeSet) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for _, id := range ids {
		if !f.gone[id] {
			out = append(out, id)
			f.acquired = append(f.acquired, id)
		}
	}
	return out, nil
}

func (f *fakeSet) GenerateJIT(_ context.Context, name, workFolder string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workFolders == nil {
		f.workFolders = map[string]string{}
	}
	f.workFolders[name] = workFolder
	f.nextID++
	return "jit-for-" + name, f.nextID, nil
}

func (f *fakeSet) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable > 0 {
		f.unreachable--
		return errors.New("failed to issue the request: dial tcp: lookup api.github.com: no such host")
	}
	if f.running[id] {
		// As GitHub refused a runner it had given a job (RUU-18).
		return fmt.Errorf("bad request: request DELETE https://broker.actions.githubusercontent.com/rest/_apis/distributedtask/pools/0/agents/%d"+
			"?api-version=6.0-preview failed(status=\"400 Bad Request\"): %w: Runner is currently running a job and cannot be deleted.",
			id, scaleset.JobStillRunningError)
	}
	f.removed = append(f.removed, id)
	return nil
}

// fakeLauncher keeps each place's lock held, as the place runner process
// would, until the test finishes that place's job.
type fakeLauncher struct {
	mu      sync.Mutex
	held    map[string]int
	dirs    map[string]string
	jits    map[string]string
	stopped []string
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{held: map[string]int{}, dirs: map[string]string{}, jits: map[string]string{}}
}

func (l *fakeLauncher) Launch(p config.Place, pool string, lock *os.File, jit string) error {
	fd, err := syscall.Dup(int(lock.Fd()))
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[p.ID] = fd
	l.dirs[p.ID] = filepath.Dir(lock.Name())
	l.jits[p.ID] = jit
	return nil
}

func (l *fakeLauncher) Stop(id string) error {
	l.mu.Lock()
	l.stopped = append(l.stopped, id)
	l.mu.Unlock()
	return nil
}

// finish ends the job in place id: its runner took a job, as the place
// runner records in place.json, then the runner process exits and the kernel
// drops the place's lock.
func (l *fakeLauncher) finish(t *testing.T, id string) {
	t.Helper()
	l.end(t, id, true)
}

// giveUp ends place id's runner before any job reached it, as a virtual
// machine that never boots or a stopped spare runner does.
func (l *fakeLauncher) giveUp(t *testing.T, id string) {
	t.Helper()
	l.end(t, id, false)
}

func (l *fakeLauncher) end(t *testing.T, id string, started bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	fd, ok := l.held[id]
	if !ok {
		t.Fatalf("place %s runs nothing", id)
	}
	if path := filepath.Join(l.dirs[id], "place.json"); started {
		var run PlaceRun
		if err := config.ReadJSON(path, &run); err == nil {
			run.Started = true
			config.WriteJSON(path, run)
		}
	}
	syscall.Close(fd)
	delete(l.held, id)
}

type harness struct {
	t        *testing.T
	cfg      *config.Config
	svc      *Service
	launcher *fakeLauncher
	sets     map[string]*fakeSet
	now      time.Time
}

// oneBox is one machine with two small Docker places for ru-linux jobs and a
// large KVM place, listed last, for ru-large jobs and, when the small ones
// are busy, ru-linux jobs.
func oneBox(dir string) *config.Config {
	return &config.Config{
		StateDir: dir,
		GitHub:   config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
		Pools:    []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}},
		Machines: []config.Machine{{Name: "box", SSH: []string{"ci@box"}, Places: []config.Place{
			{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
			{ID: "box-2", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
			{ID: "box-large", Provider: config.ProviderKVM, Pools: []string{"ru-large", "ru-linux"}, Cores: 4, MemoryMB: 8192,
				Image: "/var/lib/runner-up/noble-server-cloudimg-amd64.img"},
		}}},
	}
}

func newHarness(t *testing.T, cfg *config.Config, launcher *fakeLauncher, sets map[string]*fakeSet) *harness {
	t.Helper()
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewService(cfg, log, launcher)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, svc: svc, launcher: launcher, sets: sets}
	svc.now = func() time.Time { return h.now }
	// The scale sets the coordinator opens (cmd/main.go).
	for _, target := range cfg.Targets() {
		for _, label := range cfg.Labels() {
			k := setKey(target, label)
			if sets[k] == nil {
				sets[k] = &fakeSet{gone: map[int64]bool{}}
			}
			svc.AddSet(target, label, sets[k])
		}
	}
	return h
}

func (h *harness) feed(f fixture) {
	h.t.Helper()
	h.feedRange(f, 0, len(f.Messages))
}

// feedRange hands the service messages [from, to) of f, each at its own
// arrival time where the fixture has one.
func (h *harness) feedRange(f fixture, from, to int) {
	h.t.Helper()
	for _, m := range f.Messages[from:to] {
		if !m.At.IsZero() {
			h.now = m.At
		}
		msg := m.Message
		if err := h.svc.Handle(context.Background(), m.Repo, m.Label, &msg); err != nil {
			h.t.Fatal(err)
		}
	}
}

// placed returns which job each busy place runs, by place.json.
func (h *harness) placed() map[string]string {
	out := map[string]string{}
	for _, p := range h.cfg.Places() {
		held, _ := lock.HeldBy(filepath.Join(h.cfg.PlaceDir(p.ID), "busy.lock"))
		if !held {
			continue
		}
		var run PlaceRun
		if err := config.ReadJSON(filepath.Join(h.cfg.PlaceDir(p.ID), "place.json"), &run); err != nil {
			h.t.Fatal(err)
		}
		out[p.ID] = run.Repo + " " + run.JobName
	}
	return out
}

func (h *harness) want(want map[string]string) {
	h.t.Helper()
	got := h.placed()
	for id, job := range want {
		if got[id] != job {
			h.t.Errorf("place %s runs %q, want %q", id, got[id], job)
		}
	}
	for id, job := range got {
		if _, ok := want[id]; !ok {
			h.t.Errorf("place %s runs %q, want it free", id, job)
		}
	}
}

// finishAndPlace ends a place's job the way GitHub reports it and the
// service then places what fits.
func (h *harness) finishAndPlace(id string) {
	h.launcher.finish(h.t, id)
	h.now = h.now.Add(time.Minute)
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
}

// The jobs of waiting-jobs.json, as places run them.
const (
	verify  = "octo-org/app verify / linux"
	pr184   = "octo-org/app affected-journeys"
	publish = "octo-org/app Build and publish (a dry run publishes nothing)"
	suite   = "octo-user/tool suite"
	release = "octo-user/wallet Release"
)

func TestWaitingJobsFillEveryPlaceInOrder(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feed(f)
	// Every place runs a job: the large place the large job, the small
	// places the two oldest others.
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	if n := len(h.svc.waiting); n != 3 {
		t.Fatalf("%d jobs waiting, want 3 (suite, Release, PR #186)", n)
	}

	// The large place frees. Under the default order the hour-old suite,
	// which waited longer than the wallet's release, takes it.
	h.finishAndPlace("box-large")
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": suite})

	// A small place takes the remaining ru-linux job; the release waits for
	// the large place.
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-1": pr184, "box-2": pr184, "box-large": suite})
	h.finishAndPlace("box-2")
	h.want(map[string]string{"box-1": pr184, "box-large": suite})
	var run PlaceRun
	config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &run)
	if run.RequestID != 9104 {
		t.Fatalf("box-1 runs request %d, want PR #186's 9104", run.RequestID)
	}
	h.finishAndPlace("box-large")
	h.want(map[string]string{"box-1": pr184, "box-large": release})
	if len(h.svc.waiting) != 0 {
		t.Fatalf("jobs still waiting: %v", h.svc.waiting)
	}
	// Each job was acquired only once a place was chosen for it.
	if got := h.sets[setKey("octo-org/app", "ru-linux")].acquired; !slices.Equal(got, []int64{9101, 9102, 9104}) {
		t.Fatalf("app acquired %v in that order, want 9101, 9102, 9104", got)
	}
	// The registration went to the place runner, for a runner named after
	// its place.
	if jit := h.launcher.jits["box-1"]; !strings.HasPrefix(jit, "jit-for-runner-up-box-1-") {
		t.Fatalf("box-1 got registration %q", jit)
	}
}

// Intent (a bug moving a fleet to runner-up found: Linux jobs must keep their
// work in /home/runner/work, where GitHub's Ubuntu runners do, so a cache
// saved there restores where the tools look). A runner is registered with
// its place's work folder, or the runner's own _work by default.
func TestRunnerIsRegisteredWithItsPlacesWorkFolder(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	cfg.Machines[0].Places[0].WorkFolder = "/home/runner/work"
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feed(f)
	h.want(map[string]string{"box-1": verify, "box-2": pr184, "box-large": publish})
	got := map[string]string{}
	for _, set := range h.sets {
		for name, wf := range set.workFolders {
			got[strings.Join(strings.Split(name, "-")[2:4], "-")] = wf
		}
	}
	want := map[string]string{"box-1": "/home/runner/work", "box-2": "_work", "box-large": "_work"}
	if len(got) != len(want) {
		t.Fatalf("runners registered with work folders %v, want %v", got, want)
	}
	for place, wf := range want {
		if got[place] != wf {
			t.Errorf("%s's runner was registered with work folder %q, want %q", place, got[place], wf)
		}
	}
}

func TestLargeJobNeverStartsInASmallPlace(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	if err := os.MkdirAll(cfg.PlaceDir("box-large"), 0o700); err != nil {
		t.Fatal(err)
	}
	hold, err := lock.Busy(filepath.Join(cfg.PlaceDir("box-large"), "busy.lock"))
	if err != nil || hold == nil {
		t.Fatal("could not hold the large place", err)
	}
	defer hold.Close()
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	// Only the two jobs asking for ru-large, with both small places free.
	for _, m := range f.Messages {
		if m.Label == "ru-large" {
			msg := m.Message
			h.svc.Handle(context.Background(), m.Repo, m.Label, &msg)
		}
	}
	if len(h.launcher.held) != 0 {
		t.Fatal("a ru-large job started in a place that does not serve ru-large")
	}
	if len(h.svc.waiting) != 2 {
		t.Fatalf("%d waiting, want both ru-large jobs still at GitHub", len(h.svc.waiting))
	}
	if n := len(h.sets[setKey("octo-org/app", "ru-large")].acquired); n != 0 {
		t.Fatalf("acquired %d jobs no place could take", n)
	}

	// The large place frees and takes the older one.
	hold.Close()
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-large": publish})
	h.finishAndPlace("box-large")
	h.want(map[string]string{"box-large": release})
}

func TestJobGoneAtGitHubGivesThePlaceToTheNext(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	sets := map[string]*fakeSet{setKey("octo-org/app", "ru-linux"): {gone: map[int64]bool{9101: true}}}
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), sets)
	h.now = f.Now
	h.feed(f)
	// verify was cancelled while it waited: nothing runs it, and its place
	// goes to the next job that fits.
	h.want(map[string]string{"box-1": suite, "box-2": pr184, "box-large": publish})
	if got := h.sets[setKey("octo-org/app", "ru-linux")].acquired; !slices.Equal(got, []int64{9102}) {
		t.Fatalf("app acquired %v, want only PR #184's 9102", got)
	}
}

// Intent (plan: "per-machine core/memory budgets with owner room"): a place
// starts a job only while its share fits beside the shares of its machine's
// running places, within the machine's cores and memory less the room kept
// for its owner.
func TestMachineBudgetKeepsTheOwnersRoom(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneBox(t.TempDir())
	cfg.Machines[0].Cores, cfg.Machines[0].RoomCores = 8, 2
	cfg.Machines[0].MemoryMB, cfg.Machines[0].RoomMemoryMB = 32768, 4096
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feed(f)
	// 6 of 8 cores are the places'. The two small places reserve 4; the
	// large one's 4 more would eat into the owner's 2.
	h.want(map[string]string{"box-1": verify, "box-2": pr184})
	if busy, why := h.svc.PlaceBusy(cfg.Machines[0].Places[2]); !busy || why != "cores 4 of 6 in use, 2 kept for the owner" {
		t.Fatalf("box-large reads busy=%v %q, want it held for the owner's room", busy, why)
	}

	// A small place frees: the large job fits now, and takes the large
	// place; the freed small place stays empty, though ru-linux jobs wait,
	// because the machine has no cores left for it.
	h.finishAndPlace("box-1")
	h.want(map[string]string{"box-2": pr184, "box-large": publish})
	if busy, why := h.svc.PlaceBusy(cfg.Machines[0].Places[0]); !busy || why != "cores 6 of 6 in use, 2 kept for the owner" {
		t.Fatalf("box-1 reads busy=%v %q, want it held for the owner's room", busy, why)
	}

	// Memory counts the same way: with room for only 12 GB of places, the
	// large place's 8 GB never fits beside a small place's 4 GB and another.
	cfg2 := oneBox(t.TempDir())
	cfg2.Machines[0].Cores, cfg2.Machines[0].MemoryMB, cfg2.Machines[0].RoomMemoryMB = 16, 16384, 4096
	h2 := newHarness(t, cfg2, newFakeLauncher(), map[string]*fakeSet{})
	h2.now = f.Now
	h2.feed(f)
	h2.want(map[string]string{"box-1": verify, "box-2": pr184})
	if _, why := h2.svc.PlaceBusy(cfg2.Machines[0].Places[2]); why != "memory 8192 of 12288 MB in use, 4096 MB kept for the owner" {
		t.Fatalf("box-large reads %q, want it held for the owner's memory", why)
	}
}

func TestRestartKeepsWaitingJobsAndRunningPlaces(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	dir := t.TempDir()
	launcher := newFakeLauncher()
	sets := map[string]*fakeSet{}
	h := newHarness(t, oneBox(dir), launcher, sets)
	h.now = f.Now
	h.feed(f)
	before := h.placed()

	// The coordinator restarts. The place runners carry on and keep their
	// locks; GitHub does not announce the waiting jobs again.
	h2 := newHarness(t, oneBox(dir), launcher, sets)
	h2.now = f.Now.Add(time.Minute)
	if len(h2.svc.waiting) != 3 {
		t.Fatalf("restarted coordinator knows %d waiting jobs, want 3", len(h2.svc.waiting))
	}
	h2.svc.Place(context.Background())
	h2.want(before)

	h2.finishAndPlace("box-1")
	h2.want(map[string]string{"box-1": suite, "box-2": pr184, "box-large": publish})
}

func TestCancelledBeforeRunnerStopsTheSpareRunner(t *testing.T) {
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	stopSpareRunner(h)
	// Its runner ends; the place is free and the unused runner is removed.
	h.launcher.giveUp(t, "box-2")
	h.now = h.now.Add(time.Minute)
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
	if got := h.sets[setKey("octo-org/app", "ru-linux")].removed; len(got) != 1 {
		t.Fatalf("removed runners %v, want the one unused runner", got)
	}
	h.want(map[string]string{"box-1": verify, "box-2": suite, "box-large": publish})
}

// A runner stopped before its job started stays in the repository's runner
// list, offline, unless the coordinator removes it, even when its place takes
// a new job before Tidy reads the stopped runner.
func TestUnusedRunnerIsRemovedWhenItsPlaceIsTakenBeforeTidy(t *testing.T) {
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	spare := stopSpareRunner(h)
	takeFreedPlaceBeforeTidy(h)

	// The next Tidy still removes the stopped runner, and only it.
	h.svc.Tidy(context.Background())
	set := h.sets[setKey("octo-org/app", "ru-linux")]
	if !slices.Equal(set.removed, []int64{int64(spare.RunnerID)}) {
		t.Fatalf("removed runners %v, want box-2's stopped runner %d", set.removed, spare.RunnerID)
	}
	h.svc.Tidy(context.Background())
	if len(set.removed) != 1 {
		t.Fatalf("removed runners %v, want the stopped runner removed once", set.removed)
	}
}

// A removal GitHub does not answer is tried again, after the place has taken
// its next job and after a coordinator restart.
func TestUnusedRunnerOutlivesAFailedRemovalAndARestart(t *testing.T) {
	dir := t.TempDir()
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	h := newHarness(t, oneBox(dir), launcher, sets)
	spare := stopSpareRunner(h)
	set := h.sets[setKey("octo-org/app", "ru-linux")]
	set.unreachable = 1
	takeFreedPlaceBeforeTidy(h)
	h.svc.Tidy(context.Background())
	if len(set.removed) != 0 {
		t.Fatalf("removed runners %v while GitHub could not be reached", set.removed)
	}

	h2 := newHarness(t, oneBox(dir), launcher, sets)
	h2.now = h.now.Add(time.Minute)
	h2.svc.Tidy(context.Background())
	if !slices.Equal(set.removed, []int64{int64(spare.RunnerID)}) {
		t.Fatalf("removed runners %v after the restart, want box-2's stopped runner %d", set.removed, spare.RunnerID)
	}
}

// Intent (RUU-18: at the start timeout, "runner-up removes the runner's
// GitHub registration first and powers off only after GitHub accepts the
// removal"): a runner no job reached in time keeps its place while GitHub
// cannot be reached, and is asked about again at the next tick. Once GitHub
// removes it, its place is stopped; when the place ends the runner is not
// removed a second time, and its job gets a new runner.
func TestIdleRunnerKeepsItsPlaceUntilGitHubRemovesIt(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	h := newHarness(t, proofBox(t.TempDir(), "box-1"), newFakeLauncher(), map[string]*fakeSet{})
	set := h.sets[setKey("octo-org/app", "ru-proof")]
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": proofJob})
	var first PlaceRun
	if err := config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &first); err != nil {
		t.Fatal(err)
	}
	tick := func(d time.Duration) {
		h.now = h.now.Add(d)
		h.svc.Tidy(context.Background())
	}

	// A second before the start timeout the runner is left alone.
	tick(h.cfg.Seconds(h.cfg.StartTimeoutSeconds) - time.Second)
	if len(set.removed) != 0 || len(h.launcher.stopped) != 0 {
		t.Fatalf("before the start timeout removed %v and stopped %v", set.removed, h.launcher.stopped)
	}
	// At the start timeout GitHub cannot be reached: the runner keeps its
	// place.
	set.unreachable = 1
	tick(time.Second)
	if len(set.removed) != 0 || len(h.launcher.stopped) != 0 {
		t.Fatalf("while GitHub could not be reached removed %v and stopped %v", set.removed, h.launcher.stopped)
	}
	// At the next tick GitHub removes it, and its place is stopped once.
	tick(10 * time.Second)
	tick(10 * time.Second)
	if !slices.Equal(set.removed, []int64{int64(first.RunnerID)}) || !slices.Equal(h.launcher.stopped, []string{"box-1"}) {
		t.Fatalf("removed %v and stopped %v, want runner %d removed and box-1 stopped, once each", set.removed,
			h.launcher.stopped, first.RunnerID)
	}

	// Its place ends. The runner is gone at GitHub already, and its job
	// gets a new runner.
	h.launcher.giveUp(t, "box-1")
	tick(time.Second)
	h.svc.Place(context.Background())
	tick(10 * time.Second)
	if len(set.removed) != 1 {
		t.Fatalf("removed %v, want the runner removed once", set.removed)
	}
	h.want(map[string]string{"box-1": proofJob})
	var again PlaceRun
	if err := config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &again); err != nil {
		t.Fatal(err)
	}
	if again.RunnerName == first.RunnerName {
		t.Fatalf("box-1 still names the first runner %s", first.RunnerName)
	}
}

// Intent (RUU-18: "optionally log a refused removal for an ended place as
// a stranded job, so the next one is diagnosed at once"): when a runner's
// place has ended but GitHub will not remove the runner because it gave it a
// job, the coordinator says that job is stranded, naming the runner, and
// keeps trying to remove the runner.
func TestRefusedRemovalOfAnEndedRunnerNamesTheStrandedJob(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	h := newHarness(t, proofBox(t.TempDir(), "box-1"), newFakeLauncher(), map[string]*fakeSet{})
	var out bytes.Buffer
	h.svc.Log = slog.New(slog.NewTextHandler(&out, nil))
	set := h.sets[setKey("octo-org/app", "ru-proof")]
	h.feedRange(f, 0, 1)
	var run PlaceRun
	if err := config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &run); err != nil {
		t.Fatal(err)
	}

	// GitHub gives the runner a job just as its place ends, as when SSH to
	// the machine drops.
	set.running = map[int64]bool{int64(run.RunnerID): true}
	h.launcher.giveUp(t, "box-1")
	h.now = h.now.Add(time.Minute)
	h.svc.Tidy(context.Background())
	if !strings.Contains(out.String(), `msg="a runner whose place has ended is running a job at GitHub; the job is stranded" runner=`+run.RunnerName) {
		t.Fatalf("the coordinator logged:\n%s", out.String())
	}
	if _, ok := h.svc.unused[unusedKey(run)]; !ok || len(set.removed) != 0 {
		t.Fatalf("unused %v, removed %v; want the runner still to remove", h.svc.unused, set.removed)
	}
}

// stopSpareRunner places the waiting jobs, starts verify in box-1, and
// cancels PR #184 before its runner in box-2 took it, so the service stops
// that runner. It returns the stopped runner's run.
func stopSpareRunner(h *harness) PlaceRun {
	h.t.Helper()
	f := loadFixture(h.t, "waiting-jobs.json")
	h.now = f.Now
	h.feed(f)
	h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", &scaleset.RunnerScaleSetMessage{MessageID: 2,
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "x", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9101}}}})
	markStarted(h.t, h.cfg, "box-1")
	var spare PlaceRun
	if err := config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-2"), "place.json"), &spare); err != nil {
		h.t.Fatal(err)
	}
	h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", &scaleset.RunnerScaleSetMessage{MessageID: 3,
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "canceled", JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 9102, JobDisplayName: "affected-journeys",
			JobMessageType: scaleset.JobMessageType{MessageType: scaleset.MessageTypeJobCompleted}}}}})
	if !slices.Equal(h.launcher.stopped, []string{"box-2"}) {
		h.t.Fatalf("stopped %v, want the spare runner in box-2", h.launcher.stopped)
	}
	return spare
}

// takeFreedPlaceBeforeTidy ends box-2's stopped runner, and a scale set's
// next poll places the suite there before Tidy runs.
func takeFreedPlaceBeforeTidy(h *harness) {
	h.t.Helper()
	h.launcher.giveUp(h.t, "box-2")
	h.now = h.now.Add(5 * time.Second)
	if err := h.svc.Handle(context.Background(), "octo-org/app", "ru-linux", nil); err != nil {
		h.t.Fatal(err)
	}
	h.want(map[string]string{"box-1": verify, "box-2": suite, "box-large": publish})
}

func markStarted(t *testing.T, cfg *config.Config, id string) {
	path := filepath.Join(cfg.PlaceDir(id), "place.json")
	var run PlaceRun
	if err := config.ReadJSON(path, &run); err != nil {
		t.Fatal(err)
	}
	run.Started = true
	config.WriteJSON(path, run)
}

// twoBoxes adds a second machine with the same places.
func twoBoxes(dir string) *config.Config {
	cfg := oneBox(dir)
	second := config.Machine{Name: "spare", SSH: []string{"ci@spare"}}
	for _, p := range cfg.Machines[0].Places {
		p.ID = strings.Replace(p.ID, "box", "spare", 1)
		second.Places = append(second.Places, p)
	}
	cfg.Machines = append(cfg.Machines, second)
	return cfg
}

func TestSecondMachineSharesOneLine(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, twoBoxes(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feed(f)
	// Six places on two machines, six jobs: every one runs. The two large
	// jobs take the two large places; the four others fill the small places,
	// spread so each machine carries three.
	got := h.placed()
	if len(got) != 6 {
		t.Fatalf("%d places busy, want all 6: %v", len(got), got)
	}
	if !slices.Contains([]string{publish, release}, got["box-large"]) || !slices.Contains([]string{publish, release}, got["spare-large"]) {
		t.Fatalf("large places run %q and %q, want the two large jobs", got["box-large"], got["spare-large"])
	}
	perMachine := map[string]int{}
	for id := range got {
		perMachine[strings.SplitN(id, "-", 2)[0]]++
	}
	if perMachine["box"] != 3 || perMachine["spare"] != 3 {
		t.Fatalf("machines carry %v, want three each", perMachine)
	}
}
