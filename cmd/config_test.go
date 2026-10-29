package cmd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the design): "init then machine add over SSH
// runs a job with no hand-written setup file", within "per-machine core/memory
// budgets with owner room", on Linux machines and on Macs ("tart VM and host
// on macOS", "the two-macOS-VM limit"). machine add gives a machine as many
// places as fit its cores and memory less the room kept for its owner, and no
// more tart places than macOS runs VMs at once, refuses a machine that cannot
// run the places asked for, and writes a setup that loads as written. A pool
// label GitHub's own runners, or every self-hosted runner, answer to is
// refused, so pools stay apart from other runners.

// limaProbe is what machine add read of the Linux test machine over SSH: a
// Lima VM with 4 cores and 5910 MB running Ubuntu 24.04 on arm64, with Docker
// and KVM ready.
var limaProbe = agent.Probe{Version: "dev", OS: "linux", Arch: "arm64", Cores: 4, MemoryMB: 5910, Docker: "ok", KVM: "ok"}

// macProbe is what runner-up agent probe read of a MacBook with Apple silicon,
// 10 cores and 24 GB, before tart was installed; macTartProbe is the same Mac
// with tart.
var macProbe = agent.Probe{Version: "dev", OS: "darwin", Arch: "arm64", Cores: 10, MemoryMB: 24576,
	Docker: `exec: "docker": executable file not found in $PATH`, KVM: "KVM places run only on Linux",
	Tart: "tart is not installed (brew install cirruslabs/cli/tart)", Host: "ok"}

var macTartProbe = func() agent.Probe { p := macProbe; p.Tart = "ok"; return p }()

// initialSetup is what runner-up init writes for one repository.
func initialSetup(t *testing.T) *config.Config {
	cfg := &config.Config{StateDir: t.TempDir(), GitHub: config.GitHub{Repos: []string{"octo-org/app"}}, Pools: []config.Pool{{Label: "ru-linux"}}}
	cfg.Defaults()
	return cfg
}

func TestMachineAddFitsPlacesInTheBudgetLessTheOwnersRoom(t *testing.T) {
	lima := config.Machine{Name: "lima", SSH: []string{"ci@lima"}}
	for _, tc := range []struct {
		why    string
		o      placeOptions
		places int
	}{
		// 3 cores and 4886 MB are left for places.
		{"the defaults", placeOptions{Cores: 2, MemoryMB: 4096, RoomCores: 1, RoomMemoryMB: 1024}, 1},
		{"cores run out first", placeOptions{Cores: 1, MemoryMB: 1024, RoomCores: 1, RoomMemoryMB: 1024}, 3},
		{"memory runs out first", placeOptions{Cores: 1, MemoryMB: 2048, RoomCores: 1, RoomMemoryMB: 1024}, 2},
		{"no room kept", placeOptions{Cores: 1, MemoryMB: 1024}, 4},
	} {
		cfg := initialSetup(t)
		if err := addMachine(cfg, lima, limaProbe, tc.o); err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("%s: the written setup does not load: %v", tc.why, err)
		}
		var ids []string
		for _, p := range loaded.Places() {
			if p.Machine != "lima" || p.Provider != config.ProviderDocker || !slices.Equal(p.Pools, []string{"ru-linux"}) ||
				p.Cores != tc.o.Cores || p.MemoryMB != tc.o.MemoryMB {
				t.Errorf("%s: place %+v", tc.why, p)
			}
			ids = append(ids, p.ID)
		}
		if len(ids) != tc.places {
			t.Errorf("%s: %d places %v, want %d", tc.why, len(ids), ids, tc.places)
		}
		m := loaded.Machine("lima")
		if b := m.Budget(); b.Cores != 4-tc.o.RoomCores || b.MemoryMB != 5910-tc.o.RoomMemoryMB {
			t.Errorf("%s: places may reserve %+v", tc.why, b)
		}
	}
}

