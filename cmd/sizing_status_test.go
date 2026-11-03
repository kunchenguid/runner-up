//go:build !windows

package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent (README, "Setting up" and "Job sizes and readings"): status must
// describe the pool's actual resource fit and a running job's frozen
// reservation, not its place's fallback size.
func TestStatusUsesPoolSizesAndRunningReservations(t *testing.T) {
	cfg := initialSetup(t)
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 4, 8192
	cfg.Machines = []config.Machine{{Name: "mac", Cores: macTartProbe.Cores, RoomCores: 1, MemoryMB: macTartProbe.MemoryMB,
		Places: []config.Place{
			{ID: "busy", Provider: config.ProviderHost, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
			{ID: "idle", Provider: config.ProviderHost, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
		},
	}}
	cfg.Defaults()
	os.MkdirAll(cfg.PlaceDir("busy"), 0700)
	held, err := lock.Busy(filepath.Join(cfg.PlaceDir("busy"), "busy.lock"))
	if err != nil || held == nil {
		t.Fatalf("lock: %v", err)
	}
	defer held.Close()
	reservation := config.Share{Cores: 6, MemoryMB: 8192}
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir("busy"), "place.json"), coordinator.PlaceRun{Started: true, Reservation: reservation}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	status(cfg, &out)
	if !strings.Contains(out.String(), fmt.Sprintf("place busy on mac (host, %d cores, %d MB) busy (job)", reservation.Cores, reservation.MemoryMB)) ||
		!strings.Contains(out.String(), fmt.Sprintf("place idle on mac (host, 2 cores, 4096 MB) busy (cores %d of %d in use", reservation.Cores, cfg.Machines[0].Budget().Cores)) {
		t.Fatalf("status used fallback sizes:\n%s", out.String())
	}
	cfg.Pools[0].Cores = 2
	out.Reset()
	status(cfg, &out)
	if !strings.Contains(out.String(), "place idle on mac (host, 2 cores, 4096 MB) free") ||
		!strings.Contains(out.String(), fmt.Sprintf("place busy on mac (host, %d cores, %d MB) busy (job)", reservation.Cores, reservation.MemoryMB)) {
		t.Fatalf("status lost a running reservation or hid a fitting pool:\n%s", out.String())
	}
	t.Logf("$ runner-up status (after reducing the next job's pool size)\n%s", out.String())
}
