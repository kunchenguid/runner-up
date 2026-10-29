//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// Intent (the design: "configurable priority rules, learned
// estimates"): a freed place goes to the job the configured order rules pick.
// label:NAME puts a labeled pull request's jobs first, from the same
// repository only; workflow:GLOB puts a workflow's jobs first; waited:D keeps
// a job that waited D from being passed over; shortest runs the job with the
// shortest learned runtime first, and learns it from GitHub's completion
// messages. pool:NAME puts a pool's jobs first, so short jobs a merge waits
// on, given a pool of their own, go ahead even of jobs that waited an hour and
// of labeled pull requests on the places they share with other pools.

// onePlace is one machine with one place for every pool, so the order the
// place takes waiting jobs in is the order the rules pick.
func onePlace(dir string, order ...string) *config.Config {
	return &config.Config{
		StateDir: dir, Order: order,
		GitHub: config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
		Pools:  []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}},
		Machines: []config.Machine{{Name: "box", SSH: []string{"ci@box"}, Places: []config.Place{
			{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux", "ru-large"}, Cores: 2, MemoryMB: 4096},
		}}},
	}
}

// takeOrder feeds every waiting job while the place is busy, then frees it
// one job at a time and returns the jobs it took, in order.
func takeOrder(t *testing.T, cfg *config.Config, setup func(*harness)) []string {
	t.Helper()
	f := loadFixture(t, "waiting-jobs.json")
	busy := occupy(t, cfg, "box-1")
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	if setup != nil {
		setup(h)
	}
	h.now = f.Now
	h.feed(f)
	busy.Close()
	var got []string
	h.svc.Place(context.Background())
	for range len(f.Messages) + 1 {
		job, ok := h.placed()["box-1"]
		if !ok {
			break
		}
		var run PlaceRun
		config.ReadJSON(cfg.PlaceDir("box-1")+"/place.json", &run)
		got = append(got, job+" "+requestName(run.RequestID))
		h.finishAndPlace("box-1")
	}
	return got
}

func requestName(id int64) string {
	return map[int64]string{9101: "#9101", 9102: "#9102", 9103: "#9103", 7201: "#7201", 6301: "#6301", 9104: "#9104"}[id]
}

const (
	oVerify  = verify + " #9101"
	oPR184   = pr184 + " #9102"
	oPublish = publish + " #9103"
	oSuite   = suite + " #7201"
	oRelease = release + " #6301"
	oPR186   = "octo-org/app affected-journeys #9104"
)

