package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// Memory is a machine's free memory as its agent reads it, for the floor its
// owner keeps free (config.Machine.MinAvailableMemoryMB). It is a reading,
// not a size: the coordinator only holds places back by it.
type Memory struct {
	// AvailableMB is the memory the machine can give new work now without
	// swapping: MemAvailable on Linux; on macOS, its memory less what is in
	// use as Activity Monitor counts it (app memory, wired and compressed).
	AvailableMB int `json:"availableMB"`
	// PlacesMB is the memory each place running here uses now, by place id,
	// for the places the agent can read: a KVM place's QEMU, by its resident
	// memory. A running place missing here is read as using nothing, so
	// none of its share counts as free.
	PlacesMB map[string]int `json:"placesMB,omitempty"`
}

// procKB reads field name, in kB, from text in the form of /proc/meminfo or
// /proc/PID/status, such as "MemAvailable:   3174400 kB".
func procKB(text, name string) (int, error) {
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			break
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 0 {
			break
		}
		return n, nil
	}
	return 0, fmt.Errorf("no %s reading", name)
}
