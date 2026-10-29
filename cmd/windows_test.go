package cmd

import (
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
)

// Intent (the design: "the Windows agent
// as a service with Hyper-V VM and fresh-account providers, and one-step PC
// setup"): the coordinator reaches a Windows machine over SSH, whose sign-in
// runs cmd or PowerShell, with a command line both read the same way; machine
// add sets a Windows machine up with account places, or Hyper-V places on a
// base disk; and a setup that a Windows machine's shell could not pass
// unchanged is refused before any job.

// windowsProbe is what `runner-up agent probe` reports on a Windows machine
// set up with runner-up setup --hyperv.
var windowsProbe = agent.Probe{Version: "dev", OS: "windows", Arch: "amd64", Cores: 4, MemoryMB: 16383,
	Docker: "Windows machines run account and hyperv places", KVM: "Windows machines run account and hyperv places",
	Account: "ok", HyperV: "ok"}

// winPC is the Windows test machine as machine add reads it over SSH.
var winPC = config.Machine{Name: "win-pc", SSH: []string{"runner@win-pc"}, OS: "windows"}

func TestWindowsCommandLineReadsTheSameInCmdAndPowerShell(t *testing.T) {
	cfg := initialSetup(t)
	p := config.Place{ID: "win-pc-1", Provider: config.ProviderHyperV, Cores: 4, MemoryMB: 8192,
		Image: `C:\runner-up\noble.vhdx`, Switch: "Default Switch", DiskGB: 64}
	argv := coordinator.AgentCommand(winPC, coordinator.RunArgs(cfg, p)...)
	line := argv[len(argv)-1]
	// Plain words pass as they are; the one with a space is in double
	// quotes, which cmd and PowerShell both take as one argument.
	want := `runner-up agent run --provider hyperv --place win-pc-1 --cores 4 --memory 8192 --boot-timeout 300 --image C:\runner-up\noble.vhdx --disk-gb 64 --switch "Default Switch"`
	if line != want {
		t.Errorf("the command line for win-pc is\n%s\nwant\n%s", line, want)
	}
	if got := strings.Join(argv[:len(argv)-1], " "); got != "ssh -T -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 runner@win-pc" {
		t.Errorf("ssh runs as %q", got)
	}
}

func TestWindowsSetupThatTheShellWouldChangeIsRefused(t *testing.T) {
	place := func(p config.Place) config.Place {
		if p.ID == "" {
			p.ID = "win-pc-1"
		}
		if p.Provider == "" {
			p.Provider = config.ProviderAccount
		}
		p.Cores, p.MemoryMB, p.Pools = 2, 4096, []string{"ru-linux"}
		return p
	}
	for _, tc := range []struct {
		why  string
		m    config.Machine
		want string
	}{
		{"a Windows machine without ssh", config.Machine{Name: "pc", OS: "windows", Places: []config.Place{place(config.Place{})}},
			"a Windows machine needs ssh"},
		{"an agent path with a space", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Agent: `C:\Program Files\runner-up\runner-up.exe`, Places: []config.Place{place(config.Place{})}},
			"must be a plain program name or path"},
		{"a Docker place on Windows", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Provider: config.ProviderDocker})}},
			"docker places do not run on Windows machines"},
		{"an account place on Linux", config.Machine{Name: "lima", SSH: []string{"ci@lima"}, Places: []config.Place{place(config.Place{})}},
			"account places run only on Windows machines"},
		{"a Hyper-V image that is not a full path", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Provider: config.ProviderHyperV, Image: `noble.vhdx`})}},
			"a Hyper-V place needs the full path"},
		{"an image with a variable cmd would expand", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Provider: config.ProviderHyperV, Image: `C:\Users\%USERNAME%\noble.vhdx`})}},
			"cannot pass to a Windows machine's shell unchanged"},
		{"a switch name PowerShell would expand", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Provider: config.ProviderHyperV, Image: `C:\vm\noble.vhdx`, Switch: "$env:x"})}},
			"cannot pass to a Windows machine's shell unchanged"},
		{"a place id that is a path", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{ID: `pc\1`})}},
			"has only letters, digits"},
		{"a switch on an account place", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Switch: "Default Switch"})}},
			"switch"},
		{"an image on an account place", config.Machine{Name: "pc", OS: "windows", SSH: []string{"ci@pc"}, Places: []config.Place{place(config.Place{Image: `C:\vm\noble.vhdx`})}},
			"image"},
	} {
		cfg := initialSetup(t)
		cfg.Machines = []config.Machine{tc.m}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: validate said %v, want %q", tc.why, err, tc.want)
		}
	}
}

func TestMachineAddSetsUpAWindowsMachine(t *testing.T) {
	pr := windowsProbe
	cfg := initialSetup(t)
	if err := addMachine(cfg, config.Machine{Name: "win-pc", SSH: []string{"runner@win-pc"}}, pr, placeOptions{Cores: 2, MemoryMB: 4096, RoomCores: 1, RoomMemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	m := cfg.Machine("win-pc")
	if !m.Windows() || len(m.Places) == 0 {
		t.Fatalf("machine add made %+v", m)
	}
	for _, p := range m.Places {
		if p.Provider != config.ProviderAccount {
			t.Errorf("place %s runs %s, want account places by default on Windows", p.ID, p.Provider)
		}
	}

	noService := pr
	noService.Account = "runner-up's service is not running here; run runner-up setup"
	noHyperV := pr
	noHyperV.HyperV = "no Hyper-V for this account (run runner-up setup --hyperv): Access denied"
	for _, tc := range []struct {
		pr   agent.Probe
		o    placeOptions
		want string
	}{
		{noService, placeOptions{Cores: 2, MemoryMB: 4096}, "win-pc cannot run account places: runner-up's service is not running here"},
		{noHyperV, placeOptions{Provider: config.ProviderHyperV, Image: `C:\vm\noble.vhdx`, Cores: 2, MemoryMB: 4096}, "win-pc cannot run Hyper-V places: no Hyper-V"},
		{pr, placeOptions{Provider: config.ProviderHyperV, Cores: 2, MemoryMB: 4096}, "a Hyper-V place needs --image"},
		{pr, placeOptions{Provider: config.ProviderDocker, Cores: 2, MemoryMB: 4096}, "win-pc cannot run Docker places: Windows machines run account and hyperv places"},
	} {
		cfg := initialSetup(t)
		err := addMachine(cfg, config.Machine{Name: "win-pc", SSH: []string{"runner@win-pc"}}, tc.pr, tc.o)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("machine add said %v, want %q", err, tc.want)
		}
	}
}
