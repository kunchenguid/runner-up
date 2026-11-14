package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Intent (README, "Setup file"): the setup checks refuse, when the setup
// loads, a setup no job could run under, and say what to change. They are
// plain functions of the setup, so this package owns them; the command
// package owns only what init, machine add and check-config do with them.

// initialSetup is what runner-up init writes for one repository.
func initialSetup(t *testing.T) *Config {
	cfg := &Config{StateDir: t.TempDir(), GitHub: GitHub{Repos: []string{"octo-org/app"}}, Pools: []Pool{{Label: "ru-linux"}}}
	cfg.Defaults()
	return cfg
}

// Intent (README, "Setup file"): a pool label GitHub's own runners, or every
// self-hosted runner, answer to is refused, so a pool's jobs never go to
// another runner.
func TestPoolLabelsOtherRunnersAnswerToAreRefused(t *testing.T) {
	for _, label := range []string{"ubuntu-latest", "macos-14", "windows-2025", "self-hosted", "Linux", "x64", "two words", ""} {
		cfg := initialSetup(t)
		cfg.Pools = []Pool{{Label: label}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("pool label %q was accepted", label)
		}
	}
	if err := initialSetup(t).Validate(); err != nil {
		t.Fatal(err)
	}
}

// Intent (README, "Setup file": each machine's roomCores and roomMemoryMB
// keep room for its owner): a machine whose owner keeps every core leaves
// its places nothing, so it is refused.
func TestOwnersRoomMustLeaveThePlacesSomething(t *testing.T) {
	cfg := initialSetup(t)
	cfg.Machines = []Machine{{Name: "lima", SSH: []string{"ci@lima"}, Cores: 4, MemoryMB: 5910, RoomCores: 4,
		Places: []Place{{ID: "lima-1", Provider: ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 2048}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "the room kept for its owner") {
		t.Fatalf("a machine whose owner keeps every core was accepted: %v", err)
	}
}

// Intent (README, "Setup file": "check-config rejects partial sizes and
// sizes that never fit a machine serving the pool, after its owner's room"):
// a pool's size replaces its places' sizes, a pool without one keeps them,
// and a partial, negative or impossible size is refused before a runner
// starts.
func TestPoolSizesThatCouldNeverRunAreRefused(t *testing.T) {
	// A MacBook with 10 cores and 24576 MB, as runner-up agent probe read it,
	// keeping 1 core and 1024 MB for its owner.
	mac := Machine{Name: "mac", Cores: 10, MemoryMB: 24576, RoomCores: 1, RoomMemoryMB: 1024,
		Places: []Place{{ID: "mac-1", Provider: ProviderHost, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}}
	cfg := initialSetup(t)
	cfg.Machines = []Machine{mac}
	cfg.Defaults()
	b := mac.Budget()
	for _, size := range []Share{{Cores: 4}, {MemoryMB: 8192}, {Cores: -1, MemoryMB: 8192},
		{Cores: b.Cores + 1, MemoryMB: 8192}, {Cores: 4, MemoryMB: b.MemoryMB + 1}} {
		cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = size.Cores, size.MemoryMB
		if err := cfg.Validate(); err == nil {
			t.Errorf("pool size %+v was accepted", size)
		}
	}
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = b.Cores, b.MemoryMB
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a pool the size of the machine's budget was refused: %v", err)
	}
	if got := share(cfg.SizePlace(cfg.Places()[0], "ru-linux")); got != b {
		t.Errorf("a ru-linux job reserves %+v, want the pool's %+v", got, b)
	}
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := share(cfg.SizePlace(cfg.Places()[0], "ru-linux")), share(mac.Places[0]); got != want {
		t.Fatalf("a pool without a size changed its place's %+v to %+v", want, got)
	}
}

func share(p Place) Share { return Share{Cores: p.Cores, MemoryMB: p.MemoryMB} }

// Intent (README, "Setup file", poolImages): a pool image no job could run on
// is refused when the setup loads, not when a job reaches it.
func TestPoolImagesNoJobCouldRunOnAreRefused(t *testing.T) {
	// An absolute path on the system the test runs on, as the coordinator's
	// own system reads the KVM rule.
	noble := filepath.Join(t.TempDir(), "noble.img")
	for _, tc := range []struct {
		why  string
		p    Place
		want string
	}{
		{"a pool the place does not serve", Place{Provider: ProviderDocker, PoolImages: map[string]string{"ru-large": "runner:large"}},
			`place "lima-1": poolImages names pool "ru-large", which the place does not serve`},
		{"no image", Place{Provider: ProviderDocker, PoolImages: map[string]string{"ru-linux": ""}},
			`place "lima-1": poolImages gives pool "ru-linux" no image`},
		{"a host place", Place{Provider: ProviderHost, PoolImages: map[string]string{"ru-linux": "runner:2604"}},
			`place "lima-1": pool "ru-linux"'s image: a host place runs on the machine itself and takes no image`},
		{"a KVM disk that is not an absolute path", Place{Provider: ProviderKVM, Image: noble,
			PoolImages: map[string]string{"ru-linux": "resolute.img"}},
			`place "lima-1": pool "ru-linux"'s image: a KVM place needs the absolute path of its base disk image on the machine`},
		{"a tart image with a space", Place{Provider: ProviderTart, Image: "ci-base",
			PoolImages: map[string]string{"ru-linux": "ci base"}},
			`place "lima-1": pool "ru-linux"'s image: a tart place needs the tart image it clones`},
	} {
		cfg := initialSetup(t)
		cfg.Pools = append(cfg.Pools, Pool{Label: "ru-large"})
		tc.p.ID, tc.p.Pools, tc.p.Cores, tc.p.MemoryMB = "lima-1", []string{"ru-linux"}, 2, 2048
		big := Place{ID: "lima-2", Provider: ProviderDocker, Pools: []string{"ru-large"}, Cores: 2, MemoryMB: 2048}
		cfg.Machines = []Machine{{Name: "lima", SSH: []string{"ci@lima"}, Places: []Place{tc.p, big}}}
		if err := cfg.Validate(); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: validate said %v, want %q", tc.why, err, tc.want)
		}
	}
}

// Intent (README, "Setup file", network): a place that names a Docker
// network is refused when the setup loads unless it is a Docker place and
// the name is one Docker takes as a network's.
func TestDockerNetworksNoPlaceCouldJoinAreRefused(t *testing.T) {
	for _, tc := range []struct {
		why  string
		p    Place
		want string
	}{
		{"a host place", Place{Provider: ProviderHost, Network: "runner-jobs"},
			`place "lima-1": only a Docker place joins a Docker network`},
		{"another container's network", Place{Provider: ProviderDocker, Network: "container:proxy"},
			`place "lima-1": a Docker network's name starts with a letter or digit`},
		{"two words", Place{Provider: ProviderDocker, Network: "runner jobs"},
			`place "lima-1": a Docker network's name starts with a letter or digit`},
		{"a leading dash", Place{Provider: ProviderDocker, Network: "-jobs"},
			`place "lima-1": a Docker network's name starts with a letter or digit`},
		{"the machine's own network", Place{Provider: ProviderDocker, Network: "host"},
			`place "lima-1": a Docker place can't join Docker's "host" network`},
		{"no network", Place{Provider: ProviderDocker, Network: "none"},
			`place "lima-1": a Docker place can't join Docker's "none" network`},
		{"a Docker network", Place{Provider: ProviderDocker, Network: "runner-jobs_1.lan"}, ""},
	} {
		cfg := initialSetup(t)
		tc.p.ID, tc.p.Pools, tc.p.Cores, tc.p.MemoryMB = "lima-1", []string{"ru-linux"}, 2, 2048
		cfg.Machines = []Machine{{Name: "lima", SSH: []string{"ci@lima"}, Places: []Place{tc.p}}}
		err := cfg.Validate()
		if tc.want == "" && err != nil {
			t.Errorf("%s: refused: %v", tc.why, err)
		}
		if tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
			t.Errorf("%s: validate said %v, want %q", tc.why, err, tc.want)
		}
	}
}

