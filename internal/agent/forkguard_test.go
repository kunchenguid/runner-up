package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testbin"
)

// Intent (the design: "fork PR jobs refused by default on every
// provider"): every place's runner starts the job only after its job-started
// hook allows it. The hook refuses a job from another repository's pull
// request, its follow-on run, a deleted fork's pull request and an event it
// cannot read, and allows every job from the repository itself. The events
// are GitHub payloads saved from a real fork's pull request, renamed
// (testdata/events).

// hook writes the job-started hook a place gives its runner and returns a
// runner for it that sets the job's event as the runner does.
func hook(t *testing.T) func(event, name string) (stdout, stderr string, err error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fork-guard.sh")
	if err := os.WriteFile(path, []byte(ForkGuardHook(testbin.Build(t, ""))), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(event, name string) (string, string, error) {
		cmd := exec.Command("bash", path)
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_NAME="+name)
		if event != "" {
			cmd.Env = append(cmd.Env, "GITHUB_EVENT_PATH="+event)
		}
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}
}

func TestForkGuardRefusesForeignJobs(t *testing.T) {
	run := hook(t)
	events := func(name string) string { return filepath.Join("testdata", "events", name) }
	for _, tc := range []struct{ event, why string }{
		{events("pull-request-521-fork.json"), "pull request from a-contributor/tool"},
		{events("pull-request-review-521-fork.json"), "pull request from a-contributor/tool"},
		{events("pull-request-521-deleted-fork.json"), "pull request from a deleted fork"},
		{events("workflow-run-fork.json"), "follow-on run of a-contributor/tool"},
		{events("garbage.txt"), "unreadable event file"},
		{events("array.json"), "unreadable event file"},
		{filepath.Join(t.TempDir(), "missing.json"), "no event file"},
		{"", "no event file"},
	} {
		_, stderr, err := run(tc.event, "pull_request")
		if err == nil {
			t.Errorf("%s: the guard allowed the job", tc.event)
			continue
		}
		if !strings.HasPrefix(stderr, "runner-up refuses this job: "+tc.why+".") {
			t.Errorf("%s: the guard refused without saying %q: %s", tc.event, tc.why, stderr)
		}
	}
}

func TestForkGuardAllowsTheRepositorysOwnJobs(t *testing.T) {
	run := hook(t)
	for _, tc := range []struct{ event, name string }{
		{"pull-request-527-same-repo.json", "pull_request"},
		{"workflow-run-same-repo.json", "workflow_run"},
		{"push-main.json", "push"},
		{"release-published.json", "release"},
		{"workflow-dispatch.json", "workflow_dispatch"},
	} {
		stdout, stderr, err := run(filepath.Join("testdata", "events", tc.event), tc.name)
		if err != nil {
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
	b, err := os.ReadFile(filepath.Join("testdata", "events", "push-main.json"))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := os.ReadFile(filepath.Join("testdata", "events", "pull-request-521-fork.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "two.json")
	os.WriteFile(path, append(b, fork...), 0o600)
	if _, stderr, err := hook(t)(path, "push"); err == nil || !strings.Contains(stderr, "unreadable event file") {
		t.Fatalf("the guard read only the first of two values: %v %s", err, stderr)
	}
}
