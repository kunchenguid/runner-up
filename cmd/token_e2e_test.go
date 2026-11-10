//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// The stand-in gh prints the token in gh-token, as `gh auth token` prints a
// sign-in's token, or fails as gh does when it has none.
const fakeGh = `#!/bin/sh
printf '%s\n' "$*" >> "$E2E_DIR/gh.log"
if [ ! -s "$E2E_DIR/gh-token" ]; then
  echo "no oauth token found for github.com" >&2
  exit 1
fi
cat "$E2E_DIR/gh-token"
`

// tokenE2E is a setup whose coordinator reads its GitHub token through a
// stand-in gh, and whose stand-in GitHub takes only the current token and
// hands out admin tokens that need renewing a few seconds after they are
// made, as a short-lived GitHub App token behind gh does.
func tokenE2E(t *testing.T) *e2e {
	e := newE2E(t)
	gh := filepath.Join(e.dir, "bin", "gh")
	os.WriteFile(gh, []byte(fakeGh), 0o755)
	e.setGhToken("first")
	e.actions.mu.Lock()
	e.actions.signIn = "first"
	e.actions.adminTTL = 63 * time.Second
	e.actions.mu.Unlock()
	e.env = append(e.env, ghEnv+"="+gh)
	return e
}

func (e *e2e) setGhToken(token string) {
	os.WriteFile(filepath.Join(e.dir, "gh-token"), []byte(token), 0o600)
}

// expireToken has GitHub refuse the token the session was opened with, and
// waits past the admin token's renewal, so the next call renews it.
func (e *e2e) expireToken(valid, gh string) {
	e.actions.mu.Lock()
	e.actions.signIn = valid
	e.actions.mu.Unlock()
	e.setGhToken(gh)
	time.Sleep(5 * time.Second)
}

func (e *e2e) ghCalls() int {
	data, _ := os.ReadFile(filepath.Join(e.dir, "gh.log"))
	return strings.Count(string(data), "auth token --hostname github.com\n")
}

// Issue #55 as reported: a job is acquired while gh's token is good, and the
// token expires before its runner starts. The runner still starts: the
// coordinator reads the token again through gh when GitHub refuses the old
// one, without a restart or a new session.
func TestAcquiredJobStartsAfterTheTokenExpires(t *testing.T) {
	e := tokenE2E(t)
	e.actions.mu.Lock()
	e.actions.onAcquire = func() { e.expireToken("second", "second") }
	e.actions.mu.Unlock()
	e.serve()
	e.eventually("the session opens", func() bool { return e.actions.read().opened == 1 })
	before := e.ghCalls()
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })

	e.actions.mu.Lock()
	refused := e.actions.signInRefused
	e.actions.mu.Unlock()
	seen := e.actions.read()
	t.Logf("GitHub refused %d tokens; gh asked %d more times; sessions %d; acquired %v; registrations %d",
		refused, e.ghCalls()-before, seen.opened, seen.acquired, len(seen.jits))
	if refused < 1 || e.ghCalls() <= before || seen.opened != 1 || len(seen.jits) != 1 {
		t.Fatalf("want >=1 refusal, >=1 more gh read, 1 session, 1 registration\n%s", e.logs())
	}
	if got := e.registrations(); len(got) != 1 || got[0] != seen.jits[0] {
		t.Fatalf("the runner got registration %v, want %v", got, seen.jits)
	}
	logs := e.logs()
	if !strings.Contains(logs, "GitHub refused the session's token; read it again") {
		t.Errorf("the coordinator did not log the renewal\n%s", logs)
	}
	if strings.Contains(logs, "could not start a runner") {
		t.Errorf("a runner start failed\n%s", logs)
	}
	for _, f := range e.filesUnder(e.dir) {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), "second") && !strings.HasSuffix(f, "gh-token") {
			t.Errorf("%s holds the GitHub token", f)
		}
	}
}

// gh's token expires while the session stays open, before any job arrives.
// A job that arrives after that is acquired through the session's own
// client, which GitHub now refuses, so the coordinator ends the session and
// opens a new one with the token read again through gh; the job, still in
// line, starts without a coordinator restart.
func TestJobArrivingAfterTheTokenExpiresStarts(t *testing.T) {
	e := tokenE2E(t)
	e.serve()
	e.eventually("the session opens and polls", func() bool { s := e.actions.read(); return s.opened == 1 && s.polls > 0 })
	before := e.ghCalls()

	e.expireToken("second", "second")
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	// A session ended on an error opens again after 30 seconds.
	deadline := time.Now().Add(75 * time.Second)
	for pid, _ := e.container(); pid == 0; pid, _ = e.container() {
		if time.Now().After(deadline) {
			t.Fatalf("no runner started within 75 seconds of the job arriving\n%s", e.logs())
		}
		time.Sleep(100 * time.Millisecond)
	}

	seen := e.actions.read()
	t.Logf("gh asked %d more times; sessions %d; acquired %v; registrations %d", e.ghCalls()-before, seen.opened, seen.acquired, len(seen.jits))
	if e.ghCalls() <= before || seen.opened != 2 || len(seen.jits) != 1 {
		t.Fatalf("want >=1 more gh read, 2 sessions, 1 registration\n%s", e.logs())
	}
	if got := e.registrations(); len(got) != 1 || got[0] != seen.jits[0] {
		t.Fatalf("the runner got registration %v, want %v", got, seen.jits)
	}
	for _, f := range e.filesUnder(e.dir) {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), "second") && !strings.HasSuffix(f, "gh-token") {
			t.Errorf("%s holds the GitHub token", f)
		}
	}
}

func (e *e2e) filesUnder(dir string) []string {
	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// Adversarial: gh has no sign-in at the moment GitHub refuses the token an
// acquired job's start was to use. No runner starts with the refused token,
// the coordinator says why and gives the job back; once gh signs in again
// and GitHub hands the job to the scale set again, as it does with an
// acquired job no runner took, its runner starts without a coordinator
// restart or a new session.
func TestAcquiredJobWaitsForGhToSignInAgainThenStarts(t *testing.T) {
	e := tokenE2E(t)
	e.actions.mu.Lock()
	e.actions.onAcquire = func() { e.expireToken("third", "") }
	e.actions.mu.Unlock()
	e.serve()
	e.eventually("the session opens", func() bool { return e.actions.read().opened == 1 })
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("the coordinator reports why the runner could not start", func() bool {
		return strings.Contains(e.logs(), "no oauth token found for github.com")
	})
	if pid, _ := e.container(); pid != 0 {
		t.Fatalf("a container started with no valid token\n%s", e.logs())
	}
	if n := len(e.actions.read().jits); n != 0 {
		t.Fatalf("%d registrations handed out with a refused token", n)
	}

	e.setGhToken("third")
	// The place rests a minute after the failed start.
	time.Sleep(61 * time.Second)
	e.actions.send(scaleset.JobAssigned{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAssigned)})
	deadline := time.Now().Add(60 * time.Second)
	for pid, _ := e.container(); pid == 0; pid, _ = e.container() {
		if time.Now().After(deadline) {
			t.Fatalf("no runner started within 60 seconds of gh signing in again\n%s", e.logs())
		}
		time.Sleep(100 * time.Millisecond)
	}
	seen := e.actions.read()
	t.Logf("sessions %d; acquired %v; registrations %d", seen.opened, seen.acquired, len(seen.jits))
	if seen.opened != 1 || len(seen.jits) != 1 {
		t.Errorf("%d sessions, %d registrations; want the first session kept and one registration\n%s", seen.opened, len(seen.jits), e.logs())
	}
}