// Intent (README, "Updating runner-up": the setup names a runner-up version
// and each platform's SHA-256): a release named as the release workflow tags
// it, with each platform's archive SHA-256 as its checksums.txt lists it, is
// accepted; a release the coordinator could not fetch or check is refused
// when the setup loads.
func TestTheSetupsReleaseNamesAVersionAndEachArchivesSHA256(t *testing.T) {
	// runner-up-v9.0.2-windows-amd64.zip's line in the release fixture's
	// checksums.txt (internal/release/testdata).
	const sum = "1a2dc4c30dbf507b3762d4a738564654c94213cb4879b801c2d994c62099f154"
	cfg := initialSetup(t)
	cfg.Release = &Release{Version: "v0.1.11", SHA256: map[string]string{"darwin-arm64": sum, "windows-amd64": sum}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a release as the workflow publishes it was refused: %v", err)
	}
	for _, tc := range []struct {
		why  string
		r    Release
		want string
	}{
		{"a version without its v", Release{Version: "0.1.11", SHA256: map[string]string{"darwin-arm64": sum}},
			`release: version "0.1.11" is not a release version such as v0.1.11`},
		{"no archive", Release{Version: "v0.1.11"}, "release: sha256 names no platform's archive"},
		{"a platform no release has", Release{Version: "v0.1.11", SHA256: map[string]string{"macos-arm64": sum}},
			`release: sha256 names platform "macos-arm64"`},
		{"a SHA-256 in capitals", Release{Version: "v0.1.11", SHA256: map[string]string{"linux-amd64": strings.ToUpper(sum)}},
			"release: linux-amd64's sha256 is not 64 lowercase hex digits"},
		{"a URL that is not a web address", Release{Version: "v0.1.11", SHA256: map[string]string{"linux-amd64": sum}, URL: "file:///srv/releases"},
			"release: url must be an http or https address"},
	} {
		cfg := initialSetup(t)
		cfg.Release = &tc.r
		if err := cfg.Validate(); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: validate said %v, want %q", tc.why, err, tc.want)
		}
	}
}

