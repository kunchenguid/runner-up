package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kunchenguid/runner-up/internal/quote"
)

// The fork guard is every runner's job-started hook, so a job from another
// repository's pull request runs nothing: the runner fails the job at "Set up
// runner", before any of its steps. runner-up runs it on every provider unless
// allowForkPullRequests is set. It reads only the job's event file, which the
// runner writes from GitHub's payload just before the hook runs; a workflow
// cannot point the hook at another file, since the hook's environment is the
// runner's, not the job's.
//
// Refused:
//   - an event whose pull_request head repository is not its base repository,
//     or has no id (a deleted fork); pull_request, pull_request_target,
//     pull_request_review and pull_request_review_comment events all carry
//     pull_request;
//   - a workflow_run event whose run came from another repository;
//   - no event file, or one that is not a single JSON object.
//
// Every other job is allowed. A job started by a comment names no head
// repository and is allowed.

// The runner runs a hook by its file extension, so the hook it is given is a
// two-line script that runs this program's fork-guard command.
func ForkGuardHook(agent string) string {
	return "#!/bin/bash\nexec " + quote.ShQuote(agent) + " fork-guard\n"
}

// forkGuard decides on the event file at path: nil allows the job, an error
// refuses it and says why.
func forkGuard(path string) error {
	if path == "" {
		return fmt.Errorf("no event file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no event file")
	}
	var event map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&event); err != nil || event == nil {
		return fmt.Errorf("unreadable event file")
	}
	// One JSON object, alone.
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unreadable event file")
	}
	if pr, ok := event["pull_request"]; ok && pr != nil {
		if !sameRepo(lookup(pr, "head", "repo", "id"), lookup(pr, "base", "repo", "id")) {
			return fmt.Errorf("pull request from %s", nameOr(lookup(pr, "head", "repo", "full_name"), "a deleted fork"))
		}
	}
	if run, ok := event["workflow_run"]; ok && run != nil {
		if !sameRepo(lookup(run, "head_repository", "id"), lookup(run, "repository", "id")) {
			return fmt.Errorf("follow-on run of %s", nameOr(lookup(run, "head_repository", "full_name"), "a deleted fork"))
		}
	}
	return nil
}

func lookup(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// sameRepo says whether head and base are the same positive repository id.
func sameRepo(head, base any) bool {
	h, ok1 := head.(json.Number)
	b, ok2 := base.(json.Number)
	if !ok1 || !ok2 {
		return false
	}
	hi, err1 := h.Int64()
	bi, err2 := b.Int64()
	return err1 == nil && err2 == nil && hi > 0 && hi == bi
}

func nameOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

// RunForkGuard is the fork-guard command, as the runner's hook runs it.
func RunForkGuard(stdout, stderr io.Writer) int {
	if err := forkGuard(os.Getenv("GITHUB_EVENT_PATH")); err != nil {
		fmt.Fprintf(stderr, "runner-up refuses this job: %s. Jobs from another repository's pull request never run on these runners.\n", err)
		return 1
	}
	event := os.Getenv("GITHUB_EVENT_NAME")
	if event == "" {
		event = "event"
	}
	fmt.Fprintf(stdout, "runner-up: %s job from this repository; allowed\n", event)
	return 0
}
