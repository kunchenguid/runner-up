//go:build !windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (README.md, "Setting up": "runner-up status shows each place, its
// running reservation and the waiting work"; "When a machine is down":
// "runner-up status shows a resting machine's places as resting"; "When a
// place fails to start": status shows the place resting until when): status
// gives each place one line saying whether it is free, which job or borrow
// holds it, or that its machine or the place itself rests, then each waiting
// job and borrow and each job acquired for a place. This test owns those
// lines' words; journey tests read the state status reads (place.json,
// busy.lock, state.json) instead of typing them.
// TestDrainCommandAdmitsThePairOnlyOnceItsPlacesAreQuiet owns a drain's
// lines, which need a drain a coordinator moves on.
func TestStatusSaysEachPlaceAndJobInOneLine(t *testing.T) {
	cfg := initialSetup(t)
	docker := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}
	}
	cfg.Machines = []config.Machine{
		{Name: "box", Places: []config.Place{docker("box-1"), docker("box-2"), docker("box-3"), docker("box-4"), docker("box-5"),
			docker("box-6")}},
		{Name: "far", Places: []config.Place{docker("far-1")}},
	}
	cfg.Defaults()
	at := time.Date(2026, 11, 3, 9, 5, 0, 0, time.UTC)
	since := at.Local().Format(time.RFC3339)
	until := time.Now().Add(15 * time.Minute).Truncate(time.Second)
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
		// box-5 rests until a quarter of an hour from now; box-6's rest
		// ended.
		"failedStarts": map[string]coordinator.PlaceRest{
			"box-5": {Failures: 5, Until: until},
			"box-6": {Failures: 1, Until: at},
		},
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
		`place box-5 on box (docker, 2 cores, 4096 MB) resting (5 failed starts in a row; takes jobs again at ` +
			until.Local().Format(time.RFC3339) + `)`,
		`place box-6 on box (docker, 2 cores, 4096 MB) free`,
		`place far-1 on far (docker, 2 cores, 4096 MB) resting (agent unreachable since ` + since + `)`,
		`waiting octo-org/app ru-linux "suite" since ` + since,
		`waiting borrow ru-linux "second check" since ` + since,
		`starting octo-org/app ru-linux "build" in box-1`,
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("status said\n%s\nwant\n%s", out.String(), want)
	}
}

// Intent (README, "Setup file", minAvailableMemoryMB; issue #56): a place
// held back by its machine's free-memory floor shows in status as busy, with
// the coordinator's latest reading against the floor, and check-config says
// the machine keeps one.
func TestStatusSaysAPlaceWaitsUnderTheFreeMemoryFloor(t *testing.T) {
	cfg := initialSetup(t)
	kvm := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderKVM, Pools: []string{"ru-linux"}, Cores: 4, MemoryMB: 8192,
			Image: "/var/lib/runner-up/noble-server-cloudimg-amd64.img"}
	}
	cfg.Machines = []config.Machine{{Name: "desk", SSH: []string{"ci@desk"}, Cores: 16, MemoryMB: 32768, RoomCores: 4, RoomMemoryMB: 16384,
		MinAvailableMemoryMB: 4096, Places: []config.Place{kvm("desk-1")}}}
	cfg.Defaults()
	at := time.Date(2026, 11, 3, 9, 5, 0, 0, time.UTC)
	state := map[string]any{"memory": map[string]coordinator.MemoryReading{"desk": {At: at, Memory: agent.Memory{AvailableMB: 2048}}}}
	if err := config.WriteJSON(filepath.Join(cfg.StateDir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := status(cfg, &out); code != 0 {
		t.Fatalf("status exited %d", code)
	}
	want := "place desk-1 on desk (kvm, 4 cores, 8192 MB) busy (free memory 2048 MB, under the floor 4096 MB + 8192 MB for this job)\n"
	if out.String() != want {
		t.Errorf("status said\n%s\nwant\n%s", out.String(), want)
	}
	line := machineLine(cfg.Machines[0])
	if !strings.HasSuffix(line, "all at once; a place starts only while 4096 MB stays free beside its places") {
		t.Errorf("check-config says %q", line)
	}
}
