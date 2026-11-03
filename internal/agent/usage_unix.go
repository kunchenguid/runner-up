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

	"github.com/kunchenguid/runner-up/internal/cli"
)

type processUse struct {
	pid, group int
	cpu, rss   float64
}

// readProcessUse reads numbers only, never another process's arguments or
// environment. ps is available on macOS and Linux hosts.
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

// sample is one reading: the memory in use now and, once the reader can
// tell, the cores used since its previous reading.
type sample struct {
	cores    float64
	hasCores bool
	memoryMB float64
}

// measureGroup samples one host runner's process group. It stops before the
// caller releases the place. A process that exits between samples, or escapes
// the group, can be missed: the report deliberately calls these sampled peaks.
func measureGroup(group int) func() Usage {
	return measureGroupWith(group, readProcessUse)
}

func measureGroupWith(group int, read func(context.Context) ([]processUse, error)) func() Usage {
	return measureWith("process-group", groupReader(group, read))
}

// groupReader adds up the group's processes: the CPU time they used since the
// previous reading over the time between the two, and their RSS.
func groupReader(group int, read func(context.Context) ([]processUse, error)) func(context.Context) (sample, error) {
	var previous map[int]float64
	var last time.Time
	return func(ctx context.Context) (sample, error) {
		rows, err := read(ctx)
		if err != nil {
			return sample{}, err
		}
		now := time.Now()
		current := map[int]float64{}
		var s sample
		cpu := 0.0
		for _, p := range rows {
			if p.group != group {
				continue
			}
			current[p.pid] = p.cpu
			s.memoryMB += p.rss
			if old, ok := previous[p.pid]; ok && p.cpu >= old {
				cpu += p.cpu - old
			}
		}
		if elapsed := now.Sub(last).Seconds(); previous != nil && elapsed > 0 {
			s.cores, s.hasCores = cpu/elapsed, true
		}
		previous, last = current, now
		return s, nil
	}
}

// measureGuest samples the whole macOS guest a tart job runs in, until the
// returned function stops it.
func measureGuest() func() Usage {
	return measureWith("guest", guestReader())
}

// measureWith keeps the peaks of read's samples, taken once a second until
// the returned function stops it.
func measureWith(scope string, read func(context.Context) (sample, error)) func() Usage {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Usage, 1)
	go func() {
		start := time.Now()
		u := Usage{Scope: scope}
		take := func() {
			s, err := read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					u.Incomplete = true
				}
				return
			}
			if s.hasCores {
				u.PeakCores = max(u.PeakCores, s.cores)
			}
			u.PeakMemoryMB = max(u.PeakMemoryMB, s.memoryMB)
			u.Samples++
		}
		take()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				take()
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

// MeasureMain runs inside a disposable tart guest. The whole guest, including
// simulators started by launchd, belongs to this job, so it reads the guest
// rather than the runner's processes. The wrapper takes the registration on
// stdin and gives only the runner its required input variable; the wrapper's
// environment never holds it.
func MeasureMain(args []string) int {
	fs := flag.NewFlagSet("agent measure", flag.ContinueOnError)
	if code, stop := cli.Parse(fs, args, true); stop {
		return code
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: runner-up agent measure -- COMMAND")
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
	cmd.Env = append(os.Environ(), registrationVar+"="+registration)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	finish := measureGuest()
	err := waitOrStop(ctx, cmd, func() { cmd.Process.Signal(syscall.SIGTERM) }, 30*time.Second)
	writeUsage(os.Stdout, finish())
	code := ExitCode(err)
	if code < 0 {
		return 1
	}
	return code
}
