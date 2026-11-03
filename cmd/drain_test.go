//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
)

// drainBoxes gives the e2e machine three Docker places: box-1 and box-2 for
// ru-linux jobs, and box-3 for ru-linux and ru-large jobs.
func drainBoxes(e *e2e) {
	m := &e.cfg.Machines[0]
	m.Cores, m.MemoryMB = 8, 16384
	m.Places = []config.Place{
		{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
		{ID: "box-2", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096},
		{ID: "box-3", Provider: config.ProviderDocker, Pools: []string{"ru-linux", "ru-large"}, Cores: 2, MemoryMB: 4096},
	}
	e.cfg.Pools = []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}}
	e.save()
}

// drainCLI is a running `runner-up drain`, its output going to a file.
type drainCLI struct {
	cmd *exec.Cmd
	out string
}

func (e *e2e) drain(args ...string) *drainCLI {
	e.t.Helper()
	out := filepath.Join(e.dir, "drain.out")
	f, err := os.Create(out)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(runnerUp(e.t), append([]string{"drain", "--config", e.config}, args...)...)
	cmd.Env, cmd.Stdout, cmd.Stderr = e.env, f, f
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return &drainCLI{cmd, out}
}

func (d *drainCLI) said() string {
	b, _ := os.ReadFile(d.out)
	return string(b)
}

func (d *drainCLI) wait(t *testing.T) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()
	select {
	case err := <-done:
		return agent.ExitCode(err)
	case <-time.After(30 * time.Second):
		t.Fatalf("the drain did not end within 30 seconds; it said:\n%s", d.said())
		return -1
	}
}

func (e *e2e) status() string {
	e.t.Helper()
	out, err := exec.Command(runnerUp(e.t), "status", "--config", e.config).CombinedOutput()
	if err != nil {
		e.t.Fatalf("status failed: %v\n%s", err, out)
	}
	return string(out)
}

func (e *e2e) runIn(id string) (coordinator.PlaceRun, bool) {
	var run coordinator.PlaceRun
	err := config.ReadJSON(filepath.Join(e.cfg.PlaceDir(id), "place.json"), &run)
	return run, err == nil
}

// statusSays checks status prints each line in want.
func (e *e2e) statusSays(want ...string) string {
	e.t.Helper()
	out := e.status()
	lines := strings.Split(out, "\n")
	for _, w := range want {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, w) }) {
			e.t.Errorf("status said:\n%s\nwant a line starting %q", out, w)
		}
	}
	return out
}

