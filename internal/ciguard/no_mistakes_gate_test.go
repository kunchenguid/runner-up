package ciguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

// requiredCheckContext is the status check context this repository's main
// ruleset requires. The workflow job name must match it exactly, or the
// ruleset waits forever on a check nothing ever reports.
const requiredCheckContext = "PR must be raised via no-mistakes"

// ciCheckContext is the second required context on main: ci.yml's job that
// gathers lint and the test matrix into one stable name.
const ciCheckContext = "CI passed"

// requiredCheckContexts lists every context main's ruleset requires.
var requiredCheckContexts = []string{requiredCheckContext, ciCheckContext}

const (
	gateWorkflowPath    = ".github/workflows/no-mistakes-required.yml"
	releaseWorkflowPath = ".github/workflows/release.yml"
)

// gateScriptPath is release.yml's release check. The required check itself is
// decided by the shared composite action the gate workflow delegates to
// (tested upstream in the no-mistakes repository); release.yml's
// release-pr-gate-status job runs this script to decide whether a PR is
// release-please's own, because GitHub creates no workflow runs on a
// GITHUB_TOKEN PR and nothing else can report the required contexts there.
const gateScriptPath = "./.github/scripts/no-mistakes-gate.sh"

// gateStep is a workflow step under test: its run block and its env: block.
type gateStep struct {
	env map[string]string
	run string
}

type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

// cachedPRLookupInputs are the event-payload facts v1.80.1 must read live
// from one GitHub API response instead of accepting as action inputs.
var cachedPRLookupInputs = []string{
	"pr-body",
	"pr-head-sha",
	"pr-head-ref",
	"pr-author",
	"pr-number",
}

// requireActionPin is the immutable commit the workflow must delegate to. A
// mutable ref such as @main would let the pull request under judgement rewrite
// its own judge; bumping this is a separate, deliberate pull request.
const requireActionPin = "kunchenguid/no-mistakes/.github/actions/require-no-mistakes@f6441c96c352a18b9cadcaef6b6c7017e9ac3970"

// The required check on every ordinary pull request comes from the pinned
// shared action, with the bot exemptions as its input and the job always
// running. The gate script no longer exempts bots, so this workflow is the
// only place those exemptions live.
func TestGateWorkflowDelegatesToPinnedAction(t *testing.T) {
	data, err := os.ReadFile(gateWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", gateWorkflowPath, err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", gateWorkflowPath, err)
	}

	for jobID, job := range wf.Jobs {
		if job.Name != requiredCheckContext {
			continue
		}
		// This workflow backs a REQUIRED status check, and a skipped job never
		// reports the context, so the PR would block on a status that can never
		// arrive. Exemptions must therefore ride as action inputs and keep the
		// job running, never as a job-level `if:`.
		if strings.TrimSpace(job.If) != "" {
			t.Fatalf("job %q must not carry a job-level if:, a skipped job never reports the required check", jobID)
		}
		var step *workflowStep
		for i := range job.Steps {
			if job.Steps[i].Uses == requireActionPin {
				step = &job.Steps[i]
				break
			}
		}
		if step == nil {
			t.Fatalf("job %q does not use the pinned shared action %q", jobID, requireActionPin)
		}
		exemptAuthors := make(map[string]struct{})
		for _, entry := range strings.FieldsFunc(step.With["exempt-authors"], func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r'
		}) {
			exemptAuthors[strings.TrimSpace(entry)] = struct{}{}
		}
		for _, login := range []string{"github-actions[bot]", "dependabot[bot]"} {
			if _, ok := exemptAuthors[login]; !ok {
				t.Errorf("job %q must exempt %q via the action's exempt-authors input", jobID, login)
			}
		}
		return
	}

	t.Fatalf("%s has no job named %q", gateWorkflowPath, requiredCheckContext)
}

// pullRequest is what the gate script can see of a pull request: the
// variables release.yml's release-pr-gate-status job exports from the pull
// request's API reading. author and headSHA are exported too, though the
// script must ignore them, so a case can prove neither one passes a PR.
type pullRequest struct {
	number   int
	body     string
	author   string
	headRef  string
	headRepo string
	headSHA  string
	baseRepo string
}

