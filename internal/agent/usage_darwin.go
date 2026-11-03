package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// guestReader reads the whole macOS guest from its kernel's own counters
// rather than adding up its processes. Adding them up reads a guest
// above its size: one ps that returned late shrank the time the CPU it read
// was divided by, and pages shared by many processes counted once per process.
//
// CPU is the busy share of all the guest's CPUs over one second, which iostat
// works out from the kernel's CPU ticks, so a reading that returns late cannot
// raise it. Memory is what the guest has in use, as Activity Monitor counts
// it, so a shared page counts once. Neither can exceed the guest's own CPUs
// and memory; a reading that would is refused. The tools are macOS's own, in
// the guest image at fixed paths.
func guestReader() func(context.Context) (sample, error) { return guestReaderWith(guestTool) }

// tool runs one of macOS's reading tools and returns its output.
type tool func(ctx context.Context, name string, args ...string) ([]byte, error)

func guestReaderWith(run tool) func(context.Context) (sample, error) {
	cpus, memoryMB := 0, 0.0
	return func(ctx context.Context) (sample, error) {
		if cpus == 0 {
			c, m, err := guestSize(ctx, run)
			if err != nil {
				return sample{}, err
			}
			cpus, memoryMB = c, m
		}
		busy, err := guestBusy(ctx, run)
		if err != nil {
			return sample{}, err
		}
		used, err := guestMemoryMB(ctx, run)
		if err != nil {
			return sample{}, err
		}
		if used > memoryMB {
			return sample{}, fmt.Errorf("guest memory reading above the guest's %.0f MB", memoryMB)
		}
		return sample{cores: busy * float64(cpus), hasCores: true, memoryMB: used}, nil
	}
}

// guestSize reads the guest's logical CPUs and memory. tart sets both to the
// job's reservation.
func guestSize(ctx context.Context, run tool) (int, float64, error) {
	b, err := run(ctx, "/usr/sbin/sysctl", "-n", "hw.logicalcpu", "hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unreadable guest size")
	}
	cpus, e1 := strconv.Atoi(fields[0])
	memory, e2 := strconv.ParseFloat(fields[1], 64)
	if e1 != nil || e2 != nil || cpus <= 0 || memory <= 0 {
		return 0, 0, fmt.Errorf("unreadable guest size")
	}
	return cpus, memory / (1 << 20), nil
}

// guestBusy returns the share of the guest's CPU time that was not idle over
// one second. iostat's first line covers the time since boot; its second, the
// interval.
func guestBusy(ctx context.Context, run tool) (float64, error) {
	b, err := run(ctx, "/usr/sbin/iostat", "-n", "0", "-c", "2", "-w", "1")
	if err != nil {
		return 0, err
	}
	var idle []float64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var cpu [3]float64
		numeric := true
		for i := range cpu {
			n, err := strconv.ParseFloat(fields[i], 64)
			numeric = numeric && err == nil
			cpu[i] = n
		}
		if numeric {
			idle = append(idle, cpu[2])
		}
	}
	if len(idle) != 2 || idle[1] < 0 || idle[1] > 100 {
		return 0, fmt.Errorf("unreadable guest CPU reading")
	}
	return (100 - idle[1]) / 100, nil
}

// guestMemoryMB reads the guest's memory in use from vm_stat: app memory
// (anonymous pages less purgeable ones), wired pages and the pages the
// compressor occupies, as Activity Monitor's Memory Used counts them. File
// cache the guest can drop is not in use.
func guestMemoryMB(ctx context.Context, run tool) (float64, error) {
	b, err := run(ctx, "/usr/bin/vm_stat")
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(b), "\n")
	var pageSize float64
	if _, after, ok := strings.Cut(lines[0], "page size of "); ok {
		pageSize, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(after), " bytes)"), 64)
	}
	pages := map[string]float64{}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "."), 64); err == nil {
			pages[strings.TrimSpace(name)] = n
		}
	}
	const anonymous, purgeable, wired, compressor = "Anonymous pages", "Pages purgeable", "Pages wired down", "Pages occupied by compressor"
	for _, name := range []string{anonymous, purgeable, wired, compressor} {
		if _, ok := pages[name]; !ok || pageSize <= 0 {
			return 0, fmt.Errorf("unreadable guest memory reading")
		}
	}
	app := max(0, pages[anonymous]-pages[purgeable])
	return (app + pages[wired] + pages[compressor]) * pageSize / (1 << 20), nil
}

// guestTool runs one of macOS's reading tools. Its readings stay true however
// late it returns, so it may take a while in a busy guest.
func guestTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(c, name, args...).Output()
}
