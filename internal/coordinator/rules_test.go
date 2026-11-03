//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Order rules"): a freed place goes to the job the
// configured order rules pick, across repositories and pools, with the pull
// request labels, run workflows and learned runtimes the coordinator reads
// from GitHub. What each rule does is owned by internal/plan (rules_test.go).

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

// The order rules themselves are owned by internal/plan (rules_test.go); this
// test owns their wiring: the labels the label reader finds, the run
// workflows the workflow reader finds and the runtimes GitHub's completion
// messages teach all reach the rules, and the place takes jobs in the order
// they give. Each reading moves one job: PR #186 goes first only by its
// label, verify counts as release only by its run's top-level workflow, and
// the suite beats PR #184 only by its learned runtime.
func TestOrderRulesReadLabelsWorkflowsAndLearnedRuntimes(t *testing.T) {
	cfg := onePlace(t.TempDir(), "label:urgent", "workflow:release", "shortest")
	got := takeOrder(t, cfg, func(h *harness) {
		labelPR186(h)
		h.svc.WorkflowReader = func(_ context.Context, repo string, id int64) (string, error) {
			// The runs' top-level workflows. app's run 39361345555 is its
			// release, which calls verify.yml as a reusable workflow.
			return map[int64]string{39361345555: ".github/workflows/release.yml", 39372831799: ".github/workflows/journeys.yml",
				39381269010: ".github/workflows/journeys.yml", 39370000001: ".github/workflows/suite.yml",
				39370000002: ".github/workflows/release.yml"}[id], nil
		}
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
		h.handle("octo-user/tool", "ru-linux", msg)
	})
	wantOrder(t, got, []string{oPR186, oVerify, oPublish, oRelease, oSuite, oPR184})
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

// Intent (README, "Order rules", last:NAME). last:NAME puts a pool's jobs
// behind every other job, so Linux guests sharing a Mac's budget never take the
// room a waiting Mac job needs: that job either starts or keeps its room first,
// and the guests use only what is left. The 14-core budget, 12-core Mac pool
// and 2-core guests are the README's illustration in "Job sizes and readings".
func TestLastPoolLeavesRoomForOtherJobsFirst(t *testing.T) {
	mini := func(order ...string) *config.Config {
		cfg := onePlace(t.TempDir(), order...)
		cfg.Pools[0].Cores, cfg.Pools[0].MemoryMB = 2, 8192
		cfg.Pools[1].Cores, cfg.Pools[1].MemoryMB = 12, 32768
		m := &cfg.Machines[0]
		m.Cores, m.RoomCores = 22, 8
		m.Places = []config.Place{
			{ID: "mac", Provider: config.ProviderTart, Image: "ci-base", Pools: []string{"ru-large"}, Cores: 12, MemoryMB: 32768},
			{ID: "guest-1", Provider: config.ProviderContainer, Image: "runner-up-linux", Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 8192},
			{ID: "guest-2", Provider: config.ProviderContainer, Image: "runner-up-linux", Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 8192},
		}
		return cfg
	}
	// Every job waits while the places are busy, then they free together.
	place := func(cfg *config.Config) *harness {
		f := loadFixture(t, "waiting-jobs.json")
		var busy []*os.File
		for _, id := range []string{"mac", "guest-1", "guest-2"} {
			busy = append(busy, occupy(t, cfg, id))
		}
		h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
		h.now = f.Now
		h.feed(f)
		for _, b := range busy {
			b.Close()
		}
		h.svc.Place(context.Background())
		return h
	}

	// Without the rule the older Linux jobs take both guests first, and the
	// 12-core publish no longer fits beside their 4 cores.
	h := place(mini("waited:60m", "shortest"))
	h.want(map[string]string{"guest-1": verify, "guest-2": pr184})

	// With it publish starts first, and one guest takes the 2 cores left.
	h = place(mini("last:ru-linux", "waited:60m", "shortest"))
	h.want(map[string]string{"mac": publish, "guest-1": verify})
	h.launcher.finish(t, "mac")
	h.launcher.finish(t, "guest-1")
}
