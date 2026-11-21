package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Order rules"): each rule puts the jobs it matches first,
// except last:, which puts them last; later rules order jobs tied by earlier
// ones, and the oldest job breaks a final tie. The rules are plain functions
// of the waiting jobs, so this package owns what each rule kind does; the
// coordinator's own test (internal/coordinator, rules_test.go) owns only that
// the label, workflow and runtime readings reach these rules.

// waiting reads the jobs that waited for one machine's places at the same
// time, from the coordinator's recorded scale set messages, as the
// coordinator turns a JobAvailable message into a waiting job. It returns
// them with the moment the order was taken.
func waiting(t *testing.T) ([]Job, time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "coordinator", "testdata", "messages", "waiting-jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Now      time.Time `json:"now"`
		Messages []struct {
			Repo    string `json:"repo"`
			Label   string `json:"label"`
			Message struct {
				JobAvailableMessages []struct {
					RunnerRequestID int64     `json:"runnerRequestId"`
					DisplayName     string    `json:"jobDisplayName"`
					WorkflowRef     string    `json:"jobWorkflowRef"`
					RunID           int64     `json:"workflowRunId"`
					QueueTime       time.Time `json:"queueTime"`
				}
			} `json:"message"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var jobs []Job
	for _, m := range f.Messages {
		for _, a := range m.Message.JobAvailableMessages {
			jobs = append(jobs, Job{Target: m.Repo, Repo: m.Repo, Pool: m.Label, RequestID: a.RunnerRequestID,
				Name: a.DisplayName, Workflow: a.WorkflowRef, RunID: a.RunID, QueueTime: a.QueueTime})
		}
	}
	if len(jobs) != 6 {
		t.Fatalf("the recording holds %d waiting jobs, want 6", len(jobs))
	}
	return jobs, f.Now
}

// The recorded jobs by their runner request, oldest first. ru-large jobs are
// publish and the wallet's release; the rest ask for ru-linux.
const (
	verify  = "verify"  // 9101, app's release run, verify.yml, waited 3h46m
	pr184   = "pr184"   // 9102, app's pull request #184, waited 1h37m
	publish = "publish" // 9103, app's release run, release.yml, ru-large, waited 1h27m
	suite   = "suite"   // 7201, tool's suite, waited 1h20m
	release = "release" // 6301, wallet's release, ru-large, waited 1h10m
	pr186   = "pr186"   // 9104, app's pull request #186, waited 5m
)

var names = map[int64]string{9101: verify, 9102: pr184, 9103: publish, 7201: suite, 6301: release, 9104: pr186}

func order(jobs []Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, names[j.RequestID])
	}
	return out
}

// setupWith is a setup with the recording's two pools and the order rules.
func setupWith(rules ...string) *config.Config {
	return &config.Config{Order: rules, Pools: []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}}}
}

func TestEachOrderRuleKindPutsItsJobsFirst(t *testing.T) {
	// fill sets what the coordinator reads before planning: pull request
	// labels, the run's top-level workflow and learned runtimes.
	type fill func(j *Job)
	labelPR186 := func(j *Job) {
		if names[j.RequestID] == pr186 {
			j.Labels = map[string]bool{"urgent": true}
		}
	}
	// app's run 39361345555 is its release, which calls verify.yml as a
	// reusable workflow; the other runs' top-level files are their own.
	runWorkflows := func(j *Job) {
		if j.RunID == 39361345555 {
			j.RunWorkflow = "octo-org/app/.github/workflows/release.yml@refs/heads/main"
		} else {
			j.RunWorkflow = j.Workflow
		}
	}
	learned := func(m map[string]time.Duration) fill {
		return func(j *Job) { j.Estimate = m[names[j.RequestID]] }
	}
	for _, tc := range []struct {
		why   string
		rules []string
		fill  []fill
		want  []string
	}{
		{"the default order, nothing learned, is oldest first", nil, nil,
			[]string{verify, pr184, publish, suite, release, pr186}},
		{"pool: puts its pool's jobs first", []string{"pool:ru-large"}, nil,
			[]string{publish, release, verify, pr184, suite, pr186}},
		{"last: puts its pool's jobs behind the rest", []string{"last:ru-linux"}, nil,
			[]string{publish, release, verify, pr184, suite, pr186}},
		{"last: of the other pool keeps the rest in age order", []string{"last:ru-large"}, nil,
			[]string{verify, pr184, suite, pr186, publish, release}},
		{"label: puts the labeled pull request first", []string{"label:urgent"}, []fill{labelPR186},
			[]string{pr186, verify, pr184, publish, suite, release}},
		{"waited: puts jobs past the wait first, oldest first, ahead of shorter ones",
			[]string{"waited:90m", "shortest"}, []fill{learned(map[string]time.Duration{pr184: 2 * time.Minute, suite: 4 * time.Minute})},
			[]string{verify, pr184, suite, publish, release, pr186}},
		{"workflow: reads the run's top-level file, so a reusable verify counts",
			[]string{"workflow:release"}, []fill{runWorkflows},
			[]string{verify, publish, release, pr184, suite, pr186}},
		{"workflow: falls back to the job's own file when the run's is unread",
			[]string{"workflow:release"}, nil,
			[]string{publish, release, verify, pr184, suite, pr186}},
		{"workflow: takes a glob list", []string{"workflow:nightly,jour*"}, nil,
			[]string{pr184, pr186, verify, publish, suite, release}},
		{"shortest puts the shortest learned runtime first", []string{"shortest"},
			[]fill{learned(map[string]time.Duration{suite: 4 * time.Minute})},
			[]string{suite, verify, pr184, publish, release, pr186}},
		{"shortest counts a job never seen to succeed as 15 minutes", []string{"shortest"},
			[]fill{learned(map[string]time.Duration{verify: 20 * time.Minute, release: 14 * time.Minute})},
			[]string{release, pr184, publish, suite, pr186, verify}},
		{"an earlier rule wins over a later one", []string{"pool:ru-large", "label:urgent", "waited:60m", "shortest"},
			[]fill{labelPR186}, []string{publish, release, pr186, verify, pr184, suite}},
		{"a later rule orders the earlier rule's ties", []string{"label:urgent", "workflow:release", "shortest"},
			[]fill{labelPR186, runWorkflows, learned(map[string]time.Duration{suite: 4 * time.Minute})},
			[]string{pr186, verify, publish, release, suite, pr184}},
	} {
		jobs, now := waiting(t)
		for i := range jobs {
			for _, f := range tc.fill {
				f(&jobs[i])
			}
		}
		rules, err := ParseOrder(setupWith(tc.rules...))
		if err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		// Rank must not lean on the order jobs arrive in.
		slices.Reverse(jobs)
		if got := order(Rank(jobs, rules, now)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ranked\n  %s\nwant\n  %s", tc.why, strings.Join(got, " "), strings.Join(tc.want, " "))
		}
	}
}

// Intent (README, "Order rules" and "Setup file"): a rule the coordinator
// could not follow, or a pool: or last: rule naming a pool the setup does not
// have, which would order nothing and say nothing, is refused when the setup
// loads.
func TestOrderRulesAreChecked(t *testing.T) {
	for _, bad := range []string{"label:", "pool:", "pool:ru-lagre", "pool:ru-large,", "pool:ru-large,ru-lagre", "pool:ru-large,ru-large",
		"last:", "last:ru-linx", "last:ru-linux,ru-linx", "waited:soon", "waited:-5m",
		"workflow:[", "workflow:release,", "shortest:yes", "fastest"} {
		if _, err := ParseOrder(setupWith(bad)); err == nil || !strings.Contains(err.Error(), `"`+bad+`"`) {
			t.Errorf("order rule %q: the check said %v", bad, err)
		}
	}
	if _, err := ParseOrder(setupWith("pool:ru-large", "last:ru-linux", "pool:ru-large,ru-linux", "last:ru-linux, ru-large", "label:urgent",
		"waited:30m", "workflow:release,deploy-*", "shortest")); err != nil {
		t.Fatal(err)
	}
	if rules, err := ParseOrder(setupWith()); err != nil || len(rules) != len(defaultOrder) {
		t.Fatalf("a setup with no order rules got %v, %v; want the default order", rules, err)
	}
}

// Intent (README, "Order rules": label: matches jobs "in the same repository
// (not from forks), including runs on its branch"): a label reaches a job only
// through its own repository's pull request or branch ref, so a reusable
// workflow in another repository, a tag, or a ref shaped like a pull
// request's that is not one, cannot claim a label.
func TestOnlyTheRepositorysOwnPullRequestOrBranchCanCarryALabel(t *testing.T) {
	for _, tc := range []struct{ workflow, want string }{
		// The recording's refs.
		{"octo-org/app/.github/workflows/journeys.yml@refs/pull/184/merge", "refs/pull/184/merge"},
		{"octo-org/app/.github/workflows/verify.yml@refs/heads/main", "refs/heads/main"},
		{"octo-org/app/.github/workflows/journeys.yml@refs/pull/186/head", "refs/pull/186/head"},
		{"octo-org/app/.github/workflows/journeys.yml@refs/heads/fix/session-header", "refs/heads/fix/session-header"},
		// A reusable workflow from another repository.
		{"octo-org/shared/.github/workflows/journeys.yml@refs/pull/184/merge", ""},
		{"octo-org/app/.github/workflows/release.yml@refs/tags/v1.2.0", ""},
		{"octo-org/app/.github/workflows/journeys.yml@refs/pull/184/files", ""},
		{"octo-org/app/.github/workflows/journeys.yml@refs/pull/x184/merge", ""},
		{"octo-org/app/.github/workflows/journeys.yml@refs/heads/", ""},
		{"octo-org/app/.github/workflows/journeys.yml", ""},
	} {
		j := Job{Repo: "octo-org/app", Workflow: tc.workflow}
		if got := j.LabelRef(); got != tc.want {
			t.Errorf("%s: a label may reach %q, want %q", tc.workflow, got, tc.want)
		}
	}
}

// Intent (README, "Order rules"): the coordinator reads pull request labels
// and run workflows from GitHub only when a rule uses them.
func TestRulesSayWhichReadingsTheyNeed(t *testing.T) {
	for _, tc := range []struct {
		rules             []string
		labels, workflows bool
	}{
		{nil, false, false},
		{[]string{"label:urgent", "shortest"}, true, false},
		{[]string{"workflow:release"}, false, true},
		{[]string{"pool:ru-large", "label:urgent", "workflow:release"}, true, true},
	} {
		rules, err := ParseOrder(setupWith(tc.rules...))
		if err != nil {
			t.Fatal(err)
		}
		if labels, workflows := Needs(rules); labels != tc.labels || workflows != tc.workflows {
			t.Errorf("%v needs labels=%v workflows=%v, want %v %v", tc.rules, labels, workflows, tc.labels, tc.workflows)
		}
	}
}

// Intent (README, "Order rules": a pool: or last: rule may list several
// pools, which tie): pools listed in one rule share its side of the line, in
// the order the later rules give, so a busy pool listed first never holds
// back another listed with it, while pools left out stay behind (or, for
// last:, ahead of) them all. A rule per pool instead ranks each pool above
// the next.
func TestAPoolRuleListingSeveralPoolsTiesThem(t *testing.T) {
	cfg := &config.Config{Pools: []config.Pool{{Label: "ru-busy"}, {Label: "ru-quiet"}, {Label: "ru-shared"}}}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	job := func(pool string, id int64, waited time.Duration) Job {
		return Job{Target: "octo-org/" + pool, Repo: "octo-org/" + pool, Pool: pool, RequestID: id, QueueTime: now.Add(-waited)}
	}
	// The quiet pool's job waited longest; the busy pool's arrived after it.
	jobs := []Job{job("ru-shared", 1, 50*time.Minute), job("ru-quiet", 2, 40*time.Minute),
		job("ru-busy", 3, 20*time.Minute), job("ru-busy", 4, 5*time.Minute)}
	pools := func(ranked []Job) []string {
		var out []string
		for _, j := range ranked {
			out = append(out, fmt.Sprintf("%s#%d", j.Pool, j.RequestID))
		}
		return out
	}
	for _, tc := range []struct {
		why   string
		rules []string
		want  []string
	}{
		{"a rule per pool ranks the first pool's later jobs ahead of the second's",
			[]string{"pool:ru-busy", "pool:ru-quiet"},
			[]string{"ru-busy#3", "ru-busy#4", "ru-quiet#2", "ru-shared#1"}},
		{"one rule listing both keeps them in age order, ahead of the rest",
			[]string{"pool:ru-busy,ru-quiet"},
			[]string{"ru-quiet#2", "ru-busy#3", "ru-busy#4", "ru-shared#1"}},
		{"one last: rule listing both puts them behind the rest, in age order",
			[]string{"last:ru-quiet,ru-busy"},
			[]string{"ru-shared#1", "ru-quiet#2", "ru-busy#3", "ru-busy#4"}},
	} {
		cfg.Order = tc.rules
		rules, err := ParseOrder(cfg)
		if err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		in := slices.Clone(jobs)
		slices.Reverse(in)
		if got := pools(Rank(in, rules, now)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ranked\n  %s\nwant\n  %s", tc.why, strings.Join(got, " "), strings.Join(tc.want, " "))
		}
	}
}
