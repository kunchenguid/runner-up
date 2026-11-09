package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (README, "Setup file", minAvailableMemoryMB): what runner-up's own
// running KVM places use counts as free for the floor, so a running job does
// not hold the next one back twice. Only a place its agent holds, whose
// qemu.pid still names a process started for that place's folder, is read:
// a pid left by a killed agent may since belong to anything.
func TestMemoryReadsOnlyRunningKVMPlaces(t *testing.T) {
	state := t.TempDir()
	// start stands in for a place's QEMU: a process whose command line names
	// the place's folder, as QEMU's -pidfile does, and writes its pid there.
	start := func(id string, held bool) {
		a := &AgentRun{StateDir: state, Place: id}
		dir := kvmProvider{}.vmDir(a)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if held {
			f, err := lock.Busy(filepath.Join(a.Dir(), "agent.lock"))
			if err != nil || f == nil {
				t.Fatalf("locking %s: %v", id, err)
			}
			t.Cleanup(func() { f.Close() })
		}
		cmd := testrun.Command(t, "sh", "-c", "read x", filepath.Join(dir, "qemu.pid"))
		if _, err := cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start("box-large", true)
	// Its agent was killed: the place is not running.
	start("box-left", false)
	// Its pid now belongs to this test, which was not started for it.
	stale := &AgentRun{StateDir: state, Place: "box-reused"}
	os.MkdirAll(kvmProvider{}.vmDir(stale), 0o700)
	f, _ := lock.Busy(filepath.Join(stale.Dir(), "agent.lock"))
	t.Cleanup(func() { f.Close() })
	os.WriteFile(filepath.Join(kvmProvider{}.vmDir(stale), "qemu.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)

	got := kvmPlacesMB(state)
	if _, ok := got["box-large"]; !ok || len(got) != 1 {
		t.Fatalf("read places %v, want only box-large", got)
	}
}