func wantOrder(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("the place took\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestDefaultOrderIsFirstInFirstOutWithoutEstimates(t *testing.T) {
	got := takeOrder(t, onePlace(t.TempDir()), nil)
	wantOrder(t, got, []string{oVerify, oPR184, oPublish, oSuite, oRelease, oPR186})
}

// labelPR186 labels app's open pull request #186 urgent.
func labelPR186(h *harness) {
	h.svc.LabelReader = func(_ context.Context, repo string) (map[string]map[string]bool, error) {
		if repo != "octo-org/app" {
			return nil, nil
		}
		return map[string]map[string]bool{"refs/pull/186/merge": {"urgent": true}}, nil
	}
}

func TestLabeledPullRequestGoesFirst(t *testing.T) {
	cfg := onePlace(t.TempDir(), "label:urgent", "waited:60m", "shortest")
	got := takeOrder(t, cfg, labelPR186)
	// PR #186's job, the newest, goes first; the rest keep their order.
	wantOrder(t, got, []string{oPR186, oVerify, oPR184, oPublish, oSuite, oRelease})
}

func TestPoolGoesFirstAheadOfLabelsAndHourOldJobs(t *testing.T) {
	cfg := onePlace(t.TempDir(), "pool:ru-large", "label:urgent", "waited:60m", "shortest")
	got := takeOrder(t, cfg, labelPR186)
	// The ru-large jobs, publish and the wallet's release, go first, though
	// verify and PR #184 waited longer and PR #186 is labeled urgent. The
	// ru-linux jobs follow in the order the later rules give them.
	wantOrder(t, got, []string{oPublish, oRelease, oPR186, oVerify, oPR184, oSuite})
}

// A pool rule naming a pool the setup does not have would order nothing and
// say nothing, so the setup is refused.
func TestPoolRuleMustNameAPoolOfTheSetup(t *testing.T) {
	if err := onePlace(t.TempDir(), "pool:ru-large").Validate(); err != nil {
		t.Fatal(err)
	}
	if err := onePlace(t.TempDir(), "pool:ru-lagre").Validate(); err == nil || !strings.Contains(err.Error(), `"pool:ru-lagre"`) {
		t.Fatalf("a pool rule for a misspelled pool: %v", err)
	}
}

func TestReleaseWorkflowGoesFirst(t *testing.T) {
	cfg := onePlace(t.TempDir(), "workflow:release", "shortest")
	got := takeOrder(t, cfg, func(h *harness) {
		h.svc.WorkflowReader = func(_ context.Context, repo string, id int64) (string, error) {
			// The runs' top-level workflows. app's run 39361345555 is its
			// release, which calls verify.yml as a reusable workflow.
			return map[int64]string{39361345555: ".github/workflows/release.yml", 39372831799: ".github/workflows/journeys.yml",
				39381269010: ".github/workflows/journeys.yml", 39370000001: ".github/workflows/suite.yml",
				39370000002: ".github/workflows/release.yml"}[id], nil
		}
	})
	// Both release runs' jobs go first, oldest first: verify counts as
	// release, since its run's top-level workflow counts, not the job's own
	// file.
	wantOrder(t, got, []string{oVerify, oPublish, oRelease, oPR184, oSuite, oPR186})
}

func TestShortestLearnedJobGoesFirstUnlessAnotherWaitedAnHour(t *testing.T) {
	teach := func(h *harness) {
		// GitHub's completion of an earlier successful suite run teaches its
		// runtime, 4 minutes from runner assignment to finish; a cancelled
		// verify teaches nothing.
		at := time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
		h.now = at
		msg := &scaleset.RunnerScaleSetMessage{MessageID: 1, JobCompletedMessages: []*scaleset.JobCompleted{
			{Result: "succeeded", RunnerName: "runner-up-box-1-1", JobMessageBase: scaleset.JobMessageBase{
				RepositoryName: "tool", OwnerName: "octo-user", JobDisplayName: "suite", WorkflowRunID: 39360000001,
				JobWorkflowRef:   "octo-user/tool/.github/workflows/suite.yml@refs/heads/main",
				RunnerAssignTime: at.Add(-4 * time.Minute), FinishTime: at}},
			{Result: "canceled", RunnerName: "runner-up-box-1-2", JobMessageBase: scaleset.JobMessageBase{
				RepositoryName: "app", OwnerName: "octo-org", JobDisplayName: "verify / linux", WorkflowRunID: 39360000002,
				JobWorkflowRef:   "octo-org/app/.github/workflows/verify.yml@refs/heads/main",
				RunnerAssignTime: at.Add(-time.Minute), FinishTime: at}},
		}}
		if err := h.svc.Handle(context.Background(), "octo-user/tool", "ru-linux", msg); err != nil {
			h.t.Fatal(err)
		}
	}
	// With shortest alone the suite, known to take 4 minutes, goes ahead of
	// every job of unknown length.
	got := takeOrder(t, onePlace(t.TempDir(), "shortest"), teach)
	wantOrder(t, got, []string{oSuite, oVerify, oPR184, oPublish, oRelease, oPR186})

	// Under the default order, jobs that waited an hour go first, oldest
	// first, and the short suite only beats PR #186, the one job younger
	// than an hour.
	got = takeOrder(t, onePlace(t.TempDir()), teach)
	wantOrder(t, got, []string{oVerify, oPR184, oPublish, oSuite, oRelease, oPR186})
}

func TestOrderRulesAreChecked(t *testing.T) {
	for _, bad := range [][]string{{"label:"}, {"pool:"}, {"waited:soon"}, {"workflow:["}, {"shortest:yes"}, {"fastest"}} {
		if _, err := plan.ParseOrder(bad); err == nil {
			t.Errorf("order %v was accepted", bad)
		}
	}
	if _, err := plan.ParseOrder([]string{"pool:ru-large", "label:urgent", "waited:30m", "workflow:release,deploy-*", "shortest"}); err != nil {
		t.Fatal(err)
	}
}

// pullLabels reads labels from the repository's own open pull requests only:
// a fork's pull request cannot label its way to the front.
func TestPullRequestLabelsComeOnlyFromTheRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo-org/app/pulls" || r.Header.Get("Authorization") != "Bearer local-only" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		// The fields GitHub's list of pull requests carries that the reader
		// uses, for one pull request from a branch and one from a fork.
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 186, "state": "open", "labels": []map[string]string{{"name": "urgent"}},
				"head": map[string]any{"ref": "fix/session-header", "repo": map[string]string{"full_name": "octo-org/app"}}},
			{"number": 187, "state": "open", "labels": []map[string]string{{"name": "urgent"}},
				"head": map[string]any{"ref": "main", "repo": map[string]string{"full_name": "a-contributor/app"}}},
		})
	}))
	defer server.Close()
	api := NewGitHubAPI(func(context.Context) (string, error) { return "local-only", nil })
	api.base = server.URL
	got, err := api.PullLabels(context.Background(), "octo-org/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/pull/186/merge", "refs/pull/186/head", "refs/heads/fix/session-header"} {
		if !got[ref]["urgent"] {
			t.Errorf("%s is not labeled urgent: %v", ref, got)
		}
	}
	for _, ref := range []string{"refs/pull/187/merge", "refs/heads/main"} {
		if len(got[ref]) != 0 {
			t.Errorf("the fork's pull request labeled %s: %v", ref, got[ref])
		}
	}
}
