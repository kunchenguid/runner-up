//go:build !windows

package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (RUU-15, found planning rented Macs: "When a machine's agent cannot
// be reached (SSH refused or timing out), runner-up stops giving that
// machine's places jobs until it answers again, makes no new runner
// registration in the meantime, and the job it held goes to a healthy
// place"). Without it, a place runner whose SSH was refused ended at once,
// and the coordinator gave the same place the same job again, registering a
// runner with GitHub for each try as fast as SSH refused.
//
// The far machine is reached with the real ssh, at a port of this computer
// nothing listens on; the box is reached through the stand-in ssh.

// closedPort is a port on this computer nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return fmt.Sprint(port)
}

// farSSH is the stand-in ssh, except that it hands ssh to the far machine to
// the real ssh.
func farSSH(realSSH string) string {
	return strings.Replace(fakeSSH, "for last;", `case " $* " in *" far@127.0.0.1 "*) exec '`+realSSH+`' "$@";; esac
for last;`, 1)
}

func TestUnreachableMachineRestsUntilItsAgentAnswers(t *testing.T) {
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("needs ssh")
	}
	e := newE2E(t)
	// The far machine is listed first, so a job goes there while both
	// machines are idle.
	far := config.Machine{Name: "far", SSH: []string{"-F", "/dev/null", "-p", closedPort(t), "far@127.0.0.1"}, Agent: runnerUp(t),
		Places: []config.Place{{ID: "far-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}}
	e.cfg.Machines = append([]config.Machine{far}, e.cfg.Machines...)
	e.save()
	ssh := filepath.Join(e.dir, "bin", "ssh")
	os.WriteFile(ssh, []byte(farSSH(realSSH)), 0o755)

	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()

	// The far machine refuses the first runner's SSH. The job waits again
	// and goes to the box, the healthy place, with a second registration.
	e.eventually("a container starts in box-1", func() bool { pid, _ := e.container(); return pid > 0 })
	e.eventually("the far runner nobody used is removed", func() bool { return slices.Equal(e.actions.read().removed, []int64{1}) })
	seen := e.actions.read()
	if len(seen.jits) != 2 || !slices.Equal(e.registrations(), seen.jits[1:]) {
		t.Fatalf("made %d registrations and box-1 got %v, want two, the second box-1's\n%s", len(seen.jits), e.registrations(), e.logs())
	}
	var refused bool
	for _, line := range e.lines(filepath.Join(e.dir, "ssh.log")) {
		if strings.Contains(line, "-o ConnectTimeout=10 -F /dev/null") && strings.Contains(line, "'agent' 'run'") &&
			strings.Contains(line, "'far-1'") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no ssh to far ran far-1's agent with a connect timeout:\n%s", e.logs())
	}

	// The far machine rests: over several ticks its free place takes no
	// job and nothing more is registered.
	time.Sleep(3 * time.Second)
	if seen := e.actions.read(); len(seen.jits) != 2 || len(seen.acquired) != 2 {
		t.Fatalf("while far rests the coordinator acquired %v and made %d registrations, want 2 of each\n%s",
			seen.acquired, len(seen.jits), e.logs())
	}
	out, err := exec.Command(runnerUp(t), "status", "--config", e.config).Output()
	if err != nil || !strings.Contains(string(out), "place far-1 on far (docker, 2 cores, 4096 MB) resting (agent unreachable since ") {
		t.Errorf("status said %q (%v)", out, err)
	}

	// The coordinator stops while far rests, and far comes back. The
	// coordinator starts again still resting far, asks its agent at once,
	// and, once it answers, far takes the next job.
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	os.WriteFile(ssh, []byte(fakeSSH), 0o755)
	next := job
	next.RunnerRequestID, next.JobDisplayName = 9150, "verify / arm"
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(next, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts in far-1", func() bool {
		_, err := os.Stat(filepath.Join(e.dir, "docker", "runner-up-far-1.pid"))
		return err == nil
	})
	asked := false
	for _, line := range e.lines(filepath.Join(e.dir, "ssh.log")) {
		asked = asked || strings.Contains(line, "far@127.0.0.1 '"+runnerUp(t)+"' 'version'")
	}
	if !asked {
		t.Errorf("the coordinator never asked far's agent its version:\n%s", e.logs())
	}
	if seen := e.actions.read(); len(seen.jits) != 3 || !slices.Equal(seen.acquired, []int64{9101, 9101, 9150}) {
		t.Errorf("acquired %v with %d registrations, want 9101 twice and 9150, with three", seen.acquired, len(seen.jits))
	}
	e.noSecrets(e.actions.read().jits)
}
