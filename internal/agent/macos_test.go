//go:build !windows

package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/testbin"
)

// Intent (the design: "tart VM and host on macOS", "the two-macOS-VM limit
// and the guest reboot watch", "fork PR jobs
// refused by default on every provider" and "a crashed place is cleaned
// up"). The agent runs one job in a fresh tart clone of the place's image,
// sized to the place and seeing only a read-only folder with the runner and
// the fork guard; the runner gets its registration on its input only and the
// guard as its job-started hook; a new boot session in the clone ends the
// job; the clone is deleted whatever happened; the agent waits for a free
// macOS VM before it clones. On the machine itself, the agent runs the runner
// in a folder of its own, ends the runner and everything the job started
// when its input closes, and cleans up what an agent killed outright left.
//
// Intent (a bug moving a fleet to runner-up found: runner-up's predecessor
// kept tools such as Node in a host place's tool cache between jobs, and jobs
// relied on it). A host place keeps its runner's tool cache between jobs, so
// a tool one job downloads is there for the next; a tart place points the
// runner at the tool cache its image was set up with.
//
// The agent is the real program, built with a stand-in runner whose run.sh
// prints what GitHub's runner prints and waits for the test. tart is a
// stand-in that keeps its virtual machines as folders, runs a "VM" as a
// process, and runs `tart exec` here with the guest's home folder and
// shared folder mapped to folders of the test.

// fakeRunSH is the stand-in runner's run.sh. It records what the runner
// would read from its environment, then waits for go, starts a job that
// leaves a process running, and waits for done.
const fakeRunSH = `#!/bin/bash
d=$MAC_DIR
printf '%s\n' "$ACTIONS_RUNNER_INPUT_JITCONFIG" >> "$d/registrations"
printf '%s\n' "${ACTIONS_RUNNER_HOOK_JOB_STARTED:-none}" >> "$d/hooks"
# What the tool cache held when the job began; then the job caches a tool
# there, as setup-node does after a download.
printf '%s %s\n' "${RUNNER_TOOL_CACHE:-none}" "$(ls "${RUNNER_TOOL_CACHE:-/nonexistent}" 2>/dev/null | tr '\n' ,)" >> "$d/toolcaches"
[ -z "${RUNNER_TOOL_CACHE:-}" ] || { mkdir -p "$RUNNER_TOOL_CACHE/node/24.22.1"; touch "$RUNNER_TOOL_CACHE/node/24.22.1/arm64.complete"; }
echo $$ > "$d/runner.pid"
echo "√ Connected to GitHub"
echo "Listening for Jobs"
until [ -e "$d/go" ]; do sleep 0.05; done
echo "Running job: build"
sleep 300 &
echo $! > "$d/job.pid"
until [ -e "$d/done" ]; do sleep 0.05; done
echo "Job build completed with result: Succeeded"
`

// fakeTart keeps each VM as a folder of $MAC_DIR/tart/vms. A running VM is
// its tart run process, recorded in running; a VM's os says what it runs.
const fakeTart = `#!/bin/bash
t=$MAC_DIR/tart
mkdir -p "$t/vms"
printf '%s\n' "$*" >> "$t/tart.log"
cmd=$1; shift
case $cmd in
list)
  sep=""; printf '['
  for v in "$t"/vms/*/; do
    [ -d "$v" ] || continue
    r=false; [ -e "$v/running" ] && r=true
    printf '%s{"Name":"%s","Source":"local","Running":%s,"State":"x"}' "$sep" "$(basename "$v")" "$r"; sep=,
  done
  printf ']\n';;
get) printf '{"OS":"%s"}\n' "$(cat "$t/vms/$1/os")";;
clone) mkdir -p "$t/vms/$2"; echo darwin > "$t/vms/$2/os"; echo "$1" > "$t/vms/$2/image";;
set) n=$1; shift; echo "$*" > "$t/vms/$n/size";;
run)
  for n; do :; done; v=$t/vms/$n
  for a; do case $a in --dir=runner-up:*) d=${a#--dir=runner-up:}; echo "${d%:ro}" > "$v/share";; esac; done
  uuidgen > "$v/boot"
  echo $$ > "$v/running"
  trap 'rm -f "$v/running"; exit 0' TERM INT
  while [ -e "$v/running" ]; do sleep 0.05; done;;
exec)
  [ "$1" = -i ] && shift
  v=$t/vms/$1; shift
  [ -e "$v/running" ] || { echo "the VM is not running" >&2; exit 1; }
  case $1 in
  /usr/bin/true) exit 0;;
  /usr/sbin/sysctl) cat "$v/boot";;
  *)
    mkdir -p "$v/home"
    args=("$@")
    for i in "${!args[@]}"; do
      [ "${args[$i]}" = "/Volumes/My Shared Files/runner-up" ] && args[$i]=$(cat "$v/share")
    done
    HOME=$v/home exec "${args[@]}";;
  esac;;
stop)
  v=$t/vms/${@: -1}
  p=$(cat "$v/running" 2>/dev/null); [ "${p:-0}" -gt 1 ] && kill "$p"
  rm -f "$v/running";;
delete) [ -d "$t/vms/$1" ] || { echo "the specified VM \"$1\" does not exist" >&2; exit 1; }; rm -rf "$t/vms/$1";;
esac
`