// runGate executes the gate script from the repository root, as release.yml
// does, and reports whether it accepts the pull request.
func runGate(t *testing.T, pr pullRequest) (bool, string) {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; the gate script runs on ubuntu-latest")
	}

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	cmd := testrun.Command(t, bash, gateScriptPath)
	cmd.Dir = root
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PR_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"PR_NUMBER="+fmt.Sprint(pr.number),
		"PR_BODY="+pr.body,
		"PR_AUTHOR="+pr.author,
		"PR_HEAD_REF="+pr.headRef,
		"PR_HEAD_REPO="+pr.headRepo,
		"PR_HEAD_SHA="+pr.headSHA,
		"PR_BASE_REPO="+pr.baseRepo,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, string(output)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, string(output)
	}
	t.Fatalf("run gate script: %v (%s)", err, output)
	return false, ""
}

const (
	noMistakesSignature = "## Pipeline\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n"
	// attestationHeadSHA is the compact v1 comment's head_sha.
	attestationHeadSHA = "0123456789abcdef0123456789abcdef01234567"
	// completedAttestation matches the compact v1 comment no-mistakes >= 1.46.0 writes.
	completedAttestation = `<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"0123456789abcdef0123456789abcdef01234567","steps":[{"step":"intent","status":"completed"},{"step":"rebase","status":"completed"},{"step":"review","status":"completed"},{"step":"test","status":"completed"},{"step":"document","status":"completed"},{"step":"lint","status":"completed"},{"step":"push","status":"completed"},{"step":"pr","status":"running"},{"step":"ci","status":"pending"}]} -->`
	noMistakesBody       = noMistakesSignature + "\n" + completedAttestation + "\n"
	releaseBody          = ":robot: I have created a release *beep* *boop*\n---\n\n## [2.1.1](https://example.invalid)\n\n---\n" +
		"This PR was generated with [Release Please](https://github.com/googleapis/release-please). See [documentation](https://github.com/googleapis/release-please#release-please)."
	repo     = "kunchenguid/runner-up"
	forkRepo = "someone-else/runner-up"
)

// release.yml stamps the required statuses only on a pull request this script
// accepts, so it must accept exactly release-please's own release PR: a
// reserved branch prefix, a same-repository head and the Release Please
// footer. Author identity and a no-mistakes attestation must never be enough,
// since the shared action, not this script, judges those (RUU-54 audit, fix 5).
func TestNoMistakesGateDecisions(t *testing.T) {
	cases := []struct {
		name string
		pr   pullRequest
		pass bool
	}{
		{
			// The live shape of a release-please release PR.
			name: "release-please release PR is exempt",
			pr: pullRequest{
				number: 78, body: releaseBody, author: "github-actions[bot]",
				headRef: "release-please--branches--main", headRepo: repo, baseRepo: repo,
			},
			pass: true,
		},
		{
			// Structural, not identity: the same PR under a PAT identity, which
			// is what release-please would become if it ever used one.
			name: "release-please release PR authored by a human identity is exempt",
			pr: pullRequest{
				number: 79, body: releaseBody, author: "kunchenguid",
				headRef: "release-please--branches--main", headRepo: repo, baseRepo: repo,
			},
			pass: true,
		},
		{
			name: "legacy release-please branch prefix is exempt",
			pr: pullRequest{
				number: 80, body: releaseBody, author: "kunchenguid",
				headRef: "release-please/branches/main", headRepo: repo, baseRepo: repo,
			},
			pass: true,
		},
		{
			name: "human PR without the signature fails",
			pr: pullRequest{
				number: 84, body: "## Summary\n\nA hand-written pull request.", author: "kunchenguid",
				headRef: "fix/something", headRepo: repo, baseRepo: repo,
			},
			pass: false,
		},
		{
			name: "empty body fails",
			pr: pullRequest{
				number: 85, body: "", author: "kunchenguid",
				headRef: "fix/something", headRepo: repo, baseRepo: repo,
			},
			pass: false,
		},
		{
			name: "borrowed release branch name alone fails",
			pr: pullRequest{
				number: 86, body: "## Summary\n\nNot a release PR.", author: "kunchenguid",
				headRef: "release-please--branches--main", headRepo: repo, baseRepo: repo,
			},
			pass: false,
		},
		{
			name: "fork copying the release body fails",
			pr: pullRequest{
				number: 87, body: releaseBody, author: "outside-contributor",
				headRef: "release-please--branches--main", headRepo: forkRepo, baseRepo: repo,
			},
			pass: false,
		},
		{
			name: "release body on an ordinary same-repo branch fails",
			pr: pullRequest{
				number: 88, body: releaseBody, author: "kunchenguid",
				headRef: "feat/pretend", headRepo: repo, baseRepo: repo,
			},
			pass: false,
		},
		{
			// The script used to exempt bots; that exemption now lives only in
			// the shared action's exempt-authors input.
			name: "bot author on an ordinary branch fails",
			pr: pullRequest{
				number: 81, body: "chore: routine bot update", author: "github-actions[bot]",
				headRef: "chore/bot", headRepo: repo, baseRepo: repo,
			},
			pass: false,
		},
		{
			// The script used to pass a completed attestation bound to the head.
			// Even with the head exported, it must not stamp a release status.
			name: "completed attestation bound to the head fails",
			pr: pullRequest{
				number: 83, body: noMistakesBody, author: "kunchenguid",
				headRef: "release-please--branches--main", headRepo: repo, headSHA: attestationHeadSHA, baseRepo: repo,
			},
			pass: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, output := runGate(t, tc.pr)
			if got != tc.pass {
				t.Fatalf("gate pass = %v, want %v\n%s", got, tc.pass, output)
			}
		})
	}
}

