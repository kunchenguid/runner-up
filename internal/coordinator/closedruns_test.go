//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (the captain's direction C on INF-2175: "the coordinator forgets
// jobs GitHub closed"): GitHub can stop mentioning a job it assigned to the
// scale set without ever sending its cancellation, and the coordinator kept
// such a job waiting for a day, starting runners for it while it read as
// assigned. A waiting job GitHub has gone quiet about is asked about by its
// run, with few reads ("keep the coordinator's own GitHub calls for this
// check bounded and cheap"), and forgotten once GitHub says the run is
// closed; a job of an open run, an unanswered question, a job GitHub assigns
// again meanwhile, and a job with a runner are never forgotten.

// The recorded job's run.
const proofRun = 39463703013

// runAnswers stands in for GitHub's answer about runs and counts the
// questions.
type runAnswers struct {
	closed bool
	err    error
	asked  []int64
	during func()
}

func (r *runAnswers) read(_ context.Context, repo string, id int64) (bool, error) {
	r.asked = append(r.asked, id)
	if repo != "octo-org/app" {
		return false, errors.New("unexpected repository " + repo)
	}
	if r.during != nil {
		r.during()
	}
	return r.closed, r.err
}

// quietJob feeds the recorded messages [0, to) while both places are busy,
// so the job waits, then frees box-1. It returns the harness and the time of
// the last message.
func quietJob(t *testing.T, to int, answers *runAnswers) (*harness, time.Time, func()) {
	t.Helper()
	f := loadFixture(t, "assigned-takebacks.json")
	cfg := proofBox(t.TempDir(), "box-1", "box-2")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.svc.RunReader = answers.read
	one := occupy(t, cfg, "box-1")
	occupy(t, cfg, "box-2")
	h.feedRange(f, 0, to)
	if len(h.svc.waiting) != 1 {
		t.Fatalf("waiting %v, want the recorded job", h.svc.waiting)
	}
	return h, f.Messages[to-1].At, func() { one.Close() }
}

func TestJobOfARunGitHubClosedIsForgotten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		to    int
		quiet time.Duration
	}{
		// The job is assigned, and its cancellation never reaches the
		// coordinator, as when a session ends before it is delivered. GitHub
		// takes an assigned job no runner took back after five minutes, so
		// ten quiet minutes are suspect.
		{"assigned, its cancellation lost", 5, 10 * time.Minute},
		// GitHub took the assignment back, then the run was cancelled: GitHub
		// assigns it to nothing and says nothing more. A taken-back job of a
		// long queue can wait quietly for its next assignment, and no runner
		// starts for it, so only an hour is suspect.
		{"taken back, then cancelled", 4, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := &runAnswers{closed: true}
			h, last, free := quietJob(t, tc.to, answers)

			// Just short of its quiet time, nothing is asked.
			h.now = last.Add(tc.quiet - time.Minute)
			h.svc.Place(context.Background())
			if len(answers.asked) != 0 || len(h.svc.waiting) != 1 {
				t.Fatalf("asked %v, waiting %v; want no question and the job waiting", answers.asked, h.svc.waiting)
			}

			// At its quiet time: one question about its run, which GitHub
			// says is closed, and the job is forgotten, also across a restart.
			h.now = last.Add(tc.quiet)
			h.svc.Place(context.Background())
			if len(answers.asked) != 1 || answers.asked[0] != proofRun || len(h.svc.waiting) != 0 {
				t.Fatalf("asked %v, waiting %v; want one question about run %d and nothing waiting", answers.asked,
					h.svc.waiting, proofRun)
			}
			restarted, err := NewService(h.cfg, h.svc.Log, h.launcher)
			if err != nil || len(restarted.waiting) != 0 {
				t.Fatalf("after a restart waiting %v (%v), want nothing", restarted.waiting, err)
			}

			// A freed place starts no runner for it.
			free()
			h.svc.Place(context.Background())
			h.want(map[string]string{"box-2": suite})
		})
	}
}

