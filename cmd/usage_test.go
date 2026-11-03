package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
)

// Intent (README, "Job sizes and readings"): `runner-up usage` never shows a
// tart guest using more than its own CPUs or memory; such a value reads as
// unavailable. tart sizes a guest to its reservation. A host job is not
// confined to its reservation, so its reading stays. The records are real
// readings from a Mac fleet, renamed: they read 147.70 cores and 83 GB in a
// 4-core, 16 GB guest.
func TestUsageWithholdsGuestReadingsAboveTheGuestsSize(t *testing.T) {
	cfg := initialSetup(t)
	files, err := filepath.Glob(filepath.Join("testdata", "usage", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("records: %v %v", files, err)
	}
	os.MkdirAll(filepath.Join(cfg.StateDir, "usage"), 0700)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(cfg.StateDir, "usage", filepath.Base(f)), b, 0600)
	}
	records, err := coordinator.ReadUsage(cfg)
	if err != nil || len(records) != len(files) {
		t.Fatalf("records: %d %v", len(records), err)
	}
	cfg.Pools = nil
	for _, r := range records {
		cfg.Pools = append(cfg.Pools, config.Pool{Label: r.Pool, Cores: r.Reservation.Cores, MemoryMB: r.Reservation.MemoryMB})
	}
	var out bytes.Buffer
	if usageReport(cfg, &out, io.Discard) != 0 {
		t.Fatal(out.String())
	}
	lines := strings.Split(out.String(), "\n")
	withheld := 0
	for _, r := range records {
		line := ""
		for _, l := range lines {
			if strings.Contains(l, r.StartedAt.Local().Format("2006-01-02T15:04:05")) {
				line = l
			}
		}
		guest := r.Provider == config.ProviderTart
		cores, memory := fmt.Sprintf("%.2f cores", r.Usage.PeakCores), fmt.Sprintf("%.1f MB", r.Usage.PeakMemoryMB)
		overCores := guest && r.Usage.PeakCores > float64(r.Reservation.Cores)
		overMemory := guest && r.Usage.PeakMemoryMB > float64(r.Reservation.MemoryMB)
		if strings.Contains(line, cores) == overCores || strings.Contains(line, "cores unavailable") != overCores ||
			strings.Contains(line, memory) == overMemory || strings.Contains(line, "memory unavailable") != overMemory {
			t.Fatalf("%s record %+v\nread: %q", r.Provider, *r.Usage, line)
		}
		if overCores || overMemory {
			withheld++
		}
	}
	if withheld == 0 || withheld == len(records) {
		t.Fatalf("the records must hold readings above and within their size: %d of %d above", withheld, len(records))
	}
	t.Logf("$ runner-up usage\n%s", out.String())
}
