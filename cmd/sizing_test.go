package cmd

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (INF-2112 / RUU-28): a person can configure one pool size that
// replaces all its place sizes, while old setups keep working. Partial,
// negative and impossible sizes must be rejected before a runner starts.
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
	for _, size := range []config.Share{{Cores: 4}, {MemoryMB: 8192}, {Cores: -1, MemoryMB: 8192}, {Cores: macTartProbe.Cores, MemoryMB: 8192}, {Cores: 4, MemoryMB: macTartProbe.MemoryMB}} {
		cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = size.Cores, size.MemoryMB
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid pool size accepted: %+v", size)
		}
	}
	cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 0, 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SizePlace(cfg.Places()[0], "ru-linux"); got.Cores != 2 || got.MemoryMB != 4096 {
		t.Fatalf("legacy size changed: %+v", got)
	}
}
