//go:build !windows

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Intent: sizes must be informed by readings of actual
// work, not copied limits. This child uses one CPU and touches 32 MiB until
// the parent has collected real OS readings. No fabricated ps fixture or
// duration-dependent expectation stands in for a measurement.
func TestUsageWorkload(t *testing.T) {
	if os.Getenv("RUNNER_UP_USAGE_WORKLOAD") != "1" {
		return
	}
	memory := make([]byte, 32<<20)
	for i := range memory {
		memory[i] = byte(i)
	}
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
	fmt.Println("ready")
	for {
		select {
		case <-done:
			runtime.KeepAlive(memory)
			return
		default:
			for i := range memory {
				memory[i]++
			}
		}
	}
}

// Intent: the sampler reads this job's group, captures
// nonzero CPU and resident memory, and emits a record the coordinator can
// consume. Stopping readings completes without leaving a sampler behind.
func TestUsageObservesActualJobProcesses(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageWorkload$")
	cmd.Env = append(os.Environ(), "RUNNER_UP_USAGE_WORKLOAD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	sampled := make(chan struct{}, 8)
	finish := measureGroupWith(cmd.Process.Pid, func(ctx context.Context) ([]processUse, error) {
		rows, err := readProcessUse(ctx)
		if err == nil {
			sampled <- struct{}{}
		}
		return rows, err
	})
	for range 3 {
		select {
		case <-sampled:
		case <-time.After(15 * time.Second):
			t.Fatal("actual process readings did not arrive")
		}
	}
	u := finish()
	in.Close()
	io.Copy(io.Discard, reader)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if u.Samples < 2 || u.PeakCores <= 0 || u.PeakMemoryMB < 32 || u.Scope != "process-group" || u.Incomplete {
		t.Fatalf("readings: %+v", u)
	}
	var line strings.Builder
	writeUsage(&line, u)
	parsed, ok := ParseUsage(strings.TrimSpace(strings.TrimPrefix(line.String(), UsagePrefix)))
	if !ok || parsed != u {
		t.Fatalf("round-trip readings: %s", line.String())
	}
	b, _ := json.Marshal(u)
	t.Logf("actual isolated job readings: %s", b)
}

// Intent: absent and too-short readings must not look
// like measured zero use. Exercise the actual reader with ps absent, then
// with one real reading; the barrier models a job ending after that read.
func TestUsageReportsUnavailableAndShortReadings(t *testing.T) {
	group, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{true, false} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			if unavailable {
				t.Setenv("PATH", t.TempDir())
			}
			read := make(chan struct{}, 1)
			finish := measureGroupWith(group, func(ctx context.Context) ([]processUse, error) {
				rows, err := readProcessUse(ctx)
				select {
				case read <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return rows, err
			})
			<-read
			u := finish()
			var output strings.Builder
			writeUsage(&output, u)
			if unavailable {
				if u.Samples != 0 || !strings.Contains(output.String(), "readings unavailable") || strings.Contains(output.String(), UsagePrefix) {
					t.Fatalf("unavailable reading: %+v, %q", u, output.String())
				}
			} else {
				parsed, ok := ParseUsage(strings.TrimSpace(strings.TrimPrefix(output.String(), UsagePrefix)))
				if !ok || parsed.Samples != 1 || !parsed.Incomplete || parsed.PeakMemoryMB <= 0 {
					t.Fatalf("short reading: %+v, %q", u, output.String())
				}
			}
			t.Log(output.String())
		})
	}
}
