//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/runner-up/internal/lock"
)

// readMemory reads MemAvailable, the kernel's own estimate of the memory
// new work can take without swapping, and the resident memory of the QEMU
// of each KVM place running under stateDir. Other places are not read.
func readMemory(_ context.Context, stateDir string) (Memory, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	kb, err := procKB(string(b), "MemAvailable")
	if err != nil {
		return Memory{}, err
	}
	return Memory{AvailableMB: kb / 1024, PlacesMB: kvmPlacesMB(stateDir)}, nil
}

// kvmPlacesMB reads the resident memory of each running KVM place's QEMU,
// by place id. A place counts only while its agent holds the place's lock
// and its qemu.pid names a QEMU started for the place's folder, since the
// pid may be reused.
func kvmPlacesMB(stateDir string) map[string]int {
	entries, err := os.ReadDir(filepath.Join(stateDir, "places"))
	if err != nil {
		return nil
	}
	out := map[string]int{}
	for _, e := range entries {
		a := &AgentRun{StateDir: stateDir, Place: e.Name()}
		if held, err := lock.HeldBy(filepath.Join(a.Dir(), "agent.lock")); err != nil || !held {
			continue
		}
		dir := kvmProvider{}.vmDir(a)
		b, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !bytes.Contains(cmdline, []byte(dir)) {
			continue
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		if kb, err := procKB(string(status), "VmRSS"); err == nil {
			out[e.Name()] = kb / 1024
		}
	}
	return out
}
