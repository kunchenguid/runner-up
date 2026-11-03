//go:build !windows

package coordinator

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent: a pool has one size whichever
// place takes it; a waiting large job keeps its room instead of being starved
// by later small jobs. The 14-core budget and 12+2 illustration are from the
// accepted card, not proposed production measurements. Job messages are the
// project's renamed GitHub recording, not fabricated protocol payloads.
func TestPoolSizesKeepRoomForTheFirstJob(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := onePlace(t.TempDir(), "pool:ru-large")
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 2, 4096
	cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 12, 32768
	m := &cfg.Machines[0]
	m.Cores, m.RoomCores = 22, 8
	m.MemoryMB, m.RoomMemoryMB = 65536, 16384
	m.Places = []config.Place{
		{ID: "host-1", Provider: config.ProviderHost, Pools: []string{"ru-large", "ru-linux"}, Cores: 6, MemoryMB: 16384},
		{ID: "host-2", Provider: config.ProviderHost, Pools: []string{"ru-large", "ru-linux"}, Cores: 6, MemoryMB: 16384},
		{ID: "phone", Provider: config.ProviderTart, Pools: []string{"ru-linux"}, Cores: 4, MemoryMB: 16384, Image: "ci-base"},
	}
	phone := occupy(t, cfg, "phone")
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir("phone"), "place.json"), PlaceRun{Started: true, Pool: "ru-linux"}); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	// An already-running legacy phone job reserves four cores from its place.
	h.feedRange(f, 1, 2)
	h.feedRange(f, 0, 1)
	if len(h.launcher.held) != 0 {
		t.Fatalf("later work took the large job's room: %v", h.placed())
	}
	t.Log("phone holds 4 of 14 cores; first-ranked 12-core job and later 2-core jobs wait")
	phone.Close()
	h.svc.Place(context.Background())
	h.want(map[string]string{"host-1": publish, "host-2": verify})
	for id, want := range map[string]config.Share{"host-1": cfg.Pools[1].Size(), "host-2": cfg.Pools[0].Size()} {
		var run PlaceRun
		if err := config.ReadJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), &run); err != nil {
			t.Fatal(err)
		}
		if run.Reservation != want {
			t.Fatalf("%s holds %+v, want %+v", id, run.Reservation, want)
		}
	}
	t.Log("phone ended; 12-core job starts beside one 2-core job, with identical pool sizes on either host place")
	// A coordinator restart and a configuration edit cannot shrink live use.
	cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 4, 8192
	restarted := newHarness(t, cfg, h.launcher, h.sets)
	restarted.now = f.Now.Add(time.Minute)
	if got := restarted.svc.inUse("box", ""); got.Cores != m.Budget().Cores || got.MemoryMB != 32768+cfg.Pools[0].MemoryMB {
		t.Fatalf("restart shrank live reservations: %+v", got)
	}
	restarted.svc.Place(context.Background())
	restarted.want(map[string]string{"host-1": publish, "host-2": verify})
	t.Log("restart with a smaller configured size retains the running job's original 12-core reservation")
	h.launcher.finish(t, "host-1")
	h.launcher.finish(t, "host-2")
}

// Intent: a job waits only when its cores OR memory do
// not fit. Pool sizes reach VM launch arguments, not just planner accounting.
func TestPoolSizeFitsMemoryAndSizesTheAgent(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := onePlace(t.TempDir(), "pool:ru-large")
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 2, 4096
	cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 4, 16384
	m := &cfg.Machines[0]
	m.Cores = 14
	m.MemoryMB = 24576
	m.RoomMemoryMB = 8192
	first := m.Places[0]
	first.Provider = config.ProviderTart
	first.Image = "ci-base"
	first.Cores = 2
	first.MemoryMB = 8192
	second := first
	second.ID = "box-2"
	m.Places = []config.Place{first, second}
	busy := occupy(t, cfg, "box-2")
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir("box-2"), "place.json"), PlaceRun{Started: true, Pool: "ru-linux"}); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 1, 2)
	h.feedRange(f, 0, 1)
	if len(h.launcher.held) != 0 {
		t.Fatalf("later job used reserved memory: %v", h.placed())
	}
	busy.Close()
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": publish})
	args := RunArgs(cfg, cfg.SizePlace(first, "ru-large"), "ru-large")
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		values[args[i]] = args[i+1]
	}
	if values["--cores"] != strconv.Itoa(cfg.Pools[1].Cores) || values["--memory"] != strconv.Itoa(cfg.Pools[1].MemoryMB) {
		t.Fatalf("agent arguments: %v", args)
	}
	t.Logf("VM launch arguments: %v", args)
	h.launcher.finish(t, "box-1")
}
