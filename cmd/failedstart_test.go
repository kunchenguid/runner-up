//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/actions/scaleset"
)

// Intent (README, "When a place fails to start", and the fixed bug: a Mac's
// virtual machine network broke, and the coordinator gave the same place the
// same job again every few seconds, registering and removing a runner at
// GitHub each time): when a place's agent cannot start its runner, the place
// runner records it and the coordinator rests the place. Over the passes that
// follow it acquires nothing and registers no runner, the job waits, and
// status says what its setup and state say.
func TestPlaceWhoseContainerCannotStartRests(t *testing.T) {
	e := newE2E(t)
	// docker cannot start the container: its network cannot attach it.
	broken := "docker: Error response from daemon: failed to create endpoint runner-up-box-1 on network bridge: " +
		"failed to add the host (veth3c1f2a7) <=> sandbox (veth9b04e1d) pair interfaces: operation not supported.\n"
	os.WriteFile(filepath.Join(e.dir, "docker", "broken"), []byte(broken), 0o600)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()

	e.eventually("box-1 rests after its runner failed to start", func() bool { return e.saved().FailedStarts["box-1"].Failures == 1 })
	e.eventually("the runner nobody used is removed", func() bool { return slices.Equal(e.actions.read().removed, []int64{1}) })
	e.passes(3)
	if seen := e.actions.read(); len(seen.jits) != 1 || !slices.Equal(seen.acquired, []int64{9101}) {
		t.Fatalf("while box-1 rests the coordinator acquired %v and made %d registrations, want 9101 and one\n%s",
			seen.acquired, len(seen.jits), e.logs())
	}
	if !slices.Equal(e.waiting(), []string{job.JobDisplayName}) {
		t.Errorf("waiting %v, want the job waiting again", e.waiting())
	}
	e.statusAsRead()
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	e.noSecrets(e.actions.read().jits)
}