func loadReleaseGateStatusStep(t *testing.T) gateStep {
	t.Helper()

	data, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflowPath, err)
	}

	job, ok := wf.Jobs["release-pr-gate-status"]
	if !ok {
		t.Fatal("release workflow has no release-pr-gate-status job")
	}
	if job.Permissions["statuses"] != "write" {
		t.Fatalf("release-pr-gate-status statuses permission = %q, want write", job.Permissions["statuses"])
	}
	hasCheckout := false
	var publishStep *gateStep
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			hasCheckout = true
		}
		if step.Name == "Publish the required statuses on the release PR" {
			if strings.TrimSpace(step.Run) == "" {
				t.Fatal("release gate status step has no executable run block")
			}
			publishStep = &gateStep{env: step.Env, run: step.Run}
		}
	}
	if !hasCheckout {
		t.Fatal("release-pr-gate-status must check out the repository before running its local gate script")
	}
	if publishStep != nil {
		return *publishStep
	}

	t.Fatal("release workflow has no gate status publishing step")
	return gateStep{}
}

func shellAssignment(name, value string) string {
	return name + "='" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func runReleaseGateStatusStep(t *testing.T, step gateStep, body string) (string, string) {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; the release workflow runs on ubuntu-latest")
	}

	fakeBin := t.TempDir()
	postLog := filepath.Join(t.TempDir(), "posts.log")
	fakeGH := `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"pulls?state=open"*)
    echo 78
    ;;
  *"pulls/78"*)
    printf '%s\n' "$GH_PULL_ASSIGNMENTS"
    ;;
  *"statuses/"*)
    {
      echo CALL
      printf 'ARG=%s\n' "$@"
    } >> "$GH_POST_LOG"
    ;;
  *)
    echo "unexpected gh invocation: $*" >&2
    exit 64
    ;;
esac
`
	fakeGHPath := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(fakeGHPath, []byte(fakeGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	assignments := []string{
		shellAssignment("PR_NUMBER", "78"),
		shellAssignment("PR_HEAD_REF", "release-please--branches--main"),
		shellAssignment("PR_HEAD_REPO", repo),
		shellAssignment("PR_BASE_REPO", repo),
		shellAssignment("PR_HEAD_SHA", attestationHeadSHA),
		shellAssignment("PR_BODY", body),
	}
	values := map[string]string{
		"GH_TOKEN":            "test-token",
		"REPO":                repo,
		"RUN_URL":             "https://example.invalid/actions/runs/123",
		"GH_PULL_ASSIGNMENTS": strings.Join(assignments, " "),
		"GH_POST_LOG":         postLog,
		"PATH":                fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for _, name := range []string{"GH_TOKEN", "REPO", "RUN_URL"} {
		if _, ok := step.env[name]; !ok {
			t.Fatalf("release gate status step does not declare required environment input %q", name)
		}
	}
	for name := range step.env {
		if _, ok := values[name]; !ok {
			t.Fatalf("release gate status step declares unhandled environment input %q", name)
		}
	}

	cmd := testrun.Command(t, bash, "-e", "-c", step.run)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := values[name]; !overridden && !strings.HasPrefix(name, "PR_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for name, value := range values {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run release gate status step: %v\n%s", err, output)
	}
	log, err := os.ReadFile(postLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read fake status writes: %v", err)
	}
	return string(output), string(log)
}

func TestReleaseWorkflowGateStatusWiring(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("release status step is an ubuntu-latest bash interface")
	}
	step := loadReleaseGateStatusStep(t)

	// GitHub runs no workflow on a release-please PR, so every required
	// context must be stamped there or the release PR can never merge.
	t.Run("structural release PR stamps every required context", func(t *testing.T) {
		output, posts := runReleaseGateStatusStep(t, step, releaseBody)
		calls := strings.Split(strings.TrimPrefix(posts, "CALL\n"), "CALL\n")
		if len(calls) != len(requiredCheckContexts) {
			t.Fatalf("got %d status writes, want one per required context %v\nrecorded calls:\n%s", len(calls), requiredCheckContexts, posts)
		}
		for i, context := range requiredCheckContexts {
			for _, want := range []string{
				"ARG=--method\nARG=POST",
				"ARG=repos/" + repo + "/statuses/" + attestationHeadSHA,
				"ARG=state=success",
				"ARG=context=" + context + "\n",
				"ARG=target_url=https://example.invalid/actions/runs/123",
			} {
				if !strings.Contains(calls[i], want) {
					t.Fatalf("status write %d missing %q\noutput:\n%s\nrecorded calls:\n%s", i, want, output, posts)
				}
			}
		}
	})

	t.Run("release branch without the Release Please footer is not stamped", func(t *testing.T) {
		output, posts := runReleaseGateStatusStep(t, step, "not a release-please body")
		if posts != "" {
			t.Fatalf("non-structural release-branch PR wrote a status:\n%s", posts)
		}
		if !strings.Contains(output, "not a structural release-please PR") {
			t.Fatalf("missing rejection output:\n%s", output)
		}
	})

	// A completed no-mistakes attestation bound to the release PR's head must
	// not stamp the required contexts: only the structural check may.
	t.Run("attestation cannot widen structural release path", func(t *testing.T) {
		output, posts := runReleaseGateStatusStep(t, step, noMistakesBody)
		if posts != "" {
			t.Fatalf("attestation-only release PR wrote a status:\n%s", posts)
		}
		if !strings.Contains(output, "not a structural release-please PR") {
			t.Fatalf("missing rejection output:\n%s", output)
		}
	})
}

// The required check must always report on a pull request. A path filter would
// silently withhold the status and block the PR forever.
func TestGateWorkflowHasNoPathFilter(t *testing.T) {
	data, err := os.ReadFile(gateWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", gateWorkflowPath, err)
	}
	on, err := parseWorkflowOn(data)
	if err != nil {
		t.Fatalf("parse on: %v", err)
	}
	filter, hasPR, err := pullRequestPathFilter(on)
	if err != nil {
		t.Fatalf("read pull_request filter: %v", err)
	}
	if !hasPR {
		t.Fatal("gate workflow must trigger on pull_request")
	}
	if filter.kind != "none" {
		t.Fatalf("gate workflow must not filter by path, got %s filter %v", filter.kind, filter.patterns)
	}
}

// TestGateWorkflowLiveLookup locks the v1.80.1 caller contract on the shipped
// workflow: the shared action reads the live PR body and head from one API
// response, which fails closed without pull-requests: read. A pin-only
// assertion stayed green if a later edit restored cached PR inputs, restored
// the Fetch current PR body step, or dropped that permission.
func TestGateWorkflowLiveLookup(t *testing.T) {
	data, err := os.ReadFile(gateWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", gateWorkflowPath, err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", gateWorkflowPath, err)
	}
	if got := gateLiveLookupViolations(wf); len(got) > 0 {
		t.Fatalf("live lookup contract broken:\n%s", strings.Join(got, "\n"))
	}
}