func TestJobOfAnOpenRunWaitsAndIsAskedAboutRarely(t *testing.T) {
	open := func() *runAnswers { return &runAnswers{} }
	unanswered := func() *runAnswers {
		return &runAnswers{closed: true, err: errors.New("GET /repos/octo-org/app/actions/runs/39463703013 returned HTTP 403")}
	}
	for _, tc := range []struct {
		name           string
		answers        *runAnswers
		to             int
		quiet, recheck time.Duration
	}{
		{"assigned, GitHub says the run is open", open(), 5, 10 * time.Minute, 15 * time.Minute},
		{"assigned, GitHub does not answer", unanswered(), 5, 10 * time.Minute, 15 * time.Minute},
		{"taken back, GitHub says the run is open", open(), 4, time.Hour, time.Hour},
		{"taken back, GitHub does not answer", unanswered(), 4, time.Hour, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, last, free := quietJob(t, tc.to, tc.answers)
			h.now = last.Add(tc.quiet)
			h.svc.Place(context.Background())
			if len(tc.answers.asked) != 1 || len(h.svc.waiting) != 1 {
				t.Fatalf("asked %v, waiting %v; want one question and the job waiting", tc.answers.asked, h.svc.waiting)
			}

			// Every placement each minute asks nothing more until the
			// recheck time is up.
			for range int(tc.recheck/time.Minute) - 1 {
				h.now = h.now.Add(time.Minute)
				h.svc.Place(context.Background())
			}
			if len(tc.answers.asked) != 1 {
				t.Fatalf("asked %v within %s, want one question", tc.answers.asked, tc.recheck)
			}
			h.now = h.now.Add(time.Minute)
			h.svc.Place(context.Background())
			if len(tc.answers.asked) != 2 {
				t.Fatalf("asked %v after %s, want a second question", tc.answers.asked, tc.recheck)
			}

			// The job is kept: once GitHub assigns it, as it recorded, it gets
			// the freed place.
			free()
			if tc.to < 5 {
				msg := loadFixture(t, "assigned-takebacks.json").Messages[4].Message
				if err := h.svc.Handle(context.Background(), "octo-org/app", "ru-proof", &msg); err != nil {
					t.Fatal(err)
				}
			}
			h.svc.Place(context.Background())
			h.want(map[string]string{"box-1": proofJob, "box-2": suite})
		})
	}
}

func TestJobAssignedAgainWhileItsRunIsAskedAboutWaits(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	answers := &runAnswers{closed: true}
	h, last, free := quietJob(t, 5, answers)
	// GitHub answers about the run as it was, closed; meanwhile the run is run
	// again and its job assigned anew, the way GitHub assigns it after a
	// take-back.
	again := f.Messages[2].Message.JobAssignedMessages[0].JobMessageBase
	answers.during = func() {
		h.svc.mu.Lock()
		h.svc.waitLocked(jobOf("octo-org/app", "ru-proof", again, true, last.Add(10*time.Minute+time.Second)))
		h.svc.mu.Unlock()
	}
	h.now = last.Add(10 * time.Minute)
	h.svc.Place(context.Background())
	if len(answers.asked) != 1 || len(h.svc.waiting) != 1 {
		t.Fatalf("asked %v, waiting %v; want one question and the job assigned again waiting", answers.asked, h.svc.waiting)
	}
	free()
	h.now = h.now.Add(time.Minute)
	h.svc.Place(context.Background())
	h.want(map[string]string{"box-1": proofJob, "box-2": suite})
}

