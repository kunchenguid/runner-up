package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/testrun"
	"golang.org/x/sys/windows"
)

// Intent (a bug found on a real PC): a Windows account job ran to its end,
// but the agent heard nothing from runner-up's service - not even its
// "running as" line - until the agent left, so the coordinator never saw the
// job reach the runner and held the place until its start timeout. The
// service's pipe did synchronous I/O, so every write waited behind the read
// that watches for the agent leaving. Over a real pipe served by the
// service's own listener, the agent hears "running as" and the job's output
// while the job runs and the agent stays connected, the service still stops
// the job and deletes its account when the agent leaves, and a job that ends
// on its own ends the agent's request with the start script's exit code.
func TestTheAgentHearsItsJobWhileConnectedAndTheServiceHearsItLeave(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := user.User.Sid
	// A pipe like the service's, owned by this test's account and open to it
	// alone, so the agent's owner check holds without SYSTEM.
	path := fmt.Sprintf(`\\.\pipe\runner-up-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	sa, err := securityAttributes(fmt.Sprintf("O:%sD:P(A;;GA;;;%s)", me, me))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listenAt(path, sa)
	if err != nil {
		t.Fatal(err)
	}
	h := newFakeHost()
	h.block = true
	s := newTestServer(t, h)
	ctx, stopService := context.WithCancel(context.Background())
	var jobs sync.WaitGroup
	serving := make(chan struct{})
	go func() {
		defer close(serving)
		ln.serve(ctx, s, slog.New(slog.NewTextHandler(io.Discard, nil)), &jobs)
	}()
	defer func() {
		stopService()
		ln.close()
		stopped := make(chan struct{})
		go func() { <-serving; jobs.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Error("the service's listener did not stop")
		}
	}()

	// A job that keeps running while the agent stays connected.
	conn, err := dialPipe(path, me)
	if err != nil {
		t.Fatal(err)
	}
	agent, leave := context.WithCancel(context.Background())
	defer leave()
	out := &listening{lines: make(chan string, 100)}
	asked := make(chan error, 1)
	go func() { asked <- askService(agent, conn, runRequest("win-pc-1"), out) }()
	running, ok := out.hear(servicePrefix + serviceRunningAs)
	if !ok {
		t.Fatalf("the agent, still connected while the job ran, never heard the service's running as line; it heard %q", out.heard.String())
	}
	if _, ok := out.hear("Running job: build"); !ok {
		t.Fatalf("the agent, still connected while the job ran, never heard the job's output; it heard %q", out.heard.String())
	}
	name := strings.TrimSpace(strings.TrimPrefix(running, servicePrefix+serviceRunningAs))

	// The coordinator stopped the place: the agent leaves while the job runs.
	leave()
	if err := testrun.Receive(t, asked, "the agent to end once it left"); err != nil {
		t.Errorf("a job stopped by the agent leaving failed the agent: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(strings.Join(h.steps(), "\n"), "verify "+name) {
		if time.Now().After(deadline) {
			t.Fatalf("the service did not stop and delete %s after the agent left; it went through %q", name, stepsOf(h.steps(), name))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, want := strings.Join(stepsOf(h.steps(), name), " "), "create prepare start stop close "+strings.Join(cleanupOrder, " "); got != want {
		t.Errorf("%s went through %q after the agent left, want %q", name, got, want)
	}

	// A job that ends on its own while the agent stays connected: the
	// service's last line ends the agent's request well, before the agent
	// would give up.
	h.mu.Lock()
	h.block = false
	h.mu.Unlock()
	conn, err = dialPipe(path, me)
	if err != nil {
		t.Fatal(err)
	}
	waiting, giveUp := context.WithTimeout(context.Background(), 30*time.Second)
	defer giveUp()
	var all bytes.Buffer
	err = askService(waiting, conn, runRequest("win-pc-2"), &all)
	if waiting.Err() != nil {
		t.Fatalf("the agent heard no end of a job that ended while it stayed connected; it heard %q", all.String())
	}
	if err != nil {
		t.Fatalf("a job that ran to its end failed the agent: %v\n%s", err, all.String())
	}
	for _, want := range []string{servicePrefix + serviceRunningAs, "Running job: build", servicePrefix + serviceScriptEnded + "0"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("the agent did not hear %q from a job that ran to its end; it heard %q", want, all.String())
		}
	}
}

// listening is the agent's output as askService writes it, one line a write.
type listening struct {
	lines chan string
	heard strings.Builder
}

func (l *listening) Write(b []byte) (int, error) {
	l.lines <- string(b)
	return len(b), nil
}

// hear reads lines for up to 20 seconds until one holds want, and gives it.
func (l *listening) hear(want string) (string, bool) {
	deadline := time.After(20 * time.Second)
	for {
		select {
		case line := <-l.lines:
			l.heard.WriteString(line)
			if strings.Contains(line, want) {
				return line, true
			}
		case <-deadline:
			return "", false
		}
	}
}
