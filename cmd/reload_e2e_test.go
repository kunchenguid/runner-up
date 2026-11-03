//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (DOP-235's approved plan, piece 1): "serve watches its setup file
// and waits about a second after a change, because a pull rewrites the file.
// It runs check-config and applies pools, places, sizes and order in place,
// without dropping running jobs or open sessions. It keeps the running setup
// when the new one fails."
//
// The coordinator, its place runners and the agent are the real programs, and
// GitHub, ssh and docker the stand-ins of e2e_test.go. The setup file is
// rewritten whole, as a pull or an editor rewrites it.

// logged says whether the running coordinator's log holds text.
func (e *e2e) logged(text string) bool {
	data, _ := os.ReadFile(filepath.Join(e.dir, fmt.Sprintf("coordinator-%d.log", e.served)))
	return strings.Contains(string(data), text)
}

// A pool and a place added to the setup while a job runs take a job at once,
// at the pool's size, and the running job and its scale set's session carry
// on. Dropped from the setup again while the new place's job runs, the pool's
// session closes and that job runs to its end too.
func TestSetupChangeAppliesWhileAJobRuns(t *testing.T) {
	e := newE2E(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, _ := e.container()
	e.startJob(pid)
	e.eventually("the job reaches its runner", func() bool { run, ok := e.placeRun(); return ok && run.Started })
	original := e.cfg

	// The setup gains a pool of 1-core jobs and a place serving it. The
	// machine's places may reserve 3 cores, box-1's job holds 2, so the new
	// place's job fits only at the pool's size, not the place's 2 cores.
	grown, err := config.LoadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	grown.Pools = append(grown.Pools, config.Pool{Label: "ru-small", Cores: 1, MemoryMB: 1024})
	grown.Machines[0].Places = append(grown.Machines[0].Places,
		config.Place{ID: "box-2", Provider: config.ProviderDocker, Pools: []string{"ru-small"}, Cores: 2, MemoryMB: 4096})
	e.cfg = grown
	e.save()
	e.eventually("the coordinator applies the changed setup", func() bool {
		return e.logged(`msg="applied the changed setup" pools=+ru-small places=+box-2`)
	})
	e.eventually("a session opens for the new pool", func() bool { return e.actions.read().opened == 2 })

	e.actions.sendTo("ru-small", scaleset.JobAvailable{JobMessageBase: jobMessage(journeysJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("a container starts in the new place", func() bool { small, _ := e.containerIn("box-2"); return small > 0 })
	small, _ := e.containerIn("box-2")
	wantDocker := "run --rm --name runner-up-box-2 --label runner-up.place=box-2 --cpus 1 --memory 1024m"
	if docker := e.lines(filepath.Join(e.dir, "docker", "docker.log")); !slices.ContainsFunc(docker, func(l string) bool {
		return strings.HasPrefix(l, wantDocker)
	}) {
		t.Errorf("docker ran %q, want a line starting %q", docker, wantDocker)
	}
	if seen := e.actions.read(); seen.closed != 0 || !alive(pid) {
		t.Fatalf("at the setup change %d sessions closed and box-1's job is alive %v, want none closed and the job alive\n%s",
			seen.closed, alive(pid), e.logs())
	}

	// The setup drops the pool and the place while the place's job runs.
	e.startJob(small)
	e.cfg = original
	e.save()
	e.eventually("the coordinator applies the setup without the pool", func() bool {
		return e.logged(`msg="applied the changed setup" pools=-ru-small places=-box-2`)
	})
	e.eventually("the dropped pool's session closes", func() bool { return e.actions.read().closed == 1 })
	if !alive(small) {
		t.Fatalf("box-2's job stopped when the setup dropped its place\n%s", e.logs())
	}
	e.endJob(small)
	e.eventually("the coordinator retires box-2's finished run", func() bool {
		_, err := os.Stat(filepath.Join(e.cfg.PlaceDir("box-2"), "last-place.json"))
		return err == nil
	})

	// box-1's job ran throughout, and its pool's session was never reopened.
	if seen := e.actions.read(); !alive(pid) || seen.opened != 2 {
		t.Fatalf("box-1's job is alive %v and %d sessions opened, want alive and 2", alive(pid), seen.opened)
	}
	e.endJob(pid)
	e.eventually("box-1 frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	if seen := e.actions.read(); seen.closed != 2 || len(seen.stray) != 0 {
		t.Errorf("%d sessions closed, want 2; unknown requests %v", seen.closed, seen.stray)
	}
}

// A setup file that fails check-config is refused, and the coordinator keeps
// its running setup: it places the next job as before, and the job's place
// runner starts from the setup the coordinator runs, not from the refused
// file. Once the file holds the running setup again, the coordinator says so.
func TestRefusedSetupKeepsTheRunningSetup(t *testing.T) {
	e := newE2E(t)
	e.serve()
	e.eventually("the coordinator polls its session", func() bool { return e.actions.read().polls >= 1 })

	// A hand edit misspells the pool the place serves.
	broken, err := config.LoadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	broken.Machines[0].Places[0].Pools = []string{"ru-linx"}
	if err := config.SaveConfig(e.config, broken); err != nil {
		t.Fatal(err)
	}
	e.eventually("the coordinator refuses the setup", func() bool {
		return e.logged(`msg="the setup file was not applied; keeping the running setup" err="place \"box-1\": unknown pool \"ru-linx\""`)
	})

	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, _ := e.container()
	run, _ := e.placeRun()
	args, _ := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(run.PID)).Output()
	if want := "--config " + filepath.Join(e.cfg.StateDir, "setups") + string(filepath.Separator); !strings.Contains(string(args), want) {
		t.Errorf("the place runner runs as %q, want it to read the coordinator's setup under %q", args, want)
	}

	// The edit is put right.
	e.save()
	e.eventually("the coordinator says the file holds its setup again", func() bool {
		return e.logged(`msg="the setup file holds the running setup again"`)
	})
	e.startJob(pid)
	e.endJob(pid)
	e.eventually("the place frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	if seen := e.actions.read(); seen.opened != 1 || len(seen.stray) != 0 {
		t.Errorf("%d sessions opened, want 1; unknown requests %v", seen.opened, seen.stray)
	}
}
