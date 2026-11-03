//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (DOP-235's approved plan, piece 1): "serve watches its setup file
// and waits about a second after a change, because a pull rewrites the file.
// It runs check-config and applies pools, places, sizes and order in place,
// without dropping running jobs or open sessions. It keeps the running setup
// when the new one fails." The end-to-end journey is in cmd/reload_e2e_test.go.

// A pull rewrites the setup file in steps; the watcher sends nothing until
// the file has held one content for its quiet time, then sends that once.
// A file that cannot be read is sent the same way, for the coordinator to
// keep its running setup.
func TestSetupWatchSendsTheFileOnceItHasSettled(t *testing.T) {
	old, err := json.MarshalIndent(oneBox("/var/lib/runner-up"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	cfg := oneBox("/var/lib/runner-up")
	cfg.Pools = append(cfg.Pools, config.Pool{Label: "ru-mac", Cores: 4, MemoryMB: 8192})
	pulled, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	missing := &fs.PathError{Op: "open", Path: "config.json", Err: fs.ErrNotExist}
	start := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	w := &setupWatch{quiet: time.Second}
	type step struct {
		at   time.Duration
		read SetupRead
		want *SetupRead // what is sent, if anything
	}
	steps := []step{
		{0, SetupRead{Data: old}, nil},
		// The pull truncates the file and writes it in two parts.
		{250 * time.Millisecond, SetupRead{Data: pulled[:len(pulled)/2]}, nil},
		{500 * time.Millisecond, SetupRead{Data: pulled}, nil},
		{1250 * time.Millisecond, SetupRead{Data: pulled}, nil},
		{1500 * time.Millisecond, SetupRead{Data: pulled}, &SetupRead{Data: pulled}},
		{1750 * time.Millisecond, SetupRead{Data: pulled}, nil},
		// The file goes missing for a while, then comes back.
		{2 * time.Second, SetupRead{Err: missing}, nil},
		{3 * time.Second, SetupRead{Err: missing}, &SetupRead{Err: missing}},
		{3250 * time.Millisecond, SetupRead{Data: pulled}, nil},
		{4250 * time.Millisecond, SetupRead{Data: pulled}, &SetupRead{Data: pulled}},
	}
	for _, s := range steps {
		got, sent := w.next(s.read, start.Add(s.at))
		switch {
		case s.want == nil && sent:
			t.Fatalf("at %v the watcher sent %.40q, want nothing yet", s.at, got.Data)
		case s.want != nil && !sent:
			t.Fatalf("at %v the watcher sent nothing, want the file's settled content", s.at)
		case s.want != nil && !got.same(*s.want):
			t.Fatalf("at %v the watcher sent %.40q (%v), want %.40q (%v)", s.at, got.Data, got.Err, s.want.Data, s.want.Err)
		}
	}
}

// startedJob marks place id's runner as running its job, as the place runner
// records once the job reaches it.
func startedJob(t *testing.T, cfg *config.Config, id string) {
	t.Helper()
	path := filepath.Join(cfg.PlaceDir(id), "place.json")
	var run PlaceRun
	if err := config.ReadJSON(path, &run); err != nil {
		t.Fatal(err)
	}
	run.Started = true
	if err := config.WriteJSON(path, run); err != nil {
		t.Fatal(err)
	}
}

func held(t *testing.T, cfg *config.Config, id string) bool {
	t.Helper()
	h, err := lock.HeldBy(filepath.Join(cfg.PlaceDir(id), "busy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func reload(t *testing.T, svc *Service, next *config.Config) {
	t.Helper()
	next.Defaults()
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	rules, err := plan.ParseOrder(next.Order)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := svc.Reload(next, rules); err != nil || !changed {
		t.Fatalf("reload: changed %v, %v", changed, err)
	}
}

// A setup change that removes a place while its job runs leaves the job
// running, and the job's share keeps counting against its machine until it
// ends, through a coordinator restart too: a place the change adds there does
// not start a job that would not fit beside it.
func TestRemovedPlaceKeepsItsShareUntilItsJobEnds(t *testing.T) {
	dir := t.TempDir()
	// One machine whose places may reserve 3 cores, with one 2-core place.
	box := func(places ...config.Place) *config.Config {
		return &config.Config{StateDir: dir, GitHub: config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
			Pools: []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}},
			Machines: []config.Machine{{Name: "box", SSH: []string{"ci@box"}, Cores: 4, MemoryMB: 16384, RoomCores: 1, RoomMemoryMB: 1024,
				Places: places}}}
	}
	docker := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderDocker, Pools: []string{"ru-linux", "ru-large"}, Cores: 2, MemoryMB: 4096}
	}
	launcher, sets := newFakeLauncher(), map[string]*fakeSet{}
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, box(docker("box-1")), launcher, sets)
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": verify})
	startedJob(t, h.cfg, "box-1")

	// The setup now has box-2 on the machine in box-1's stead.
	next := box(docker("box-2"))
	reload(t, h.svc, next)
	h.cfg = next
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
	if !held(t, next, "box-1") || len(launcher.stopped) != 0 {
		t.Fatalf("box-1's job stopped at the setup change: held %v, stopped %v", held(t, next, "box-1"), launcher.stopped)
	}
	h.want(map[string]string{})
	if got := h.svc.inUse("box", ""); got.Cores != 2 {
		t.Fatalf("box's places reserve %d cores, want box-1's running job's 2", got.Cores)
	}

	// The coordinator restarts with the new setup while the job runs.
	h = newHarness(t, next, launcher, sets)
	h.now = f.Now.Add(time.Minute)
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
	h.want(map[string]string{})

	// The job ends. Its place is retired, and box-2 takes the next job.
	launcher.finish(t, "box-1")
	h.now = h.now.Add(time.Minute)
	h.svc.Tidy(context.Background())
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-2": pr184})
	if _, err := os.Stat(filepath.Join(next.PlaceDir("box-1"), "last-place.json")); err != nil {
		t.Errorf("box-1's finished run was not retired: %v", err)
	}
	if len(h.svc.departed) != 0 {
		t.Errorf("the coordinator still looks after %v", h.svc.departed)
	}
}