// runnerUpWithFakeRunner builds this program with the stand-in runner as the
// runner archive its places use.
func runnerUpWithFakeRunner(t *testing.T) string {
	t.Helper()
	archive := filepath.Join(testbin.Dir(t), "fake-runner.tar.gz")
	if _, err := os.Stat(archive); err != nil {
		if err := writeTarGz(archive, "run.sh", fakeRunSH); err != nil {
			t.Fatal(err)
		}
	}
	return testbin.Build(t, "-X github.com/kunchenguid/runner-up/internal/agent.runnerArchive="+archive)
}

func TestMain(m *testing.M) { testbin.Main(m) }

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func writeTarGz(path, name, body string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	io.WriteString(tw, body)
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// mac is one machine for the agent: a folder holding the agent's state, the
// stand-in tart and what the stand-in runner records.
type mac struct {
	t     *testing.T
	dir   string
	state string
	env   []string
}

func newMac(t *testing.T) *mac {
	if testing.Short() {
		t.Skip("runs the agent as a program")
	}
	// The runner runs its hook's path unquoted, so the agent's folder must
	// have no space; t.TempDir() names folders after the test only.
	dir := t.TempDir()
	m := &mac{t: t, dir: dir, state: filepath.Join(dir, "state")}
	os.MkdirAll(filepath.Join(dir, "bin"), 0o700)
	os.WriteFile(filepath.Join(dir, "bin", "tart"), []byte(fakeTart), 0o755)
	m.env = append(os.Environ(), "MAC_DIR="+dir,
		"PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		for _, name := range []string{"runner.pid", "job.pid"} {
			if pid := m.pid(name); pid > 1 {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		filepath.Walk(filepath.Join(dir, "tart", "vms"), func(path string, info os.FileInfo, err error) error {
			if err == nil && info.Name() == "running" {
				b, _ := os.ReadFile(path)
				if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			return nil
		})
	})
	return m
}

// agent is one `agent run` on the mac, its input held open.
type agent struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	mu   sync.Mutex
	out  strings.Builder
	done chan int
}

func (m *mac) run(jit string, args ...string) *agent {
	m.t.Helper()
	argv := append([]string{"agent", "run", "--state-dir", m.state}, args...)
	a := &agent{cmd: exec.Command(runnerUpWithFakeRunner(m.t), argv...), done: make(chan int, 1)}
	a.cmd.Env = m.env
	in, err := a.cmd.StdinPipe()
	if err != nil {
		m.t.Fatal(err)
	}
	a.in = in
	pr, pw := io.Pipe()
	a.cmd.Stdout, a.cmd.Stderr = pw, pw
	if err := a.cmd.Start(); err != nil {
		m.t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			a.mu.Lock()
			a.out.WriteString(sc.Text() + "\n")
			a.mu.Unlock()
		}
	}()
	go func() { err := a.cmd.Wait(); pw.Close(); a.done <- ExitCode(err) }()
	io.WriteString(in, jit+"\n")
	m.t.Cleanup(func() { a.cmd.Process.Kill() })
	return a
}

func (a *agent) output() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.String()
}

// exit waits for the agent to end and returns its exit code.
func (m *mac) exit(a *agent) int {
	m.t.Helper()
	select {
	case code := <-a.done:
		return code
	case <-time.After(60 * time.Second):
		m.t.Fatalf("the agent did not end within a minute:\n%s", a.output())
		return -1
	}
}

