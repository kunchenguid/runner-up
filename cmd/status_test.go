//go:build !windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (README.md, "Setting up": "runner-up status shows each place, its
// running reservation and the waiting work"; "When a machine is down":
// "runner-up status shows a resting machine's places as resting"): status
// gives each place one line saying whether it is free, which job or borrow
// holds it, or that its machine rests, then each waiting job and borrow and
// each job acquired for a place. This test owns those lines' words; journey
// tests read the state status reads (place.json, busy.lock, state.json)
// instead of typing them. TestDrainCommandAdmitsThePairOnlyOnceItsPlacesAreQuiet
// owns a drain's lines, which need a drain a coordinator moves on.
func TestStatusSaysEachPlaceAndJobInOneLine(t *testing.T) {
	cfg := initialSetup(t)
	docker := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}
	}
	cfg.Machines = []config.Machine{
		{Name: "box", Places: []config.Place{docker("box-1"), docker("box-2"), docker("box-3"), docker("box-4")}},
		{Name: "far", Places: []config.Place{docker("far-1")}},
	}
	cfg.Defaults()
	at := time.Date(2026, 11, 3, 9, 5, 0, 0, time.UTC)
	since := at.Local().Format(time.RFC3339)
	hold := func(id string, run coordinator.PlaceRun) {
		os.MkdirAll(cfg.PlaceDir(id), 0o700)
		held, err := lock.Busy(filepath.Join(cfg.PlaceDir(id), "busy.lock"))
		if err != nil || held == nil {
			t.Fatalf("lock %s: %v", id, err)
		}
		t.Cleanup(func() { held.Close() })
		run.Reservation = config.Share{Cores: 2, MemoryMB: 4096}
		if err := config.WriteJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), run); err != nil {
			t.Fatal(err)
		}
	}
	hold("box-2", coordinator.PlaceRun{Repo: "octo-org/app", JobName: "verify / linux"})
	hold("box-3", coordinator.PlaceRun{Repo: "octo-org/app", JobName: "affected-journeys", Started: true})
	hold("box-4", coordinator.PlaceRun{BorrowID: "request-check", JobName: "native check", LaunchedAt: at})
	// state.json as the coordinator saves it; an acquired job's type is the
	// coordinator's own, so it is written as JSON.
	state := map[string]any{
		"waiting": []plan.Job{
			{Repo: "octo-org/app", Pool: "ru-linux", Name: "suite", QueueTime: at},
			{BorrowID: "request-two", Pool: "ru-linux", Name: "second check", QueueTime: at},
		},
		"acquired":    []map[string]any{{"job": plan.Job{Repo: "octo-org/app", Pool: "ru-linux", Name: "build"}, "place": "box-1", "at": at}},
		"unreachable": map[string]time.Time{"far": at},
	}
	if err := config.WriteJSON(filepath.Join(cfg.StateDir, "state.json"), state); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := status(cfg, &out); code != 0 {
		t.Fatalf("status exited %d", code)
	}
	want := strings.Join([]string{
		`place box-1 on box (docker, 2 cores, 4096 MB) free`,
		`place box-2 on box (docker, 2 cores, 4096 MB) busy (job): octo-org/app "verify / linux" waiting for its job`,
		`place box-3 on box (docker, 2 cores, 4096 MB) busy (job): octo-org/app "affected-journeys" running`,
		`place box-4 on box (docker, 2 cores, 4096 MB) busy (borrow): borrow "native check" since ` + since,
		`place far-1 on far (docker, 2 cores, 4096 MB) resting (agent unreachable since ` + since + `)`,
		`waiting octo-org/app ru-linux "suite" since ` + since,
		`waiting borrow ru-linux "second check" since ` + since,
		`starting octo-org/app ru-linux "build" in box-1`,
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("status said\n%s\nwant\n%s", out.String(), want)
	}
}
