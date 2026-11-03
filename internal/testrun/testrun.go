// Package testrun gives every command and wait in runner-up's tests a
// deadline, so a hang fails the test that hung with what it was waiting for
// instead of holding go test until its own timeout, and ends what each
// command started when its test ends.
package testrun

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Limit bounds any one command or wait. The slowest whole test takes about
// 20 seconds, builds included, so only a hang reaches it.
const Limit = 3 * time.Minute

// Grace is how long before go test's own timeout a command or wait gives up,
// leaving time to report it and end what the test started.
const Grace = 10 * time.Second

// deadline is how long a command or wait in t may take: Limit, or less when
// go test's timeout comes sooner.
func deadline(t testing.TB) time.Duration {
	d := Limit
	if dt, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if end, ok := dt.Deadline(); ok {
			d = min(d, time.Until(end)-Grace)
		}
	}
	return max(d, 0)
}

// Command is exec.Command for a test: the program runs in its own process
// group, and when it outlives its deadline the test fails naming it and the
// group is killed. When the test ends, the group is killed too, so nothing a
// test started outlives it unless it left the group on purpose. On Windows,
// which has no process groups, the program's process tree is killed instead
// while the program runs.
func Command(t testing.TB, name string, args ...string) *exec.Cmd {
	t.Helper()
	d := deadline(t)
	ctx, cancel := context.WithTimeout(context.Background(), d)
	cmd := exec.CommandContext(ctx, name, args...)
	ownGroup(cmd)
	var mu sync.Mutex
	ended := false
	cmd.Cancel = func() error {
		mu.Lock()
		if !ended && ctx.Err() == context.DeadlineExceeded {
			t.Errorf("%s ran past its %s deadline, so the test ended it", strings.Join(cmd.Args, " "), d)
		}
		mu.Unlock()
		killGroup(cmd.Process)
		return cmd.Process.Kill()
	}
	// A process the program left behind may hold its output open; Wait
	// returns this long after the program itself ends.
	cmd.WaitDelay = Grace
	t.Cleanup(func() {
		mu.Lock()
		ended = true
		mu.Unlock()
		cancel()
		if cmd.Process != nil {
			endLeftovers(cmd.Process)
		}
	})
	return cmd
}

// Receive returns the next value on ch. If none comes within the deadline the
// test fails, saying it waited for what.
func Receive[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	d := deadline(t)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("waited %s for %s", d, what)
		var zero T
		return zero
	}
}

// Wait calls wait, such as a WaitGroup's Wait. If it has not returned within
// the deadline the test fails, saying it waited for what.
func Wait(t testing.TB, what string, wait func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	Receive(t, done, what)
}
