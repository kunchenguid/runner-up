package testrun

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Intent (the RUU-54 test audit, fix 3): a command or wait in a test that
// never ends fails that test, naming what it waited for, instead of holding
// go test until its 10-minute timeout and losing the cause; and nothing a
// test's command started, its children included, outlives the test.

const hangEnv = "TESTRUN_HANG"

// TestMain lets the test binary stand in for a program that hangs: with
// hangEnv set to "parent" it starts a child copy of itself that hangs too,
// prints the child's process ID, and never exits.
func TestMain(m *testing.M) {
	if role, ok := os.LookupEnv(hangEnv); ok {
		if role == "parent" {
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), hangEnv+"=child")
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			fmt.Println(child.Process.Pid)
		}
		time.Sleep(time.Hour)
	}
	os.Exit(m.Run())
}

// recorder is a test whose deadline comes soon and whose failures are kept
// rather than reported.
type recorder struct {
	testing.TB
	end time.Time

	mu       sync.Mutex
	failures []string
	cleanups []func()
}

func newRecorder(within time.Duration) *recorder {
	return &recorder{end: time.Now().Add(Grace + within)}
}

func (r *recorder) Deadline() (time.Time, bool) { return r.end, true }
func (r *recorder) Helper()                     {}
func (r *recorder) Cleanup(f func())            { r.mu.Lock(); r.cleanups = append(r.cleanups, f); r.mu.Unlock() }
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// run runs body as the recorder's test and then its cleanups, as go test
// does, and returns its failures.
func (r *recorder) run(body func()) []string {
	done := make(chan struct{})
	go func() { defer close(done); body() }()
	<-done
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}

// hang starts the hanging stand-in under r and returns it with its child's
// process ID.
func hang(t *testing.T, r *recorder) (*exec.Cmd, int) {
	t.Helper()
	c := Command(r, os.Args[0])
	c.Env = append(os.Environ(), hangEnv+"=parent")
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	var child int
	if _, err := fmt.Fscanln(out, &child); err != nil {
		t.Fatalf("the stand-in never named its child: %v", err)
	}
	return c, child
}

func TestACommandThatHangsFailsItsTestAndEndsWithItsChildren(t *testing.T) {
	r := newRecorder(3 * time.Second)
	var waited error
	var child int
	start := time.Now()
	failures := r.run(func() {
		var cmd *exec.Cmd
		cmd, child = hang(t, r)
		waited = cmd.Wait()
	})
	took := time.Since(start)
	if len(failures) != 1 || !strings.Contains(failures[0], os.Args[0]) || !strings.Contains(failures[0], "deadline") {
		t.Errorf("the test failed with %q, want one failure naming the command and its deadline", failures)
	}
	if waited == nil {
		t.Error("waiting for the hung command succeeded")
	}
	if took > Limit {
		t.Errorf("the hung command held its test %s", took)
	}
	childEnds(t, child)
}

func TestACommandStillRunningWhenItsTestEndsIsEndedWithItsChildren(t *testing.T) {
	r := newRecorder(Limit)
	var cmd *exec.Cmd
	var child int
	failures := r.run(func() { cmd, child = hang(t, r) })
	if len(failures) != 0 {
		t.Errorf("a test that ended before its command's deadline failed with %q", failures)
	}
	if err := Receive(t, wait(cmd), "the command to end with its test"); err == nil {
		t.Error("the command was still running when its test ended, and later exited 0")
	}
	childEnds(t, child)
}

// wait waits for cmd in the background.
func wait(cmd *exec.Cmd) chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

func TestAWaitThatNeverEndsFailsItsTestNamingWhatItWaitedFor(t *testing.T) {
	r := newRecorder(100 * time.Millisecond)
	reached := false
	failures := r.run(func() {
		Receive(r, make(chan int), "the stand-in's answer")
		reached = true
	})
	if len(failures) != 1 || !strings.Contains(failures[0], "the stand-in's answer") {
		t.Errorf("the test failed with %q, want one failure naming the wait", failures)
	}
	if reached {
		t.Error("the test went on past a wait that never ended")
	}

	r = newRecorder(100 * time.Millisecond)
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()
	failures = r.run(func() { Wait(r, "the jobs to stop", wg.Wait) })
	if len(failures) != 1 || !strings.Contains(failures[0], "the jobs to stop") {
		t.Errorf("the test failed with %q, want one failure naming the wait", failures)
	}
}
