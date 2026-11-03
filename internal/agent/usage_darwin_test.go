package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Intent (RUU-34 / DOP-252): a tart guest's reading stays within the guest's
// own CPUs and memory, even when its readings return late as they do in a busy
// guest. Adding up processes, a ps returning 1.9 s late on every second reading
// read 70-93 cores on a 14-CPU Mac, and summed RSS read 41-97 GB in 10-16 GB
// guests. This reads the real machine with macOS's own tools, as a guest does,
// while a child keeps one core and 32 MiB busy; every second tool returns 1.9 s
// after it read.
func TestGuestReadingStaysWithinTheGuest(t *testing.T) {
	cpus, memoryMB, err := guestSize(context.Background(), guestTool)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageWorkload$")
	cmd.Env = append(os.Environ(), "RUNNER_UP_USAGE_WORKLOAD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	reader := bufio.NewReader(out)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("workload: %q %v", line, err)
	}
	calls := 0
	late := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		b, err := guestTool(ctx, name, args...)
		if calls++; calls%2 == 0 {
			select {
			case <-time.After(1900 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		return b, err
	}
	read := guestReaderWith(late)
	sampled := make(chan struct{}, 8)
	finish := measureWith("guest", func(ctx context.Context) (sample, error) {
		s, err := read(ctx)
		if err == nil {
			sampled <- struct{}{}
		}
		return s, err
	})
	for range 4 {
		select {
		case <-sampled:
		case <-time.After(30 * time.Second):
			t.Fatal("guest readings did not arrive")
		}
	}
	u := finish()
	in.Close()
	io.Copy(io.Discard, reader)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if u.Scope != "guest" || u.Samples < 4 || u.Incomplete || u.PeakCores < 0.5 || u.PeakCores > float64(cpus) || u.PeakMemoryMB < 32 || u.PeakMemoryMB > memoryMB {
		t.Fatalf("readings of a %d-CPU, %.0f MB machine: %+v", cpus, memoryMB, u)
	}
	var line strings.Builder
	writeUsage(&line, u)
	parsed, ok := ParseUsage(strings.TrimSpace(strings.TrimPrefix(line.String(), UsagePrefix)))
	if !ok || parsed != u {
		t.Fatalf("round-trip readings: %s", line.String())
	}
	b, _ := json.Marshal(u)
	t.Logf("readings of a %d-CPU, %.0f MB machine with late tools: %s", cpus, memoryMB, b)
}
