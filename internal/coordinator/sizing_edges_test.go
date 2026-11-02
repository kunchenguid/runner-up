//go:build !windows

package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (INF-2112 / RUU-28): keeping room for a waiting job on one machine
// must not stop compatible work on another machine. The messages are the
// same recorded release and check offers used by the placement tests.
func TestPoolReservationLeavesAnotherMachineAvailable(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := onePlace(t.TempDir(), "pool:ru-large")
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 2, 4096
	cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 4, 8192
	m := &cfg.Machines[0]
	m.Cores = 4
	m.MemoryMB = 8192
	other := m.Places[0]
	other.ID = "box-busy"
	m.Places = append(m.Places, other)
	cfg.Machines = append(cfg.Machines, config.Machine{Name: "other", Cores: 2, MemoryMB: 4096, Places: []config.Place{
		{ID: "other-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
	}})
	busy := occupy(t, cfg, "box-busy")
	defer busy.Close()
	config.WriteJSON(filepath.Join(cfg.PlaceDir("box-busy"), "place.json"), PlaceRun{Started: true, Reservation: cfg.Pools[0].Size()})
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 1, 2)
	h.feedRange(f, 0, 1)
	if len(h.launcher.held) != 1 {
		t.Fatalf("placed %v", h.placed())
	}
	if _, ok := h.launcher.held["other-1"]; !ok {
		t.Fatalf("the unrelated machine was held: %v", h.placed())
	}
	h.launcher.finish(t, "other-1")
}

// Intent (INF-2112 / RUU-28): a running job keeps its reservation. A missing
// snapshot must not make a large running job look like a smaller place
// default, even after the configured kind size changes.
func TestUnknownRunningReservationKeepsTheCountedBudget(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := onePlace(t.TempDir())
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 2, 4096
	cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 4, 8192
	m := &cfg.Machines[0]
	m.Cores = 14
	m.MemoryMB = 49152
	other := m.Places[0]
	other.ID = "box-2"
	m.Places = append(m.Places, other)
	busy := occupy(t, cfg, "box-1")
	defer busy.Close()
	// occupy writes a readable legacy record; remove it to model a live
	// runner whose reservation snapshot is unavailable.
	if err := os.Remove(filepath.Join(cfg.PlaceDir("box-1"), "place.json")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedRange(f, 0, 1)
	if len(h.launcher.held) != 0 {
		t.Fatalf("unknown live reservation allowed new work: %v", h.placed())
	}
	// Readable legacy records still mean the pre-sizing place default.
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir("box-1"), "place.json"), PlaceRun{Started: true}); err != nil {
		t.Fatal(err)
	}
	h.svc.Place(context.Background())
	if len(h.launcher.held) != 1 {
		t.Fatalf("legacy reservation held too much: %v", h.placed())
	}
	h.launcher.finish(t, "box-2")
	os.Remove(filepath.Join(cfg.PlaceDir("box-1"), "place.json"))
}
