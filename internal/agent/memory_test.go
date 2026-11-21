package agent

import "testing"

// Intent (README, "Setup file", minAvailableMemoryMB): the floor is checked
// against the kernel's own estimate of the memory new work can take,
// MemAvailable, not MemFree, which leaves out the file cache Linux gives
// back. A reading the agent cannot make is an error, never zero free.
func TestMeminfoReadsMemAvailable(t *testing.T) {
	// /proc/meminfo of a 32 GB workstation whose owner's use left about
	// 2 GB available and much of its swap taken, as in the issue.
	meminfo := `MemTotal:       32599452 kB
MemFree:          412236 kB
MemAvailable:    2104520 kB
Buffers:           61312 kB
Cached:          2318408 kB
SwapCached:       843120 kB
SwapTotal:      33554428 kB
SwapFree:       10102300 kB
`
	kb, err := procKB(meminfo, "MemAvailable")
	if err != nil || kb != 2104520 {
		t.Fatalf("MemAvailable read %d kB, %v; want 2104520 kB", kb, err)
	}
	for _, bad := range []string{
		"MemTotal:       32599452 kB\nMemFree:          412236 kB\n",
		"MemAvailable:    lots kB\n",
		"MemAvailable:    2104520 pages\n",
		"MemAvailable:    -1 kB\n",
	} {
		if kb, err := procKB(bad, "MemAvailable"); err == nil {
			t.Errorf("read %d kB from %q, want no reading", kb, bad)
		}
	}
	// A process's resident memory, as /proc/PID/status gives it.
	if kb, err := procKB("Name:\tqemu-system-x86\nVmRSS:\t 4194304 kB\n", "VmRSS"); err != nil || kb != 4194304 {
		t.Fatalf("VmRSS read %d kB, %v", kb, err)
	}
}