// Intent ("drain new job placement, let running jobs
// finish, do the blocked step, then resume placement right away"; its goal:
// "without stopping the coordinator or affecting other places"): with a
// real coordinator serving, `runner-up drain` keeps two places from new
// jobs while the job running there finishes, admits only the named pair
// there, and exits 0 once the pair has run, the places then free; a place
// it does not name takes a job meanwhile, and status shows the drain
// throughout.
func TestDrainCommandAdmitsThePairOnlyOnceItsPlacesAreQuiet(t *testing.T) {
	e := newE2E(t)
	drainBoxes(e)
	// PR #186's affected-journeys runs in box-1 before the drain.
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(recordedJob(t, 4, 0), scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("PR #186's job starts in box-1", func() bool { pid, _ := e.containerIn("box-1"); return pid > 0 })
	before, _ := e.containerIn("box-1")
	e.startJobNamed(before, "affected-journeys")
	e.eventually("box-1's runner takes the job", func() bool { run, ok := e.runIn("box-1"); return ok && run.Started })

	d := e.drain("--place", "box-1", "--place", "box-2", "--repo", "octo-org/app",
		"--job", "verify / linux", "--job", "affected-journeys", "--label", "release check", "--minutes", "5")
	e.eventually("the drain waits for box-1's job", func() bool {
		return strings.Contains(d.said(), `drain: draining: box-1 runs octo-org/app "affected-journeys"`)
	})

	// The pair arrives with the dry-run publish, which only box-3 takes:
	// the drained box-2 and box-3 are free, yet the pair waits.
	e.actions.sendTo("ru-large", scaleset.JobAvailable{JobMessageBase: jobMessage(recordedJob(t, 1, 0), scaleset.MessageTypeJobAvailable)})
	e.actions.send(
		scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)},
		scaleset.JobAvailable{JobMessageBase: jobMessage(journeysJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("box-3 takes the publish", func() bool { pid, _ := e.containerIn("box-3"); return pid > 0 })
	e.eventually("the pair waits", func() bool {
		return strings.Contains(e.status(), `waiting octo-org/app ru-linux "affected-journeys"`)
	})
	t.Logf("runner-up status while draining:\n%s", e.statusSays(
		`place box-1 on box (docker, 2 cores, 4096 MB) busy (job): octo-org/app "affected-journeys" running`,
		`place box-2 on box (docker, 2 cores, 4096 MB) drained (for "release check")`,
		`place box-3 on box (docker, 2 cores, 4096 MB) busy (job): octo-org/app "Build and publish (a dry run publishes nothing)"`,
		`drain "release check" of box-1, box-2 for octo-org/app "verify / linux", "affected-journeys" since `,
		`waiting octo-org/app ru-linux "verify / linux"`,
		`waiting octo-org/app ru-linux "affected-journeys"`))
	if pid, _ := e.containerIn("box-2"); pid > 0 {
		t.Fatal("the drained box-2 took a job before box-1's job finished")
	}

	// box-1's job finishes: the pair takes box-1 and box-2.
	e.endJob(before)
	pair := map[string]int{}
	e.eventually("the pair starts in box-1 and box-2", func() bool {
		for _, id := range []string{"box-1", "box-2"} {
			run, ok := e.runIn(id)
			pid, _ := e.containerIn(id)
			if !ok || pid == 0 || pid == before {
				return false
			}
			pair[run.JobName] = pid
		}
		return len(pair) == 2
	})
	if pair["verify / linux"] == 0 || pair["affected-journeys"] == 0 {
		t.Fatalf("box-1 and box-2 run %v, want the pair", pair)
	}
	for name, pid := range pair {
		e.startJobNamed(pid, name)
	}
	e.eventually("the drain sees the pair run", func() bool { return strings.Contains(d.said(), "drain: running: ") })
	t.Logf("runner-up status while the pair runs:\n%s", e.statusSays(
		`drain "release check" of box-1, box-2 for octo-org/app "verify / linux", "affected-journeys" since `))

	for _, pid := range pair {
		e.endJob(pid)
	}
	if code := d.wait(t); code != 0 {
		t.Fatalf("the drain exited %d, want 0; it said:\n%s", code, d.said())
	}
	said := d.said()
	t.Logf("runner-up drain said:\n%s", said)
	if !strings.Contains(said, `"verify / linux" ran in box-`) || !strings.Contains(said, `"affected-journeys" ran in box-`) ||
		!strings.HasSuffix(said, "; placement resumed\n") {
		t.Errorf("the drain said:\n%s\nwant both jobs ran and placement resumed", said)
	}
	e.eventually("box-1 and box-2 are free, no longer drained", func() bool {
		out := e.status()
		return strings.Contains(out, "place box-1 on box (docker, 2 cores, 4096 MB) free\n") &&
			strings.Contains(out, "place box-2 on box (docker, 2 cores, 4096 MB) free\n") && !strings.Contains(out, "drain ")
	})
	t.Logf("runner-up status after the drain:\n%s", e.status())
	if pid, _ := e.containerIn("box-3"); !alive(pid) {
		t.Error("box-3's job did not carry on through the drain")
	}
}

// Intent ("then resume placement right away"; README: "Stopping
// runner-up drain ... ends it early"): stopping the drain mid-way exits 130,
// gives its places back at once to a job it held, and the job running in a
// drained place carries on.
func TestStoppingTheDrainResumesPlacementAndKeepsRunningJobs(t *testing.T) {
	e := newE2E(t)
	drainBoxes(e)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("verify / linux starts in box-1", func() bool { pid, _ := e.containerIn("box-1"); return pid > 0 })
	running, _ := e.containerIn("box-1")
	e.startJobNamed(running, "verify / linux")

	d := e.drain("--place", "box-1", "--place", "box-2", "--place", "box-3", "--job", "affected-journeys",
		"--label", "release check", "--minutes", "5")
	e.eventually("the drain waits for box-1's job", func() bool { return strings.Contains(d.said(), "drain: draining: ") })
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(journeysJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("the named job waits", func() bool {
		return strings.Contains(e.status(), `waiting octo-org/app ru-linux "affected-journeys"`)
	})

	d.cmd.Process.Signal(syscall.SIGTERM)
	if code := d.wait(t); code != 130 {
		t.Fatalf("the stopped drain exited %d, want 130; it said:\n%s", code, d.said())
	}
	if !strings.HasSuffix(d.said(), "drain: stopped; placement resumes, and running jobs carry on\n") {
		t.Errorf("the stopped drain said:\n%s", d.said())
	}
	e.eventually("the held job takes a freed place", func() bool { pid, _ := e.containerIn("box-2"); return pid > 0 })
	if !alive(running) {
		t.Error("box-1's job did not carry on after the drain stopped")
	}
	if out := e.status(); strings.Contains(out, "drain") {
		t.Errorf("status still shows the stopped drain:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(e.cfg.StateDir, "drains"))
	if len(entries) != 0 {
		t.Errorf("the stopped drain left %d entries behind", len(entries))
	}
}

// Intent (the drain names places and jobs; a mistyped one must not
// pause anything): the drain refuses a place the setup lacks, a job named
// twice and missing input, exiting 2 before it publishes anything.
func TestDrainRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := &config.Config{StateDir: filepath.Join(dir, "state"), GitHub: config.GitHub{Repos: []string{"octo-org/app"}},
		Pools: []config.Pool{{Label: "ru-linux"}},
		Machines: []config.Machine{{Name: "box", SSH: []string{"ci@box"},
			Places: []config.Place{{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}}}}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--place", "box-1", "--job", "build", "--label", "check"}, "drain needs --place ID --job NAME --label TEXT --minutes N"},
		{[]string{"--place", "box-9", "--job", "build", "--label", "check", "--minutes", "5"}, `no place "box-9" in the setup`},
		{[]string{"--place", "box-1", "--job", "build", "--job", "build", "--label", "check", "--minutes", "5"},
			`job "build" is named twice; each name admits one job`},
	} {
		cmd := exec.Command(runnerUp(t), append([]string{"drain", "--config", path}, c.args...)...)
		out, err := cmd.CombinedOutput()
		if code := agent.ExitCode(err); code != 2 || !strings.Contains(string(out), c.says) {
			t.Errorf("drain %v exited %d saying %q, want 2 saying %q", c.args, code, out, c.says)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "state", "drains")); len(entries) != 0 {
		t.Errorf("a refused drain left %d entries behind", len(entries))
	}
}
