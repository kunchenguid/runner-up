package agent

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (README, "When a machine is down", and the fixed bug: during a
// router outage of a few minutes, the coordinator stopped answering two
// compute Macs' SSH keepalives, their SSH servers closed the place sessions,
// and each agent took its input ending as a stop and deleted its VM mid-job):
// losing the link to the coordinator - input that ends without "stop", or a
// hangup - never stops the place or cleans it up. Only the run returning
// does, and the coordinator reconnects to collect its result.

func TestClosedInputDoesNotCleanUpThePlaceBeforeItsRunReturns(t *testing.T) {
	loseLink(t, func(input io.Closer) { input.Close() })
}

// heldPlace is a provider whose run holds its place until released, and that
// counts its clean-ups.
type heldPlace struct {
	running chan io.Writer
	release chan struct{}
	cleans  atomic.Int32
	// stopped: the run's context ended before it was released. cleansInRun:
	// the clean-ups made before the run returned.
	stopped     atomic.Bool
	cleansInRun atomic.Int32
}

func (h *heldPlace) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	h.running <- out
	select {
	case <-h.release:
	case <-ctx.Done():
		h.stopped.Store(true)
	}
	h.cleansInRun.Store(h.cleans.Load())
	return nil
}

func (h *heldPlace) clean(context.Context, *AgentRun) error { h.cleans.Add(1); return nil }
func (h *heldPlace) launch(*AgentRun) runnerLaunch          { return runnerLaunch{} }
func (h *heldPlace) ready() string                          { return "ok" }

// loseLink runs a held place and, once its run has started, loses the
// agent's link to the coordinator with lose. Then it reconnects as the
// coordinator's place runner does, releases the run and checks the place was
// cleaned up only after the run returned, with the result handed to the
// reconnected link.
func loseLink(t *testing.T, lose func(input io.Closer)) {
	t.Helper()
	h := &heldPlace{running: make(chan io.Writer, 1), release: make(chan struct{})}
	a := &AgentRun{Provider: "held", Place: "box-1", StateDir: t.TempDir()}
	in, input := io.Pipe()
	defer input.Close()
	ended := make(chan int, 1)
	go func() { ended <- agentRunPlace(a, h, in, io.Discard) }()
	io.WriteString(input, "registration\n")
	s := testrun.Receive(t, h.running, "the place's run to start").(*session)

	lose(input)
	waitUntil(t, "the agent to see its link to the coordinator go", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.link == nil
	})
	if h.stopped.Load() || h.cleans.Load() != 1 {
		t.Fatalf("losing the link stopped the place (%v) or cleaned it up (%d clean-ups, want the one before the run)", h.stopped.Load(), h.cleans.Load())
	}

	// The coordinator reconnects, and the run ends on its own.
	attachIn, keep := io.Pipe()
	defer keep.Close()
	var out bytes.Buffer
	attached := make(chan int, 1)
	go func() { attached <- agentAttach(a, attachIn, &out) }()
	close(h.release)
	code := testrun.Receive(t, attached, "the reconnected coordinator to get the run's result")
	if got := testrun.Receive(t, ended, "the agent to end"); got != ExitDone {
		t.Errorf("the agent exited %d, want %d", got, ExitDone)
	}
	if h.stopped.Load() {
		t.Error("the place was stopped, though its run was never told to stop")
	}
	if got := h.cleansInRun.Load(); got != 1 {
		t.Errorf("%d clean-ups before the run returned, want the one before it started", got)
	}
	if got := h.cleans.Load(); got != 2 {
		t.Errorf("%d clean-ups in all, want one before the run and one after", got)
	}
	if code != ExitDone || !strings.Contains(out.String(), "lost the link to the coordinator; the place keeps running") ||
		!strings.Contains(out.String(), "place box-1 finished") {
		t.Errorf("the reconnected coordinator got exit %d and:\n%s", code, out.String())
	}
}

// waitUntil waits for ok to hold, within testrun's deadline.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	stop := make(chan struct{})
	defer close(stop)
	testrun.Wait(t, what, func() {
		for !ok() {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}
