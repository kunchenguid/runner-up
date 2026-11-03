package cmd

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Setup file"): a person can configure one pool size that
// replaces all its place sizes: it survives the written setup, and
// check-config shows it. Which sizes are refused is owned by internal/config
// (TestPoolSizesThatCouldNeverRunAreRefused).
func TestConfiguredPoolSizes(t *testing.T) {
	cfg := initialSetup(t)
	cfg.Machines = []config.Machine{{Name: "mac", Cores: macTartProbe.Cores, MemoryMB: macTartProbe.MemoryMB, RoomCores: 1, RoomMemoryMB: 1024,
		Places: []config.Place{{ID: "mac-1", Provider: config.ProviderHost, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}}}
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 4, 8192
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := loaded.SizePlace(loaded.Places()[0], "ru-linux")
	if p.Cores != cfg.Pools[0].Cores || p.MemoryMB != cfg.Pools[0].MemoryMB {
		t.Fatalf("pool size lost: %+v", p)
	}
	var out bytes.Buffer
	if checkConfig(loaded, &out) != 0 || !strings.Contains(out.String(), fmt.Sprintf("pool ru-linux: each job reserves %d cores, %d MB", cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB)) {
		t.Fatalf("check-config: %s", out.String())
	}
	t.Logf("$ runner-up check-config\n%s", out.String())
}