func (m *mac) eventually(what string, a *agent, ok func() bool) {
	m.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			out := ""
			if a != nil {
				out = a.output()
			}
			log, _ := os.ReadFile(filepath.Join(m.dir, "tart", "tart.log"))
			m.t.Fatalf("after 30 seconds: %s\nagent:\n%s\ntart:\n%s", what, out, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *mac) pid(name string) int {
	b, _ := os.ReadFile(filepath.Join(m.dir, name))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func (m *mac) lines(name string) []string {
	b, _ := os.ReadFile(filepath.Join(m.dir, name))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (m *mac) touch(name string) { os.WriteFile(filepath.Join(m.dir, name), nil, 0o600) }

func (m *mac) vm(name string) string { return filepath.Join(m.dir, "tart", "vms", name) }

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// guard runs a job-started hook with a saved event as the runner would, and
// says whether it allowed the job.
func guard(t *testing.T, hook, event string) bool {
	t.Helper()
	cmd := exec.Command("bash", hook)
	cmd.Env = append(os.Environ(), "GITHUB_EVENT_PATH="+filepath.Join("testdata", "events", event))
	return cmd.Run() == nil
}

const sequoia = "ghcr.io/cirruslabs/macos-sequoia-base:latest"

func TestTartPlaceRunsTheJobInAFreshCloneAndDeletesIt(t *testing.T) {
	m := newMac(t)
	jit := "eyJydW5uZXIiOiJydW5uZXItdXAtbWFjLTEifQ-mac-1"
	a := m.run(jit, "--provider", "tart", "--place", "mac-1", "--image", sequoia, "--cores", "4", "--memory", "8192",
		"--boot-timeout", "30", "--guest-check", "1")
	vm := m.vm(CloneName("mac-1"))
	m.eventually("the runner starts in the clone", a, func() bool { return m.pid("runner.pid") > 0 })

	// The clone is of the place's image, sized to the place, and sees only
	// the agent's read-only folder for the place.
	if b, _ := os.ReadFile(filepath.Join(vm, "image")); strings.TrimSpace(string(b)) != sequoia {
		t.Errorf("the clone is of %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "size")); strings.TrimSpace(string(b)) != "--cpu 4 --memory 8192" {
		t.Errorf("the clone was sized %q", b)
	}
	share := filepath.Join(m.state, "places", "mac-1", "share")
	log := m.lines(filepath.Join("tart", "tart.log"))
	wantRun := "run --no-graphics --dir=runner-up:" + share + ":ro " + CloneName("mac-1")
	if !contains(log, wantRun) {
		t.Errorf("tart ran %q, want %q among them", log, wantRun)
	}
	entries, _ := os.ReadDir(share)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "runner-up runner.tar.gz" {
		t.Errorf("the clone sees %v", names)
	}

	// The runner got its registration on its input, and the fork guard as
	// its hook, from the clone's home folder; the guard refuses a fork's
	// pull request there and allows the repository's own.
	if got := m.lines("registrations"); len(got) != 1 || got[0] != jit {
		t.Errorf("the runner got registrations %q", got)
	}
	hook := filepath.Join(vm, "home", ".runner-up", "fork-guard.sh")
	if got := m.lines("hooks"); len(got) != 1 || got[0] != hook {
		t.Errorf("the runner's hook is %q, want %q", got, hook)
	}
	if guard(t, hook, "pull-request-521-fork.json") || !guard(t, hook, "pull-request-527-same-repo.json") {
		t.Error("the clone's fork guard does not decide as the guard does")
	}
	for _, line := range log {
		if strings.Contains(line, jit) {
			t.Errorf("the registration appeared in a tart command line: %q", line)
		}
	}

	// The job runs and ends; the clone is stopped and deleted.
	m.touch("go")
	m.eventually("the job starts", a, func() bool { return strings.Contains(a.output(), "Running job: build") })
	m.touch("done")
	if code := m.exit(a); code != ExitDone {
		t.Fatalf("the agent exited %d:\n%s", code, a.output())
	}
	if exists(vm) || exists(share) {
		t.Errorf("the clone or its folder outlived the job: %v %v", exists(vm), exists(share))
	}
	if !strings.Contains(a.output(), "runner-up agent: place mac-1 finished") {
		t.Errorf("the agent said:\n%s", a.output())
	}
}

func TestTartPlaceEndsTheJobWhenTheClonesMacOSRestarts(t *testing.T) {
	m := newMac(t)
	a := m.run("jit-mac-2", "--provider", "tart", "--place", "mac-2", "--image", sequoia, "--boot-timeout", "30", "--guest-check", "1")
	vm := m.vm(CloneName("mac-2"))
	m.eventually("the runner starts in the clone", a, func() bool { return m.pid("runner.pid") > 0 })
	m.touch("go")
	m.eventually("the job starts", a, func() bool { return strings.Contains(a.output(), "Running job: build") })
	// The clone panics and restarts, with a new boot session. The runner's
	// tart exec goes on waiting, as it did on the predecessor's Mac.
	time.Sleep(1500 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(vm, "boot"), []byte("0F9A2B4C-7D1E-4C2A-8B5F-3E6D7A8B9C0D\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := m.exit(a); code != ExitFailed {
		t.Fatalf("the agent exited %d, want %d:\n%s", code, ExitFailed, a.output())
	}
	if !strings.Contains(a.output(), "the clone's macOS restarted during the job") {
		t.Errorf("the agent said:\n%s", a.output())
	}
	if exists(vm) {
		t.Error("the restarted clone was not deleted")
	}
	if alive(m.pid("runner.pid")) {
		t.Error("the runner's tart exec outlived the restart")
	}
}

func TestTartPlaceWaitsForAFreeMacOSVM(t *testing.T) {
	m := newMac(t)
	// Two VMs run that runner-up did not start: a macOS proof clone and a
	// Linux VM, which does not count. A leftover clone of this same place is
	// cleaned up first and does not count either.
	for name, system := range map[string]string{"sshhip-xcode271-proof": "darwin", "ubuntu": "linux", CloneName("mac-1"): "darwin"} {
		os.MkdirAll(m.vm(name), 0o700)
		os.WriteFile(filepath.Join(m.vm(name), "os"), []byte(system+"\n"), 0o600)
		os.WriteFile(filepath.Join(m.vm(name), "running"), []byte("0\n"), 0o600)
	}
	a := m.run("jit-mac-1", "--provider", "tart", "--place", "mac-1", "--image", sequoia, "--boot-timeout", "30")
	m.eventually("the runner starts beside one other macOS VM", a, func() bool { return m.pid("runner.pid") > 0 })
	m.touch("go")
	m.touch("done")
	if code := m.exit(a); code != ExitDone {
		t.Fatalf("the agent exited %d:\n%s", code, a.output())
	}
	os.Remove(filepath.Join(m.dir, "runner.pid"))
	os.Remove(filepath.Join(m.dir, "go"))
	os.Remove(filepath.Join(m.dir, "done"))

	// A second macOS VM now runs outside runner-up: the agent waits rather
	// than have tart refuse the clone, and clones once one stops.
	os.MkdirAll(m.vm("xcode-26-test"), 0o700)
	os.WriteFile(filepath.Join(m.vm("xcode-26-test"), "os"), []byte("darwin\n"), 0o600)
	os.WriteFile(filepath.Join(m.vm("xcode-26-test"), "running"), []byte("0\n"), 0o600)
	a = m.run("jit-mac-1-again", "--provider", "tart", "--place", "mac-1", "--image", sequoia, "--boot-timeout", "60")
	m.eventually("the agent says it waits", a, func() bool { return strings.Contains(a.output(), "waiting for a macOS VM to stop") })
	time.Sleep(time.Second)
	if exists(m.vm(CloneName("mac-1"))) {
		t.Fatal("the agent cloned while macOS ran two VMs")
	}
	os.Remove(filepath.Join(m.vm("xcode-26-test"), "running"))
	m.eventually("the runner starts once a VM stopped", a, func() bool { return m.pid("runner.pid") > 0 })
	m.touch("go")
	m.touch("done")
	if code := m.exit(a); code != ExitDone {
		t.Fatalf("the agent exited %d:\n%s", code, a.output())
	}
}

func TestHostPlaceRunsOnTheMachineAndEndsEverythingTheJobStarted(t *testing.T) {
	m := newMac(t)
	jit := "eyJydW5uZXIiOiJydW5uZXItdXAtbWFjLWhvc3QifQ-host"
	a := m.run(jit, "--provider", "host", "--place", "mac-host")
	m.eventually("the runner starts", a, func() bool { return m.pid("runner.pid") > 0 })
	runner := filepath.Join(m.state, "places", "mac-host", "host")
	if got := m.lines("registrations"); len(got) != 1 || got[0] != jit {
		t.Errorf("the runner got registrations %q", got)
	}
	hook := filepath.Join(runner, "fork-guard.sh")
	if got := m.lines("hooks"); len(got) != 1 || got[0] != hook {
		t.Errorf("the runner's hook is %q, want %q", got, hook)
	}
	if guard(t, hook, "pull-request-521-fork.json") || !guard(t, hook, "push-main.json") {
		t.Error("the host place's fork guard does not decide as the guard does")
	}
	if args, _ := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(a.cmd.Process.Pid)).Output(); strings.Contains(string(args), jit) {
		t.Errorf("the agent's command line holds the registration: %q", args)
	}

	// The job starts a process of its own; then the coordinator lets go of
	// the agent's input, as when it stops the runner or loses the machine.
	m.touch("go")
	m.eventually("the job starts its process", a, func() bool { return m.pid("job.pid") > 0 })
	runnerPID, jobPID := m.pid("runner.pid"), m.pid("job.pid")
	a.in.Close()
	if code := m.exit(a); code != ExitDone {
		t.Fatalf("the agent exited %d:\n%s", code, a.output())
	}
	if alive(runnerPID) || alive(jobPID) {
		t.Errorf("the runner (%v) or the job's process (%v) outlived the place", alive(runnerPID), alive(jobPID))
	}
	if exists(runner) {
		t.Error("the runner's folder outlived the job")
	}
}

func TestHostPlaceLeftByAKilledAgentIsCleanedUp(t *testing.T) {
	m := newMac(t)
	a := m.run("jit-host", "--provider", "host", "--place", "mac-host")
	m.eventually("the runner starts", a, func() bool { return m.pid("runner.pid") > 0 })
	m.touch("go")
	m.eventually("the job starts its process", a, func() bool { return m.pid("job.pid") > 0 })
	runnerPID, jobPID := m.pid("runner.pid"), m.pid("job.pid")
	// The agent is killed outright: its runner, in a group of its own, runs
	// on until the coordinator's clean, or the place's next agent, removes it.
	a.cmd.Process.Kill()
	m.exit(a)
	time.Sleep(200 * time.Millisecond)
	if !alive(runnerPID) {
		t.Fatal("the runner ended with the agent; this test needs it left behind")
	}
	clean := exec.Command(runnerUpWithFakeRunner(t), "agent", "clean", "--provider", "host", "--place", "mac-host", "--state-dir", m.state)
	clean.Env = m.env
	if out, err := clean.CombinedOutput(); err != nil {
		t.Fatalf("agent clean: %v %s", err, out)
	}
	m.eventually("the runner and the job's process are gone", nil, func() bool { return !alive(runnerPID) && !alive(jobPID) })
	if exists(filepath.Join(m.state, "places", "mac-host", "host")) {
		t.Error("the runner's folder outlived the clean")
	}
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestHostPlaceKeepsItsToolCacheBetweenJobs(t *testing.T) {
	m := newMac(t)
	tools := filepath.Join(m.state, "places", "mac-host", "tool-cache")
	for job := 1; job <= 2; job++ {
		os.Remove(filepath.Join(m.dir, "go"))
		os.Remove(filepath.Join(m.dir, "done"))
		a := m.run("jit-host", "--provider", "host", "--place", "mac-host")
		m.eventually("the runner starts", a, func() bool { return len(m.lines("toolcaches")) == job && m.lines("toolcaches")[0] != "" })
		m.touch("go")
		m.touch("done")
		if code := m.exit(a); code != ExitDone {
			t.Fatalf("job %d: the agent exited %d:\n%s", job, code, a.output())
		}
	}
	// The first job began with an empty cache and cached Node; the second
	// began with Node there.
	want := []string{tools + " ", tools + " node,"}
	if got := m.lines("toolcaches"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the jobs saw tool caches %q, want %q", got, want)
	}
	if !exists(filepath.Join(tools, "node", "24.22.1", "arm64.complete")) {
		t.Error("the tool cache did not outlive the jobs")
	}
}

func TestTartPlaceRunnerUsesTheImagesToolCache(t *testing.T) {
	m := newMac(t)
	// The fake tart runs the guest's commands here, so a folder of the test
	// stands in for the image's tool cache.
	tools := filepath.Join(m.dir, "image-tool-cache")
	a := m.run("jit-mac-3", "--provider", "tart", "--place", "mac-3", "--image", sequoia, "--boot-timeout", "30",
		"--guest-check", "1", "--tool-cache", tools)
	m.eventually("the runner starts in the clone", a, func() bool { return m.pid("runner.pid") > 0 })
	m.touch("go")
	m.touch("done")
	if code := m.exit(a); code != ExitDone {
		t.Fatalf("the agent exited %d:\n%s", code, a.output())
	}
	if got := m.lines("toolcaches"); len(got) != 1 || got[0] != tools {
		t.Errorf("the runner saw tool caches %q, want %q", got, tools)
	}
}
