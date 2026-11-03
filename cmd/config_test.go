package cmd

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Setting up", "Setup file" and "Macs"): init then machine add
// over SSH runs a job with no hand-written setup file, within each machine's
// cores and memory less its owner's room, on Linux machines and on Macs.
// machine add gives a machine as many places as fit its cores and memory less
// the room kept for its owner, and no more tart places than macOS runs VMs at
// once, refuses a machine that cannot run the places asked for, and writes a
// setup that loads as written. The setup checks themselves are owned by
// internal/config (config_test.go).

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
	// The readiness Apple container places gained with the provider's ready
	// method: a Mac without the container tool is refused up front.
	noContainer := macTartProbe
	noContainer.Container = "Apple's container tool is not installed (brew install container)"
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
		{noContainer, placeOptions{Provider: config.ProviderContainer, Image: "runner-up-linux:2026-11-03"},
			"lima cannot run Apple container places: Apple's container tool is not installed"},
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

// Intent (README, "Setup file", poolImages): a PC's five Hyper-V places serve
// the 24.04 and 26.04 pools alike, each pool on its own base disk: poolImages
// loads as written and leaves the places, their shares and the machine's budget
// as they were.
func TestPoolImagesKeepThePlacesAndTheirShares(t *testing.T) {
	pc := func(poolImages map[string]string) *config.Config {
		cfg := initialSetup(t)
		cfg.Pools = []config.Pool{{Label: "ru-linux"}, {Label: "ru-linux-gate"}, {Label: "ru-linux-2604"}}
		m := config.Machine{Name: "pc", SSH: []string{"ci@pc"}, OS: "windows", Cores: 24, MemoryMB: 65536, RoomCores: 4, RoomMemoryMB: 16384}
		for i := 1; i <= 5; i++ {
			m.Places = append(m.Places, config.Place{ID: "pc-" + strconv.Itoa(i), Provider: config.ProviderHyperV,
				Pools: []string{"ru-linux", "ru-linux-gate", "ru-linux-2604"}, Cores: 4, MemoryMB: 8192,
				Image: `C:\runner-up\job.vhdx`, PoolImages: poolImages})
		}
		cfg.Machines = []config.Machine{m}
		return cfg
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, pc(map[string]string{"ru-linux-2604": `C:\runner-up\job-2604.vhdx`})); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("the setup does not load: %v", err)
	}
	if got, want := machineLine(loaded.Machine("pc")), machineLine(pc(nil).Machines[0]); got != want {
		t.Errorf("check-config says\n%s\nwant, as with one image\n%s", got, want)
	}
	if n := len(loaded.Places()); n != 5 {
		t.Errorf("%d places, want 5", n)
	}
	for _, p := range loaded.Places() {
		for pool, want := range map[string]string{"ru-linux": `C:\runner-up\job.vhdx`, "ru-linux-gate": `C:\runner-up\job.vhdx`,
			"ru-linux-2604": `C:\runner-up\job-2604.vhdx`} {
			if got := p.ImageFor(pool); got != want {
				t.Errorf("place %s runs a %s job on %q, want %q", p.ID, pool, got, want)
			}
		}
	}
}