func TestGateLiveLookupViolationsRejectStaleCaller(t *testing.T) {
	action := workflowStep{
		Uses: requireActionPin,
		With: map[string]string{"exempt-authors": "github-actions[bot]"},
	}
	readPR := map[string]string{"contents": "read", "pull-requests": "read"}

	t.Run("missing pull-requests permission", func(t *testing.T) {
		wf := liveLookupWorkflow(map[string]string{"contents": "read"}, nil, []workflowStep{action})
		got := strings.Join(gateLiveLookupViolations(wf), "\n")
		if !strings.Contains(got, "pull-requests") {
			t.Fatalf("want a pull-requests permission violation, got %q", got)
		}
	})

	t.Run("job-level permissions drop pull-requests read", func(t *testing.T) {
		wf := liveLookupWorkflow(readPR, map[string]string{"contents": "read"}, []workflowStep{action})
		got := strings.Join(gateLiveLookupViolations(wf), "\n")
		if !strings.Contains(got, "pull-requests") {
			t.Fatalf("want a pull-requests permission violation, got %q", got)
		}
	})

	t.Run("restored fetch step", func(t *testing.T) {
		wf := liveLookupWorkflow(readPR, nil, []workflowStep{
			{Name: "Fetch current PR body", Run: "gh api repos/.../pulls/..."},
			action,
		})
		got := strings.Join(gateLiveLookupViolations(wf), "\n")
		if !strings.Contains(got, "inline") {
			t.Fatalf("want an inline fetch-step violation, got %q", got)
		}
	})

	t.Run("restored cached PR inputs", func(t *testing.T) {
		stale := action
		stale.With = map[string]string{
			"exempt-authors": "github-actions[bot]",
			"pr-body":        "${{ steps.pr-body.outputs.body }}",
			"pr-head-sha":    "${{ github.event.pull_request.head.sha }}",
			"pr-head-ref":    "${{ github.event.pull_request.head.ref }}",
			"pr-author":      "${{ github.event.pull_request.user.login }}",
			"pr-number":      "${{ github.event.pull_request.number }}",
		}
		wf := liveLookupWorkflow(readPR, nil, []workflowStep{stale})
		got := strings.Join(gateLiveLookupViolations(wf), "\n")
		for _, key := range cachedPRLookupInputs {
			if !strings.Contains(got, key) {
				t.Fatalf("want a cached %s input violation, got %q", key, got)
			}
		}
	})

	t.Run("current caller shape is clean", func(t *testing.T) {
		wf := liveLookupWorkflow(readPR, nil, []workflowStep{action})
		if got := gateLiveLookupViolations(wf); len(got) > 0 {
			t.Fatalf("clean caller reported violations:\n%s", strings.Join(got, "\n"))
		}
	})
}