// A setup change that moves a place to another machine while its job runs
// leaves the job's share on the machine the job runs on, until the job ends.
func TestMovedPlaceCountsWhereItsJobRuns(t *testing.T) {
	dir := t.TempDir()
	machine := func(name string, places ...config.Place) config.Machine {
		return config.Machine{Name: name, SSH: []string{"ci@" + name}, Cores: 4, MemoryMB: 16384, RoomCores: 1, RoomMemoryMB: 1024,
			Places: places}
	}
	setup := func(a, b config.Machine) *config.Config {
		return &config.Config{StateDir: dir, GitHub: config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
			Pools: []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}}, Machines: []config.Machine{a, b}}
	}
	box1 := config.Place{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux", "ru-large"}, Cores: 2, MemoryMB: 4096}
	launcher := newFakeLauncher()
	f := loadFixture(t, "waiting-jobs.json")
	h := newHarness(t, setup(machine("box", box1), machine("big")), launcher, map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": verify})
	startedJob(t, h.cfg, "box-1")

	// The setup now has box-1 on big.
	reload(t, h.svc, setup(machine("box"), machine("big", box1)))
	if box, big := h.svc.inUse("box", ""), h.svc.inUse("big", ""); box.Cores != 2 || big.Cores != 0 {
		t.Fatalf("box's places reserve %d cores and big's %d, want box-1's running job's 2 on box and none on big", box.Cores, big.Cores)
	}

	// The job ends; box-1 counts nowhere until it takes a job on big.
	launcher.finish(t, "box-1")
	h.svc.Tidy(context.Background())
	if box, big := h.svc.inUse("box", ""), h.svc.inUse("big", ""); box.Cores != 0 || big.Cores != 0 {
		t.Fatalf("after box-1's job box's places reserve %d cores and big's %d, want none", box.Cores, big.Cores)
	}
}

// A changed order picks the job a freed place takes from the jobs already
// waiting, with no restart.
func TestChangedOrderPicksTheNextJob(t *testing.T) {
	dir := t.TempDir()
	f := loadFixture(t, "waiting-jobs.json")
	busy := occupy(t, onePlace(dir), "box-1")
	h := newHarness(t, onePlace(dir), newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feed(f)
	// The default order would give the freed place verify, which waited
	// longest; the new order puts ru-large's jobs first.
	reload(t, h.svc, onePlace(dir, "pool:ru-large", "waited:60m", "shortest"))
	busy.Close()
	h.svc.Place(context.Background())
	var got []string
	for range len(f.Messages) + 1 {
		job, ok := h.placed()["box-1"]
		if !ok {
			break
		}
		var run PlaceRun
		config.ReadJSON(filepath.Join(h.cfg.PlaceDir("box-1"), "place.json"), &run)
		got = append(got, job+" "+requestName(run.RequestID))
		h.finishAndPlace("box-1")
	}
	wantOrder(t, got, []string{oPublish, oRelease, oVerify, oPR184, oSuite, oPR186})
}

// A setup that moves the state folder, where running places hold their
// locks, is refused, and the coordinator places jobs as before.
func TestSetupMovingTheStateFolderIsRefused(t *testing.T) {
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	moved := oneBox(t.TempDir())
	moved.Defaults()
	changed, err := h.svc.Reload(moved, nil)
	if err == nil || changed || !strings.Contains(err.Error(), "stateDir changes only when runner-up serve restarts") {
		t.Fatalf("a setup moving the state folder: changed %v, %v", changed, err)
	}
	if h.svc.Setup() != h.cfg {
		t.Fatal("the refused setup replaced the running one")
	}
	f := loadFixture(t, "waiting-jobs.json")
	h.now = f.Now
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": verify, "box-2": pr184})
	if _, err := os.Stat(filepath.Join(moved.StateDir, "places")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused setup's state folder was made: %v", err)
	}
}
