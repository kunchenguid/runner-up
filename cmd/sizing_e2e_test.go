//go:build !windows

package cmd

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/testbin"
)

// Intent (README, "Job sizes and readings"): a size selected for a pool reaches
// a real host agent and the job environment, its actual readings survive
// retirement and a coordinator restart, and `usage` shows readings beside the
// configured size. The real coordinator picks the size and writes it into
// place.json, so a wrong pick fails here: the place's own size differs from
// the pool's. Only GitHub's service and runner are stand-ins; no sign-in or
// real fleet is used.
func TestSizedHostJobReportsItsUse(t *testing.T) {
	e := newE2E(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the isolated workload")
	}
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
	archive := filepath.Join(e.dir, "runner.tar.gz")
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
	e.cfg.Pools = []config.Pool{{Label: "ru-linux", Cores: 2, MemoryMB: 4096}}
	e.cfg.Machines = []config.Machine{{Name: "mac", SSH: []string{"ci@mac"}, Agent: binary, Cores: 14, MemoryMB: 49152, Places: []config.Place{
		{ID: "host-1", Provider: config.ProviderHost, Pools: []string{"ru-linux"}, Cores: 6, MemoryMB: 16384},
	}}}
	e.save()
	pool, place := e.cfg.Pools[0], e.cfg.Places()[0]
	if pool.Size() == (config.Share{Cores: place.Cores, MemoryMB: place.MemoryMB}) {
		t.Fatal("the pool's size must differ from its place's for this test to tell them apart")
	}
	// The agent's own environment says 999: the job must see the pool's
	// size, which the agent sets, not what it inherited.
	e.env = append(e.env, "HOME="+e.dir, "SIZE_PROOF="+e.dir, "SIZE_PYTHON="+python,
		"RUNNER_UP_CORES=999", "RUNNER_UP_MEMORY_MB=999")
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.serve()

	e.eventually("the coordinator keeps the finished job's readings", func() bool {
		records, err := coordinator.ReadUsage(e.cfg)
		return err == nil && len(records) == 1
	})
	env, err := os.ReadFile(filepath.Join(e.dir, "env"))
	if err != nil || string(env) != fmt.Sprintf("%d %d\n", pool.Cores, pool.MemoryMB) {
		t.Fatalf("job size: %q %v\n%s", env, err, e.logs())
	}
	records, _ := coordinator.ReadUsage(e.cfg)
	r := records[0]
	if r.Pool != "ru-linux" || r.Place != "host-1" || r.Reservation != pool.Size() || r.Usage == nil || r.Usage.Samples < 2 {
		t.Fatalf("reading: %+v\n%s", r, e.logs())
	}

	// A coordinator restart cannot count the completed job twice.
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	e.serve()
	e.eventually("the restarted coordinator polls its new session twice", func() bool {
		seen := e.actions.read()
		return seen.opened == 2 && seen.polls >= 2
	})
	if records, _ := coordinator.ReadUsage(e.cfg); len(records) != 1 {
		t.Fatalf("restart duplicated readings: %+v", records)
	}
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	result, err := exec.Command(runnerUp(t), "usage", "--config", e.config).CombinedOutput()
	if err != nil || !strings.Contains(string(result), fmt.Sprintf("configured %d cores, %d MB", pool.Cores, pool.MemoryMB)) || !strings.Contains(string(result), "process-group") || strings.Contains(string(result), "unavailable") {
		t.Fatalf("usage: %v\n%s", err, result)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	t.Logf("job environment: %s\nretained state: %s\n$ runner-up usage\n%s", env, b, result)
}