func TestMachineAddRefusesMachinesThatCannotRunThePlaces(t *testing.T) {
	lima := config.Machine{Name: "lima", SSH: []string{"ci@lima"}}
	dockerMac := macTartProbe
	dockerMac.Docker = "ok"
	noDocker := limaProbe
	noDocker.Docker = "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"
	for _, tc := range []struct {
		pr   agent.Probe
		o    placeOptions
		want string
	}{
		{dockerMac, placeOptions{Provider: config.ProviderDocker}, "lima runs darwin; docker places run on Linux machines only"},
		{macProbe, placeOptions{Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest"}, "lima cannot run tart places: tart is not installed"},
		{macTartProbe, placeOptions{}, "a tart place needs --image"},
		{macTartProbe, placeOptions{Provider: config.ProviderHyperV, Image: `C:\vm\noble.vhdx`}, "lima runs darwin; hyperv places run on Windows machines only"},
		{limaProbe, placeOptions{Provider: config.ProviderTart, Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest"}, "lima cannot run tart places"},
		{noDocker, placeOptions{Cores: 2, MemoryMB: 4096}, "lima cannot run Docker places: permission denied"},
		{limaProbe, placeOptions{Provider: config.ProviderKVM, Cores: 2, MemoryMB: 2048}, "a KVM place needs --image"},
		{limaProbe, placeOptions{Cores: 4, MemoryMB: 4096, RoomCores: 1, RoomMemoryMB: 1024},
			"lima has 3 cores and 4886 MB for places, less than one place of 4 cores and 4096 MB"},
	} {
		cfg := initialSetup(t)
		err := addMachine(cfg, lima, tc.pr, tc.o)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("machine add said %v, want %q", err, tc.want)
		}
		if len(cfg.Machines) != 0 {
			t.Errorf("a refused machine joined the setup: %+v", cfg.Machines)
		}
	}
}

func TestPoolLabelsOtherRunnersAnswerToAreRefused(t *testing.T) {
	for _, label := range []string{"ubuntu-latest", "macos-14", "windows-2025", "self-hosted", "Linux", "x64", "two words", ""} {
		cfg := initialSetup(t)
		cfg.Pools = []config.Pool{{Label: label}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("pool label %q was accepted", label)
		}
	}
	if err := initialSetup(t).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnersRoomMustLeaveThePlacesSomething(t *testing.T) {
	cfg := initialSetup(t)
	cfg.Machines = []config.Machine{{Name: "lima", SSH: []string{"ci@lima"}, Cores: 4, MemoryMB: 5910, RoomCores: 4,
		Places: []config.Place{{ID: "lima-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 2048}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "the room kept for its owner") {
		t.Fatalf("a machine whose owner keeps every core was accepted: %v", err)
	}
}

func TestMachineAddGivesAMacNoMoreTartPlacesThanMacOSRunsVMs(t *testing.T) {
	mac := config.Machine{Name: "mac", SSH: []string{"you@mac"}}
	const image = "ghcr.io/cirruslabs/macos-sequoia-base:latest"
	for _, tc := range []struct {
		why                  string
		o                    placeOptions
		provider             string
		places, cores, memMB int
	}{
		// 9 cores and 23552 MB are left: room for four 2-core places or
		// two 4-core ones, but macOS runs two VMs at once.
		{"tart by default", placeOptions{Image: image, RoomCores: 1, RoomMemoryMB: 1024}, config.ProviderTart, 2, 4, 8192},
		{"small tart places", placeOptions{Image: image, Cores: 2, MemoryMB: 4096, RoomCores: 1, RoomMemoryMB: 1024}, config.ProviderTart, 2, 2, 4096},
		{"asked for three", placeOptions{Image: image, Places: 3, RoomCores: 1, RoomMemoryMB: 1024}, config.ProviderTart, 3, 4, 8192},
		{"the Mac itself", placeOptions{Provider: config.ProviderHost, RoomCores: 1, RoomMemoryMB: 1024}, config.ProviderHost, 1, 2, 4096},
	} {
		cfg := initialSetup(t)
		if err := addMachine(cfg, mac, macTartProbe, tc.o); err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("%s: the written setup does not load: %v", tc.why, err)
		}
		places := loaded.Places()
		if len(places) != tc.places {
			t.Errorf("%s: %d places, want %d", tc.why, len(places), tc.places)
		}
		for _, p := range places {
			if p.Provider != tc.provider || p.Cores != tc.cores || p.MemoryMB != tc.memMB {
				t.Errorf("%s: place %+v", tc.why, p)
			}
			if wantImage := map[string]string{config.ProviderTart: image}[tc.provider]; p.Image != wantImage {
				t.Errorf("%s: place %s clones %q", tc.why, p.ID, p.Image)
			}
		}
		line := machineLine(loaded.Machine("mac"))
		if turns := strings.Contains(line, "tart places take turns"); turns != (tc.places > agent.MacOSVMLimit && tc.provider == config.ProviderTart) {
			t.Errorf("%s: check-config says %q", tc.why, line)
		}
	}
}
