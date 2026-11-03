//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent: on a shared 14-core budget, last:ru-linux lets a newer 12-core
// native-pool job start before older 2-core Linux jobs, leaving room for only
// one Linux job. The real coordinator, place runners, agent and scale set SDK
// run against the local Actions/SSH/Docker boundaries from the E2E harness.
// The independent oracle is the approved 12 + 2 = 14 worked example, not the
// planner's own ordering function. The control rejects an ineffective last rule.
func TestLastPoolSharedBudgetThroughTheRunningCoordinator(t *testing.T) {
	for _, rankLast := range []bool{false, true} {
		name := "fifo-control"
		if rankLast {
			name = "linux-ranked-last"
		}
		t.Run(name, func(t *testing.T) {
			e := newE2E(t)
			e.cfg.Pools = []config.Pool{{Label: "ru-linux", Cores: 2, MemoryMB: 8192}, {Label: "ru-native", Cores: 12, MemoryMB: 32768}}
			e.cfg.Order = []string{"waited:60m", "shortest"}
			if rankLast {
				e.cfg.Order = append([]string{"last:ru-linux"}, e.cfg.Order...)
			}
			m := &e.cfg.Machines[0]
			m.Cores, m.RoomCores = 22, 8
			m.MemoryMB, m.RoomMemoryMB = 131072, 8192
			// Docker is the external runner emulator here. This scenario proves
			// coordinator scheduling, not Apple's virtualization implementation.
			m.Places = []config.Place{
				{ID: "native", Provider: config.ProviderDocker, Pools: []string{"ru-native"}, Cores: 12, MemoryMB: 32768},
				{ID: "linux-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 8192},
				{ID: "linux-2", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 8192},
			}
			e.save()
			var held []*os.File
			for _, p := range e.cfg.Places() {
				os.MkdirAll(e.cfg.PlaceDir(p.ID), 0o700)
				f, err := lock.Busy(filepath.Join(e.cfg.PlaceDir(p.ID), "busy.lock"))
				if err != nil || f == nil {
					t.Fatalf("hold place %s: %v", p.ID, err)
				}
				held = append(held, f)
				t.Cleanup(func() { f.Close() })
			}
			old := verifyJob(t)
			old.QueueTime = time.Now().Add(-2 * time.Hour).UTC()
			second := old
			second.RunnerRequestID++
			second.JobID = "synthetic-linux-2"
			second.QueueTime = old.QueueTime.Add(time.Minute)
			newer := old
			newer.RunnerRequestID += 10
			newer.JobID = "synthetic-native"
			newer.QueueTime = time.Now().Add(-time.Minute).UTC()
			e.actions.send(jobMessage(old, scaleset.MessageTypeJobAvailable), jobMessage(second, scaleset.MessageTypeJobAvailable))
			e.actions.sendTo("ru-native", jobMessage(newer, scaleset.MessageTypeJobAvailable))
			e.serve()
			e.eventually("all three synthetic jobs are waiting", func() bool {
				var st struct {
					Waiting []any `json:"waiting"`
				}
				return config.ReadJSON(e.cfg.StateDir+"/state.json", &st) == nil && len(st.Waiting) == 3
			})
			for _, f := range held {
				f.Close()
			}
			e.eventually("exactly two runners are launched", func() bool {
				return len(e.registrations()) == 2
			})
			// Let later ticks attempt to overcommit or bypass the reservation.
			time.Sleep(2200 * time.Millisecond)
			wantNative := rankLast
			var native coordinator.PlaceRun
			gotNative := config.ReadJSON(filepath.Join(e.cfg.PlaceDir("native"), "place.json"), &native) == nil
			if gotNative != wantNative || len(e.registrations()) != 2 {
				t.Fatalf("native running = %v, want %v; registrations = %d\n%s", gotNative, wantNative, len(e.registrations()), e.logs())
			}
			cores, linux := 0, 0
			for _, p := range e.cfg.Places() {
				var run coordinator.PlaceRun
				if config.ReadJSON(filepath.Join(e.cfg.PlaceDir(p.ID), "place.json"), &run) == nil {
					cores += p.Cores
					if strings.HasPrefix(p.ID, "linux-") {
						linux++
					}
				}
			}
			if rankLast && (cores != 14 || linux != 1) || !rankLast && (cores != 4 || linux != 2) {
				t.Fatalf("running cores=%d linux=%d", cores, linux)
			}
			cmd := exec.Command(runnerUp(t), "status", "--config", e.config)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			t.Logf("real CLI status (%s):\n%s\ncoordinator and agent transcript:\n%s", name, out, e.logs())
			e.noSecrets(e.registrations())
			if code := e.stop(); code != 0 {
				t.Fatalf("coordinator stop: %d", code)
			}
		})
	}
}