func liveLookupWorkflow(workflowPerm, jobPerm map[string]string, steps []workflowStep) workflowFile {
	return workflowFile{
		Permissions: workflowPerm,
		Jobs: map[string]workflowJob{
			"check": {
				Name:        requiredCheckContext,
				Permissions: jobPerm,
				Steps:       steps,
			},
		},
	}
}

func grantsPullRequestsRead(value string) bool {
	return value == "read" || value == "write"
}

func isRequireNoMistakesAction(uses string) bool {
	return strings.Contains(uses, "/.github/actions/require-no-mistakes@")
}

func gateLiveLookupViolations(wf workflowFile) []string {
	var (
		jobID string
		job   workflowJob
	)
	for id, candidate := range wf.Jobs {
		if candidate.Name != requiredCheckContext {
			continue
		}
		jobID = id
		job = candidate
		break
	}
	if jobID == "" {
		return []string{fmt.Sprintf("no job named %q", requiredCheckContext)}
	}

	var out []string
	perm := ""
	if job.Permissions != nil {
		perm = job.Permissions["pull-requests"]
	} else if wf.Permissions != nil {
		perm = wf.Permissions["pull-requests"]
	}
	if !grantsPullRequestsRead(perm) {
		out = append(out, fmt.Sprintf("job %q effective pull-requests permission %q, want read", jobID, perm))
	}

	for i, step := range job.Steps {
		if strings.TrimSpace(step.Run) != "" {
			out = append(out, fmt.Sprintf("job %q step %d still runs inline commands; live lookup must not fetch the PR body first", jobID, i))
		}
		if !isRequireNoMistakesAction(step.Uses) {
			continue
		}
		for _, key := range cachedPRLookupInputs {
			if _, ok := step.With[key]; ok {
				out = append(out, fmt.Sprintf("job %q passes cached %s into require-no-mistakes; live lookup reads body and head from one response", jobID, key))
			}
		}
	}
	return out
}
