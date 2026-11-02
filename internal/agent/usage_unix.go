//go:build !windows

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type processUse struct {
	pid, group int
	cpu, rss   float64
}

// readProcessUse reads numbers only, never another process's arguments or
// environment. ps is available on the host and the macOS guest image.
func readProcessUse(ctx context.Context) ([]processUse, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(c, "ps", "-axo", "pid=,pgid=,time=,rss=").Output()
	if err != nil {
		return nil, err
	}
	var out []processUse
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		cpu, e3 := cpuSeconds(fields[2])
		rss, e4 := strconv.ParseFloat(fields[3], 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			return nil, fmt.Errorf("unreadable process usage")
		}
		out = append(out, processUse{pid, group, cpu, rss / 1024})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no process readings")
	}
	return out, nil
}

func cpuSeconds(s string) (float64, error) {
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, err
		}
		days = n
		s = rest
	}
	parts := strings.Split(s, ":")
	var seconds float64
	for _, part := range parts {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, err
		}
		seconds = seconds*60 + n
	}
	return float64(days)*86400 + seconds, nil
}

// measureGroup samples one host runner's process group, or the whole guest
// when group is zero. It stops before the caller releases the place. A
// process that exits between samples, or escapes the host group, can be
// missed: the report deliberately calls these sampled peaks.
func measureGroup(group int) func() Usage {
	return measureGroupWith(group, readProcessUse)
}

func measureGroupWith(group int, read func(context.Context) ([]processUse, error)) func() Usage {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Usage, 1)
	go func() {
		start := time.Now()
		last := start
		scope := "process-group"
		if group == 0 {
			scope = "guest-processes"
		}
		u := Usage{Scope: scope}
		previous := map[int]float64{}
		sample := func() {
			rows, err := read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					u.Incomplete = true
				}
				return
			}
			now := time.Now()
			elapsed := now.Sub(last).Seconds()
			last = now
			current := map[int]float64{}
			cpu, rss := 0.0, 0.0
			for _, p := range rows {
				if group != 0 && p.group != group {
					continue
				}
				current[p.pid] = p.cpu
				rss += p.rss
				if old, ok := previous[p.pid]; ok && p.cpu >= old {
					cpu += p.cpu - old
				}
			}
			if u.Samples > 0 && elapsed > 0 {
				u.PeakCores = max(u.PeakCores, cpu/elapsed)
			}
			u.PeakMemoryMB = max(u.PeakMemoryMB, rss)
			u.Samples++
			previous = current
		}
		sample()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				sample()
			case <-ctx.Done():
				u.Seconds = time.Since(start).Seconds()
				if u.Samples < 2 {
					u.Incomplete = true
				}
				done <- u
				return
			}
		}
	}()
	return func() Usage { cancel(); return <-done }
}

func writeUsage(w io.Writer, u Usage) {
	if u.Samples == 0 {
		fmt.Fprintln(w, "runner-up agent: resource readings unavailable")
		return
	}
	b, err := json.Marshal(u)
	if err == nil {
		fmt.Fprintln(w, UsagePrefix+string(b))
	}
}

// MeasureMain runs inside a disposable tart guest. The guest's other
// processes (including simulators started by launchd) belong to this job's
// VM too. The wrapper takes the registration on stdin and gives only the
// runner its required input variable; the wrapper's environment never holds it.
func MeasureMain(args []string) int {
	fs := flag.NewFlagSet("agent measure", flag.ContinueOnError)
	if fs.Parse(args) != nil || len(fs.Args()) == 0 {
		return 2
	}
	input := bufio.NewReader(os.Stdin)
	line, _ := input.ReadString('\n')
	registration := strings.TrimSpace(line)
	if registration == "" {
		fmt.Fprintln(os.Stderr, "no runner registration on stdin")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cmd := exec.Command(fs.Args()[0], fs.Args()[1:]...)
	cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+registration)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	finish := measureGroup(0)
	err := waitOrStop(ctx, cmd, func() { cmd.Process.Signal(syscall.SIGTERM) }, 30*time.Second)
	writeUsage(os.Stdout, finish())
	code := ExitCode(err)
	if code < 0 {
		return 1
	}
	return code
}