// Intent (README, "Windows machines"): the coordinator reaches a Windows
// machine over SSH, whose sign-in runs cmd or PowerShell, so a setup that a
// Windows machine's shell could not pass unchanged, or a provider the
// machine's system cannot run, is refused before any job.
func TestWindowsSetupThatTheShellWouldChangeIsRefused(t *testing.T) {
	place := func(p Place) Place {
		if p.ID == "" {
			p.ID = "win-pc-1"
		}
		if p.Provider == "" {
			p.Provider = ProviderAccount
		}
		p.Cores, p.MemoryMB, p.Pools = 2, 4096, []string{"ru-linux"}
		return p
	}
	for _, tc := range []struct {
		why  string
		m    Machine
		want string
	}{
		{"a Windows machine without ssh", Machine{Name: "pc", OS: "windows", Places: []Place{place(Place{})}},
			"a Windows machine needs ssh"},
		{"an agent path with a space", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Agent: `C:\Program Files\runner-up\runner-up.exe`, Places: []Place{place(Place{})}},
			"must be a plain program name or path"},
		{"a Docker place on Windows", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Provider: ProviderDocker})}},
			"docker places do not run on Windows machines"},
		{"an account place on Linux", Machine{Name: "lima", SSH: []string{"ci@lima"}, Places: []Place{place(Place{})}},
			"account places run only on Windows machines"},
		{"a Hyper-V image that is not a full path", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Provider: ProviderHyperV, Image: `noble.vhdx`})}},
			"a Hyper-V place needs the full path"},
		{"an image with a variable cmd would expand", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Provider: ProviderHyperV, Image: `C:\Users\%USERNAME%\noble.vhdx`})}},
			"cannot pass to a Windows machine's shell unchanged"},
		{"a pool's image with a variable cmd would expand", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Provider: ProviderHyperV, Image: `C:\vm\noble.vhdx`, PoolImages: map[string]string{"ru-linux": `C:\Users\%USERNAME%\resolute.vhdx`}})}},
			`pool "ru-linux"'s image: "C:\\Users\\%USERNAME%\\resolute.vhdx" cannot pass to a Windows machine's shell unchanged`},
		{"a switch name PowerShell would expand", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Provider: ProviderHyperV, Image: `C:\vm\noble.vhdx`, Switch: "$env:x"})}},
			"cannot pass to a Windows machine's shell unchanged"},
		{"a place id that is a path", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{ID: `pc\1`})}},
			"has only letters, digits"},
		{"a switch on an account place", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Switch: "Default Switch"})}},
			"switch"},
		{"an image on an account place", Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []Place{place(Place{Image: `C:\vm\noble.vhdx`})}},
			"image"},
	} {
		cfg := initialSetup(t)
		cfg.Machines = []Machine{tc.m}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: validate said %v, want %q", tc.why, err, tc.want)
		}
	}
}

