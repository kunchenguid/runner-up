//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

// Intent (README, "When a machine is down", and the fixed bug: a Windows
// machine's network dropped for two minutes while its Tailscale updated, and
// its agents powered off and deleted every job's VM as their SSH sessions
// ended, failing all six jobs): a place outlives an SSH connection that drops
// for less than reconnectSeconds, and its place runner reconnects to it.
//
// The dropping ssh runs the agent's command line as the stand-in ssh does,
// but between fifos it can cut. While the file drop exists it cuts them,
// as a lost connection does: the agent's input ends and its output goes
// nowhere, while the agent itself carries on, and ssh exits 255. While the
// file down exists, ssh cannot reach the machine.
const droppingSSH = `#!/bin/bash
printf '%s\n' "$*" >> "$E2E_DIR/ssh.log"
for last; do :; done
if [ -e "$E2E_DIR/down" ]; then echo "ssh: connect to host box port 22: Connection timed out" >&2; exit 255; fi
cd "$HOME"
in=$(mktemp -u "$E2E_DIR/in.XXXXXX"); out=$(mktemp -u "$E2E_DIR/out.XXXXXX")
mkfifo "$in" "$out"
sh -c "$last" <"$in" >"$out" 2>&1 &
agent=$!
exec 3<&0
cat <&3 >"$in" & feed=$!
cat <"$out" & relay=$!
exec 3<&-
while kill -0 $agent 2>/dev/null; do
  if [ -e "$E2E_DIR/drop" ]; then
    kill $feed $relay
    rm -f "$in" "$out"
    exit 255
  fi
  sleep 0.05
done
wait $agent; code=$?
wait $relay
kill $feed 2>/dev/null
rm -f "$in" "$out"
exit $code
`

// newDroppingE2E is an e2e whose ssh can drop, with places kept for
// reconnect seconds after a drop.
func newDroppingE2E(t *testing.T, reconnect int) *e2e {
	e := newE2E(t)
	os.WriteFile(filepath.Join(e.dir, "bin", "ssh"), []byte(droppingSSH), 0o755)
	e.cfg.ReconnectSeconds = reconnect
	e.save()
	return e
}

func (e *e2e) touch(name string)  { os.WriteFile(filepath.Join(e.dir, name), nil, 0o600) }
func (e *e2e) remove(name string) { os.Remove(filepath.Join(e.dir, name)) }

// sshCalls counts the ssh command lines that hold word.
func (e *e2e) sshCalls(word string) int {
	n := 0
	for _, line := range e.lines(filepath.Join(e.dir, "ssh.log")) {
		if strings.Contains(line, word) {
			n++
		}
	}
	return n
}

// dockerRemovals counts the stand-in docker's removals of box-1's container.
func (e *e2e) dockerRemovals() int {
	n := 0
	for _, line := range e.lines(filepath.Join(e.dir, "docker", "docker.log")) {
		if strings.HasPrefix(line, "rm ") && strings.HasSuffix(line, " runner-up-box-1") {
			n++
		}
	}
	return n
}

