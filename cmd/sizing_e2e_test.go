//go:build !windows

package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/testbin"
)

// Intent (INF-2112 / RUU-28): a size selected for a pool reaches a real host
// agent and the job environment, its actual readings survive retirement and
// a coordinator restart, and `usage` shows readings beside the configured
// size. Only GitHub's runner is a stand-in; no sign-in or real fleet is used.
func TestSizedHostJobReportsItsUse(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the place runner and host agent as programs")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the isolated workload")
	}
	dir := t.TempDir()
	// This deliberately bounded workload, rather than a sleep, exercises the
	// actual CPU/RSS reader over more than one sampling interval.
	script := `#!/bin/sh
set -eu
printf '%s %s\n' "$RUNNER_UP_CORES" "$RUNNER_UP_MEMORY_MB" > "$SIZE_PROOF/env"
echo 'Running job: sized host check'
"$SIZE_PYTHON" - <<'PY'
import time
memory=bytearray(16*1024*1024)
start=time.monotonic()
while time.monotonic()-start < 3:
    memory[0]=(memory[0]+1)%256
PY
echo 'Job sized host check completed with result: Succeeded'
`
	archive := filepath.Join(dir, "runner.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0755, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	io.WriteString(tw, script)
	tw.Close()
	gz.Close()
	f.Close()
	binary := testbin.Build(t, "-X github.com/kunchenguid/runner-up/internal/agent.runnerArchive="+archive)
	cfg := initialSetup(t)
	cfg.StateDir = filepath.Join(dir, "coordinator")
	cfg.Pools = []config.Pool{{Label: "mac-check", Cores: 2, MemoryMB: 4096}}
	cfg.Machines = []config.Machine{{Name: "mac", Agent: binary, Cores: 14, MemoryMB: 49152, Places: []config.Place{
		{ID: "host-1", Provider: config.ProviderHost, Pools: []string{"mac-check"}, Cores: 6, MemoryMB: 16384},
	}}}
	cfg.Defaults()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.PlaceDir("host-1"), 0700)
	busy, err := lock.Busy(filepath.Join(cfg.PlaceDir("host-1"), "busy.lock"))
	if err != nil || busy == nil {
		t.Fatalf("lock: %v", err)
	}
	run := coordinator.PlaceRun{Pool: "mac-check", RunnerName: "isolated-host-runner", LaunchedAt: time.Now(), Reservation: cfg.Pools[0].Size()}
	if err := config.WriteJSON(filepath.Join(cfg.PlaceDir("host-1"), "place.json"), run); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run-place", "--config", path, "--place", "host-1", "--pool", "mac-check")
	cmd.ExtraFiles = []*os.File{busy}
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_STATE_HOME="+filepath.Join(dir, "agent-state"), "SIZE_PROOF="+dir, "SIZE_PYTHON="+python,
		"RUNNER_UP_CORES=999", "RUNNER_UP_MEMORY_MB=999")
	cmd.Stdin = strings.NewReader("stand-in-registration\n")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	busy.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("place runner: %v\n%s", err, output.String())
	}
	env, err := os.ReadFile(filepath.Join(dir, "env"))
	if err != nil || string(env) != fmt.Sprintf("%d %d\n", cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB) {
		t.Fatalf("job size: %q %v", env, err)
	}
	svc, err := coordinator.NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.Tidy(context.Background())
	records, err := coordinator.ReadUsage(cfg)
	if err != nil || len(records) != 1 {
		t.Fatalf("retained readings: %+v %v\n%s", records, err, output.String())
	}
	r := records[0]
	if r.Pool != "mac-check" || r.Reservation != cfg.Pools[0].Size() || r.Usage == nil || r.Usage.Samples < 2 {
		t.Fatalf("reading: %+v\n%s", r, output.String())
	}
	// Tidy again after restart cannot count a completed job twice.
	restarted, err := coordinator.NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Tidy(context.Background())
	records, _ = coordinator.ReadUsage(cfg)
	if len(records) != 1 {
		t.Fatalf("restart duplicated readings: %v", records)
	}
	result, err := exec.Command(binary, "usage", "--config", path).CombinedOutput()
	if err != nil || !strings.Contains(string(result), fmt.Sprintf("configured %d cores, %d MB", cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB)) || !strings.Contains(string(result), "process-group") || strings.Contains(string(result), "unavailable") {
		t.Fatalf("usage: %v\n%s", err, result)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	t.Logf("job environment: %s\nretained state: %s\n$ runner-up usage\n%s", env, b, result)
}