func TestJobWithARunnerIsNeverAskedAbout(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	answers := &runAnswers{closed: true}
	h := newHarness(t, proofBox(t.TempDir(), "box-1", "box-2"), newFakeLauncher(), map[string]*fakeSet{})
	h.svc.RunReader = answers.read
	h.feedRange(f, 0, 1)
	h.want(map[string]string{"box-1": proofJob})

	// Its runner lives, waiting for the job, past the quiet minutes.
	h.now = f.Messages[0].At.Add(11 * time.Minute)
	h.svc.Place(context.Background())
	first := f.Messages[0].Message.JobAssignedMessages[0]
	if _, ok := h.svc.acquired[plan.RunKey("octo-org/app", "ru-proof", first.WorkflowRunID, first.JobDisplayName)]; !ok || len(answers.asked) != 0 {
		t.Fatalf("acquired %v, asked %v; want the job kept with its runner and no question", h.svc.acquired, answers.asked)
	}
}

// GitHub's run record says whether the run is closed; a record of another
// run or repository answers nothing.
func TestRunClosedReadsTheRunsStatus(t *testing.T) {
	status, repo := "completed", "octo-org/app"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo-org/app/actions/runs/39463703013" || r.Header.Get("Authorization") != "Bearer local-only" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		// The fields of GitHub's workflow run record the reader uses.
		json.NewEncoder(w).Encode(map[string]any{"id": proofRun, "status": status, "conclusion": "cancelled",
			"repository": map[string]string{"full_name": repo}})
	}))
	defer server.Close()
	api := NewGitHubAPI(func(context.Context) (string, error) { return "local-only", nil })
	api.base = server.URL
	for _, tc := range []struct {
		status, repo string
		closed, err  bool
	}{
		{"completed", "octo-org/app", true, false},
		{"queued", "octo-org/app", false, false},
		{"in_progress", "octo-org/app", false, false},
		{"completed", "octo-org/other", false, true},
	} {
		status, repo = tc.status, tc.repo
		closed, err := api.RunClosed(context.Background(), "octo-org/app", proofRun)
		if closed != tc.closed || (err != nil) != tc.err {
			t.Errorf("status %s of %s: closed %v, err %v", tc.status, tc.repo, closed, err)
		}
	}
	if _, err := api.RunClosed(context.Background(), "octo-org/app", 1); err == nil {
		t.Error("a run GitHub has no record of read as an answer")
	}
}

// However many runs are quiet, a pass reads ten and an hour sixty: here the
// recorded job's message stands for one job of each of seventy runs, all
// quiet, all open.
func TestRunReadsAreBoundedPerPassAndPerHour(t *testing.T) {
	f := loadFixture(t, "assigned-takebacks.json")
	answers := &runAnswers{}
	cfg := proofBox(t.TempDir(), "box-1", "box-2")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.svc.RunReader = answers.read
	occupy(t, cfg, "box-1")
	occupy(t, cfg, "box-2")
	h.now = f.Messages[0].At
	for i := range int64(70) {
		msg := loadFixture(t, "assigned-takebacks.json").Messages[0].Message
		msg.JobAssignedMessages[0].WorkflowRunID = proofRun + i
		if err := h.svc.Handle(context.Background(), "octo-org/app", "ru-proof", &msg); err != nil {
			t.Fatal(err)
		}
	}
	h.now = h.now.Add(10 * time.Minute)
	h.svc.Place(context.Background())
	if len(answers.asked) != runReadsPerPass {
		t.Fatalf("one pass asked about %d runs, want %d", len(answers.asked), runReadsPerPass)
	}
	start := h.now
	for h.now.Sub(start) < 59*time.Minute {
		h.now = h.now.Add(time.Minute)
		h.svc.Place(context.Background())
		h.svc.Place(context.Background())
	}
	if len(answers.asked) != runReadsPerHour {
		t.Fatalf("an hour asked %d times, want %d", len(answers.asked), runReadsPerHour)
	}
	h.now = start.Add(time.Hour)
	h.svc.Place(context.Background())
	if len(answers.asked) != runReadsPerHour+runReadsPerPass {
		t.Fatalf("the next hour's first pass asked %d times in all, want %d", len(answers.asked), runReadsPerHour+runReadsPerPass)
	}
	if len(h.svc.waiting) != 70 {
		t.Fatalf("%d jobs waiting, want all seventy", len(h.svc.waiting))
	}
}
