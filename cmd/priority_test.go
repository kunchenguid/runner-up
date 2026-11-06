package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Setup file", priority; asked for as levels where 0 is used
// first, then 1, then 2, and nobody who skips them changes anything): init
// and machine add write no priority, and check-config shows each place's
// level and, for each pool on several levels, its places in the order tried.
// Which priorities are refused is owned by internal/config
// (TestPriorityLevels); how the planner uses them by internal/plan.
func TestCheckConfigShowsPriorityLevels(t *testing.T) {
	cfg := initialSetup(t)
	cfg.Pools = append(cfg.Pools, config.Pool{Label: "ru-mac"})
	o := placeOptions{Provider: config.ProviderHost, RoomCores: 1, RoomMemoryMB: 1024, Pools: []string{"ru-linux", "ru-mac"}}
	if err := addMachine(cfg, config.Machine{Name: "big", SSH: []string{"you@big"}}, macTartProbe, o); err != nil {
		t.Fatal(err)
	}
	o = placeOptions{Image: "ghcr.io/cirruslabs/macos-sequoia-base:latest", RoomCores: 1, RoomMemoryMB: 1024, Pools: []string{"ru-mac"}}
	if err := addMachine(cfg, config.Machine{Name: "small", SSH: []string{"you@small"}}, macTartProbe, o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(written, []byte(`"priority"`)) {
		t.Fatalf("init and machine add wrote a priority:\n%s", written)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	checkConfig(loaded, &out)
	if strings.Contains(out.String(), "priority") {
		t.Errorf("check-config speaks of priorities nobody set:\n%s", out.String())
	}

	// The big Mac's own place goes behind the small Mac's guests, though it
	// is listed first.
	loaded.Machines[0].Priority = 1
	loaded.Defaults()
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	checkConfig(loaded, &out)
	big, small := loaded.Machines[0].Places[0], loaded.Machines[1].Places
	want := "pool ru-mac tries priority 0: " + small[0].ID + ", " + small[1].ID + "; then priority 1: " + big.ID + "\n"
	if !strings.Contains(out.String(), want) || !strings.Contains(machineLine(loaded.Machines[0]), fmt.Sprintf("%s host %d cores %d MB priority 1", big.ID, big.Cores, big.MemoryMB)) {
		t.Errorf("check-config says\n%s\nwant the place's level and\n%s", out.String(), want)
	}
	if strings.Contains(out.String(), "pool ru-linux tries") {
		t.Errorf("check-config orders ru-linux, whose places are all on one level:\n%s", out.String())
	}
	t.Logf("$ runner-up check-config\n%s", out.String())
}