func TestPlaceOutlivesADroppedConnectionAndItsRunnerReconnects(t *testing.T) {
	e := newDroppingE2E(t, 120)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, agentPID := e.container()
	e.startJob(pid)
	e.eventually("the place runner sees the job start", func() bool { run, ok := e.placeRun(); return ok && run.Started })
	removals := e.dockerRemovals()

	// The connection drops, and for a while the machine cannot be reached.
	e.touch("down")
	e.touch("drop")
	e.eventually("the place runner tries to reconnect while the machine is down", func() bool {
		return e.sshCalls("'agent' 'attach'") >= 2
	})
	if !e.busy() || !alive(pid) || !alive(agentPID) {
		t.Fatalf("the drop ended the place: busy %v, container %v, agent %v\n%s", e.busy(), alive(pid), alive(agentPID), e.logs())
	}
	if got := e.dockerRemovals(); got != removals {
		t.Fatalf("the agent removed the container after the drop\n%s", e.logs())
	}

	// The machine answers again; the place runner reconnects to the same
	// place, whose job carries on and ends there.
	e.remove("drop")
	e.remove("down")
	e.eventually("the place runner reconnects", func() bool {
		return strings.Contains(e.logs(), "runner-up agent: lost the link to the coordinator")
	})
	if !e.busy() || !alive(pid) {
		t.Fatalf("the place did not survive the reconnect: busy %v, container %v\n%s", e.busy(), alive(pid), e.logs())
	}
	e.endJob(pid)
	e.eventually("the place frees", func() bool { return !e.busy() })
	if !strings.Contains(e.logs(), `msg="agent ended" place=box-1 exit=0 job=true`) {
		t.Errorf("the place runner did not see its agent end the job\n%s", e.logs())
	}
	if _, rests := e.saved().Unreachable["box"]; rests {
		t.Errorf("the machine rests after a drop its place runner reconnected through\n%s", e.logs())
	}
	if !strings.Contains(e.logs(), "Job build completed with result: Succeeded") {
		t.Errorf("the place runner did not see the job end\n%s", e.logs())
	}
	if alive(pid) || alive(agentPID) {
		t.Errorf("the container or agent outlived the job: %v %v", alive(pid), alive(agentPID))
	}
	if n := len(e.registrations()); n != 1 {
		t.Errorf("%d containers ran, want the one", n)
	}
	e.noSecrets(e.actions.read().jits)
}

// Intent (README, "When a machine is down"): a place whose machine stays out
// of reach longer than reconnectSeconds is stopped and cleaned up by its
// agent, and its place runner gives up and rests the machine.
func TestPlaceNobodyReconnectsToIsCleanedUp(t *testing.T) {
	e := newDroppingE2E(t, 3)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, agentPID := e.container()
	e.startJob(pid)
	e.eventually("the place runner sees the job start", func() bool { run, ok := e.placeRun(); return ok && run.Started })

	e.touch("down")
	e.touch("drop")
	e.eventually("the agent stops the container nobody reconnected to", func() bool { return !alive(pid) && !alive(agentPID) })
	e.eventually("the place runner gives up", func() bool { return !e.busy() })
	e.eventually("the machine rests", func() bool { _, rests := e.saved().Unreachable["box"]; return rests })
}

// Intent (README, "When a machine is down"): a job that ends while its
// connection is down still ends as a job that ran: the agent cleans up and
// holds the result until the place runner reconnects for it.
func TestJobThatEndsDuringADropIsReportedOnReconnect(t *testing.T) {
	e := newDroppingE2E(t, 120)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, agentPID := e.container()
	e.startJob(pid)
	e.eventually("the place runner sees the job start", func() bool { run, ok := e.placeRun(); return ok && run.Started })

	e.touch("down")
	e.touch("drop")
	e.eventually("the place runner tries to reconnect", func() bool { return e.sshCalls("'agent' 'attach'") >= 1 })
	e.endJob(pid)
	e.eventually("the container ends", func() bool { return !alive(pid) })
	if !e.busy() || !alive(agentPID) {
		t.Fatalf("the place was let go before its result reached the coordinator: busy %v, agent %v\n%s", e.busy(), alive(agentPID), e.logs())
	}

	e.remove("drop")
	e.remove("down")
	e.eventually("the place frees", func() bool { return !e.busy() })
	if !strings.Contains(e.logs(), `msg="agent ended" place=box-1 exit=0 job=true`) ||
		!strings.Contains(e.logs(), "Job build completed with result: Succeeded") {
		t.Errorf("the place runner did not get the job's end on reconnecting\n%s", e.logs())
	}
	if alive(agentPID) {
		t.Error("the agent outlived handing over its result")
	}
	if _, rests := e.saved().Unreachable["box"]; rests {
		t.Errorf("the machine rests after a drop its place runner reconnected through\n%s", e.logs())
	}
}
