package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testbin"
)

// Intent (README, "Pull requests from forks"): every place's runner starts
// the job only after its job-started hook allows it. The hook refuses a job
// from another repository's pull request, its follow-on run, a deleted fork's
// pull request and an event it cannot read, and allows every job from the
// repository itself. The events are GitHub payloads saved from a real fork's
// pull request, renamed (testdata/events).
//
// The rule is plain Go, so these tests run the fork-guard command in this
// process, as the hook's environment gives it the event; one test runs the
// hook a runner is given, to show it reaches the command.

func event(name string) string { return filepath.Join("testdata", "events", name) }

// runForkGuard runs the fork-guard command on the event file at path, named
// as the runner names it.
func runForkGuard(t *testing.T, path, name string) (code int, stdout, stderr string) {
	t.Helper()
	// Under GitHub Actions the test itself runs in a job whose event
	// variables would stand in for a missing one.
	t.Setenv("GITHUB_EVENT_PATH", path)
	t.Setenv("GITHUB_EVENT_NAME", name)
	var out, errOut strings.Builder
	code = RunForkGuard(&out, &errOut)
	return code, out.String(), errOut.String()
}

func TestForkGuardRefusesForeignJobs(t *testing.T) {
	for _, tc := range []struct{ event, why string }{
		{event("pull-request-521-fork.json"), "pull request from a-contributor/tool"},
		{event("pull-request-review-521-fork.json"), "pull request from a-contributor/tool"},
		{event("pull-request-521-deleted-fork.json"), "pull request from a deleted fork"},
		{event("workflow-run-fork.json"), "follow-on run of a-contributor/tool"},
		{event("garbage.txt"), "unreadable event file"},
		{event("array.json"), "unreadable event file"},
		{filepath.Join(t.TempDir(), "missing.json"), "no event file"},
		{"", "no event file"},
	} {
		code, _, stderr := runForkGuard(t, tc.event, "pull_request")
		if code == 0 {
			t.Errorf("%s: the guard allowed the job", tc.event)
			continue
		}
		if !strings.HasPrefix(stderr, "runner-up refuses this job: "+tc.why+".") {
			t.Errorf("%s: the guard refused without saying %q: %s", tc.event, tc.why, stderr)
		}
	}
}

func TestForkGuardAllowsTheRepositorysOwnJobs(t *testing.T) {
	for _, tc := range []struct{ event, name string }{
		{"pull-request-527-same-repo.json", "pull_request"},
		{"workflow-run-same-repo.json", "workflow_run"},
		{"push-main.json", "push"},
		{"release-published.json", "release"},
		{"workflow-dispatch.json", "workflow_dispatch"},
	} {
		code, stdout, stderr := runForkGuard(t, event(tc.event), tc.name)
		if code != 0 {
			t.Errorf("%s: the guard refused a job from this repository: %s", tc.event, stderr)
			continue
		}
		if want := "runner-up: " + tc.name + " job from this repository; allowed\n"; stdout != want {
			t.Errorf("%s: the guard said %q, want %q", tc.event, stdout, want)
		}
	}
}

// A JSON file with a second value after its object answers two ways; the
// guard refuses it rather than read only the first.
func TestForkGuardRefusesTrailingValues(t *testing.T) {
	b, err := os.ReadFile(event("push-main.json"))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := os.ReadFile(event("pull-request-521-fork.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "two.json")
	if err := os.WriteFile(path, append(b, fork...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForkGuard(t, path, "push"); code == 0 || !strings.Contains(stderr, errUnreadableEvent.Error()) {
		t.Fatalf("the guard read only the first of two values: %d %s", code, stderr)
	}
}

// The hook a place gives its runner is a script the runner runs by bash; it
// runs the agent's fork-guard command with the runner's environment, so a
// fork's job fails and the repository's own job starts.
func TestRunnersHookRunsTheForkGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork-guard.sh")
	if err := os.WriteFile(path, []byte(ForkGuardHook(testbin.Build(t, ""))), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(event, name string) (string, string, error) {
		cmd := exec.Command("bash", path)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GITHUB_EVENT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "GITHUB_EVENT_NAME="+name, "GITHUB_EVENT_PATH="+event)
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}
	if _, stderr, err := run(event("pull-request-521-fork.json"), "pull_request"); err == nil ||
		!strings.HasPrefix(stderr, "runner-up refuses this job: pull request from a-contributor/tool.") {
		t.Errorf("the hook let a fork's job start: %v %s", err, stderr)
	}
	if stdout, stderr, err := run(event("pull-request-527-same-repo.json"), "pull_request"); err != nil ||
		stdout != "runner-up: pull_request job from this repository; allowed\n" {
		t.Errorf("the hook refused the repository's own job: %v %s %s", err, stdout, stderr)
	}
}
