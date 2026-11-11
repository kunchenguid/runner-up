package agent

import (
	"context"
	"fmt"
)

// readMemory reads the Mac's memory less what it has in use, as Activity
// Monitor counts it, with the same tools a tart guest's readings use
// (usage_darwin.go): file cache the Mac can drop counts as free. Its places
// are not read.
func readMemory(ctx context.Context, _ string) (Memory, error) {
	return readMacMemory(ctx, guestTool)
}

func readMacMemory(ctx context.Context, run tool) (Memory, error) {
	_, total, err := guestSize(ctx, run)
	if err != nil {
		return Memory{}, err
	}
	used, err := guestMemoryMB(ctx, run)
	if err != nil {
		return Memory{}, err
	}
	if used > total {
		return Memory{}, fmt.Errorf("memory in use read above the Mac's %.0f MB", total)
	}
	return Memory{AvailableMB: int(total - used)}, nil
}