// Intent (README, "Setup file", priority; asked for as levels where 0 is
// used first, then 1, then 2): a place takes its machine's priority unless it
// sets its own, unset is 0, and a negative priority, or priorities that order
// no pool's places, are refused, while a setup without any loads as before.
func TestPriorityLevels(t *testing.T) {
	one, minus := 1, -1
	setup := func() *Config {
		cfg := initialSetup(t)
		cfg.Pools = append(cfg.Pools, Pool{Label: "ru-mac"})
		cfg.Machines = []Machine{
			{Name: "small", SSH: []string{"ci@small"}, Places: []Place{{ID: "small-vm", Provider: ProviderTart, Image: "lane", Pools: []string{"ru-mac"}, Cores: 4, MemoryMB: 8192}}},
			{Name: "big", Places: []Place{
				{ID: "big-real", Provider: ProviderHost, Pools: []string{"ru-mac"}, Cores: 6, MemoryMB: 16384},
				{ID: "big-linux", Provider: ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}},
		}
		return cfg
	}
	load := func(cfg *Config) error { cfg.Defaults(); return cfg.Validate() }

	cfg := setup()
	if err := load(cfg); err != nil {
		t.Fatalf("a setup without priorities was refused: %v", err)
	}
	for _, p := range cfg.Places() {
		if p.Level != 0 {
			t.Errorf("place %s without a priority is at level %d", p.ID, p.Level)
		}
	}

	cfg = setup()
	cfg.Machines[1].Priority = 2
	cfg.Machines[1].Places[0].Priority = &one
	if err := load(cfg); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"small-vm": 0, "big-real": 1, "big-linux": 2} {
		if p, _ := cfg.Place(id); p.Level != want {
			t.Errorf("place %s is at level %d, want %d", id, p.Level, want)
		}
	}
	if levels := cfg.PoolLevels("ru-mac"); len(levels) != 2 || levels[0][0].ID != "small-vm" || levels[1][0].ID != "big-real" {
		t.Errorf("ru-mac's levels are %v, want small-vm then big-real", levels)
	}

	for why, change := range map[string]func(*Config){
		"a negative machine priority": func(c *Config) { c.Machines[0].Priority = -1 },
		"a negative place priority":   func(c *Config) { c.Machines[0].Places[0].Priority = &minus },
		// big-linux alone serves ru-linux, so a level on it orders nothing.
		"a priority that orders no pool": func(c *Config) { c.Machines[1].Places[1].Priority = &one },
		"every place at one level":       func(c *Config) { c.Machines[0].Priority, c.Machines[1].Priority = 1, 1 },
	} {
		cfg := setup()
		change(cfg)
		if err := load(cfg); err == nil || !strings.Contains(err.Error(), "priority") {
			t.Errorf("%s was not refused for its priority: %v", why, err)
		}
	}
}
