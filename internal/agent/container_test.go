//go:build !windows

package agent

// Intent (README, "Linux guests on a Mac"). An Apple container place starts one
// fresh Linux guest per job from its image, sized to the job; the registration
// reaches the guest only on its standard input, after runner-up's guest
// program, so `container inspect` and the image never hold it; the guest runs
// the pinned runner with the fork guard; a stopped place force-deletes its
// guest, and the next job deletes whatever an agent killed outright left.
//
// container is a stand-in that records what it was asked and, for run, the
// guest's standard input, then waits until its guest is deleted.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

const fakeContainer = `#!/bin/bash
d=$CONTAINER_DIR
printf '%s\n' "$1 ${*: -1}" >> "$d/calls"
case $1 in
run)
  printf '%s\0' "$@" > "$d/args"
  env > "$d/env"
  cat > "$d/stdin"
  [ -z "${CONTAINER_FAIL:-}" ] || { echo "Error: notFound: image"; exit 1; }
  echo $$ > "$d/guest.pid"
  echo "runner-up guest: starting the runner"
  until [ -e "$d/deleted" ]; do sleep 0.05; done
  ;;
delete)
  [ -e "$d/guest.pid" ] || { echo "Error: notFound: \"container with ID ${*: -1} not found\""; exit 1; }
  touch "$d/deleted"
  rm "$d/guest.pid"
  ;;
esac
`

func standInContainer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o700)
	os.WriteFile(filepath.Join(dir, "bin", "container"), []byte(fakeContainer), 0o755)
	t.Setenv("CONTAINER_DIR", dir)
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func containerPlace(t *testing.T) *AgentRun {
	return &AgentRun{Provider: config.ProviderContainer, Place: "mini-linux1", Cores: 2, MemoryMB: 8192,
		Image: "runner-up-linux:2026-11-03", StateDir: t.TempDir()}
}

// startContainerGuest starts a container place with the stand-in container
// tool and returns the arguments and the standard input its guest got, and a
// function that stops the place and returns what the place reported.
func startContainerGuest(t *testing.T, dir string, a *AgentRun) (args []string, stdin string, stop func() error) {
	t.Helper()
	if _, ok := runnerSHA256["linux-"+runnerArch()]; !ok {
		t.Skip("no Linux runner release for " + runnerArch())
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- appleContainerProvider{}.run(ctx, a, registration, &out) }()
	waitFor(t, filepath.Join(dir, "guest.pid"))
	b, _ := os.ReadFile(filepath.Join(dir, "args"))
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), string(in), func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the place did not stop")
			return nil
		}
	}
}

func TestContainerGuestGetsTheRegistrationOnlyOnItsInput(t *testing.T) {
	dir := standInContainer(t)
	a := containerPlace(t)
	args, in, stop := startContainerGuest(t, dir, a)

	b, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(b), registration) || strings.Contains(string(env), registration) {
		t.Fatal("the registration is in the guest's arguments or the container tool's environment")
	}
	if strings.Contains(string(b), "\x00-e\x00") || strings.Contains(string(b), "--env") {
		t.Errorf("the guest gets an environment variable: %q", args)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		values[args[i]] = args[i+1]
	}
	for flag, want := range map[string]string{"--name": "runner-up-mini-linux1", "--cpus": "2", "--memory": "8192M",
		"--label": "runner-up.place=mini-linux1", "--user": "0:0", "--entrypoint": "/usr/bin/python3"} {
		if values[flag] != want {
			t.Errorf("container run %s %q, want %q (%q)", flag, values[flag], want, args)
		}
	}
	if len(args) < 3 || args[len(args)-3] != a.Image || args[len(args)-2] != "-c" {
		t.Errorf("the guest does not start a program from the place's image: %q", args)
	}

	// Its input is a length line and that many bytes of program, then the
	// registration on one line.
	size, rest, _ := strings.Cut(in, "\n")
	n, err := strconv.Atoi(size)
	if err != nil || n > len(rest) {
		t.Fatalf("the guest's input starts %q", size)
	}
	if strings.Contains(rest[:n], registration) {
		t.Error("the guest's program holds the registration")
	}
	if rest[n:] != registration+"\n" {
		t.Errorf("after the program the guest reads %q, want the registration", rest[n:])
	}

	// Stopping the place deletes its guest.
	if err := stop(); err != nil {
		t.Errorf("a stopped place reported %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "delete runner-up-mini-linux1") {
		t.Errorf("stopping the place did not delete its guest: %s", calls)
	}
}

// guestHarness runs a container guest's bootstrap, as given to python3, on
// its standard input, as the guest would, with the guest's filesystem under
// a directory and its other edges stood in for: GitHub's download serves a
// file and records what was asked, chown records its arguments, and the
// runner runs as this user and records the user it was to run as.
const guestHarness = `import builtins, importlib.util, json, os, shutil, subprocess, sys, urllib.request
root, served, bootstrap = sys.argv[1:4]
real_open = builtins.open
def record(**kw):
    with real_open(os.path.join(root, "record"), "a") as f:
        f.write(json.dumps(kw) + "\n")
def moved(p):
    if isinstance(p, str) and p.startswith(("/opt/runner-up", "/home/runner", "/etc/environment")):
        return root + p
    return p
def wrap(fn):
    return lambda p, *a, **k: fn(moved(p), *a, **k)
builtins.open = wrap(real_open)
os.makedirs, os.chmod, os.remove, shutil.rmtree = map(wrap, (os.makedirs, os.chmod, os.remove, shutil.rmtree))
def urlopen(url, timeout=None):
    record(download=url)
    return real_open(served, "rb")
urllib.request.urlopen = urlopen
real_run = subprocess.run
def run(args, **kw):
    args = [moved(a) for a in args]
    if args[0] == "chown":
        record(chown=args)
        return subprocess.CompletedProcess(args, 0)
    if "user" in kw:
        record(user=kw.pop("user"), group=kw.pop("group"), extra_groups=kw.pop("extra_groups"))
    kw["cwd"] = moved(kw.get("cwd"))
    return real_run(args, **kw)
subprocess.run = run
def execv(path, argv):
    spec = importlib.util.spec_from_file_location("guest", moved(argv[1]))
    g = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(g)
    sys.argv = argv[1:]
    sys.exit(g.main())
os.execv = execv
os.makedirs(os.path.join(root, "home/runner"))
exec(bootstrap, {"__name__": "__main__"})
`

// standInRunner is a runner release archive whose run.sh writes the
// environment it runs with beside itself.
func standInRunner(t *testing.T, path string) string {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	run := "#!/bin/sh\nenv > runner.env\necho 'Running job: build'\n"
	tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0o755, Size: int64(len(run))})
	tw.Write([]byte(run))
	tw.Close()
	gz.Close()
	os.WriteFile(path, b.Bytes(), 0o600)
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// Intent (README, "Linux guests on a Mac" and "Pull requests from forks"): the
// guest runs GitHub's runner, from GitHub's release of the pinned version, only
// when its download matches the pin; as the user runner, with the registration
// and the job's size in its environment and the fork guard, which refuses
// another repository's pull request, as its job-started hook. The pin is the
// stand-in release's own sum, so a download of anything else must be refused.
func TestContainerGuestRunsThePinnedRunnerWithTheForkGuard(t *testing.T) {
	py := python(t)
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	work := t.TempDir()
	release := filepath.Join(work, "release.tar.gz")
	key := "linux-" + runnerArch()
	pinned, had := runnerSHA256[key]
	runnerSHA256[key] = standInRunner(t, release)
	t.Cleanup(func() {
		if had {
			runnerSHA256[key] = pinned
		} else {
			delete(runnerSHA256, key)
		}
	})
	dir := standInContainer(t)
	args, in, stop := startContainerGuest(t, dir, containerPlace(t))
	stop()
	bootstrap := args[len(args)-1]

	guest := func(served string) (root string, out string, code int) {
		root = t.TempDir()
		cmd := exec.Command(py, "-I", "-c", guestHarness, root, served, bootstrap)
		cmd.Stdin = strings.NewReader(in)
		b, err := cmd.CombinedOutput()
		return root, string(b), ExitCode(err)
	}

	root, out, code := guest(release)
	if code != 0 || !strings.Contains(out, "Running job: build\nrunner-up guest: the runner ended with 0\n") {
		t.Fatalf("the guest ended with %d:\n%s", code, out)
	}
	if strings.Contains(out, registration) {
		t.Errorf("the guest's output holds the registration:\n%s", out)
	}
	recorded, _ := os.ReadFile(filepath.Join(root, "record"))
	var asked []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(recorded)), "\n") {
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		asked = append(asked, m)
	}
	wantURL := "https://github.com/actions/runner/releases/download/v" + runnerVersion +
		"/actions-runner-linux-" + runnerArch() + "-" + runnerVersion + ".tar.gz"
	want := []map[string]any{
		{"download": wantURL},
		{"chown": []any{"chown", "-R", "runner:runner", root + "/home/runner/actions-runner"}},
		{"user": "runner", "group": "runner", "extra_groups": []any{}},
	}
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("the guest asked %v, want %v", asked, want)
	}

	envFile, err := os.ReadFile(filepath.Join(root, "home/runner/actions-runner/runner.env"))
	if err != nil {
		t.Fatal("the runner did not run")
	}
	env := map[string]string{}
	for _, l := range strings.Split(string(envFile), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			env[k] = v
		}
	}
	hook := "/opt/runner-up/fork-guard.sh"
	for k, v := range map[string]string{
		"ACTIONS_RUNNER_INPUT_JITCONFIG":  registration,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED": hook,
		"RUNNER_UP_CORES":                 "2",
		"RUNNER_UP_MEMORY_MB":             "8192",
		"HOME":                            "/home/runner",
		"USER":                            "runner",
	} {
		if env[k] != v {
			t.Errorf("the runner got %s=%q, want %q", k, env[k], v)
		}
	}

	// The hook, run with the guest's filesystem under root, refuses a pull
	// request from a fork and allows one from the repository.
	script, err := os.ReadFile(root + hook)
	if info, statErr := os.Stat(root + hook); err != nil || statErr != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("the guest installed no runnable hook at %s (%v)", hook, err)
	}
	mapped := strings.NewReplacer("/usr/bin/python3", py+" -I", "/opt/runner-up", root+"/opt/runner-up").Replace(string(script))
	for event, wantCode := range map[string]int{"pull-request-521-fork.json": 1, "pull-request-527-same-repo.json": 0} {
		path, _ := filepath.Abs(filepath.Join("testdata/events", event))
		cmd := exec.Command("bash", "-c", mapped)
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_PATH="+path, "GITHUB_EVENT_NAME=pull_request")
		b, err := cmd.CombinedOutput()
		if ExitCode(err) != wantCode {
			t.Errorf("the hook on %s ended with %d, want %d: %s", event, ExitCode(err), wantCode, b)
		}
	}

	// A download that does not match the pin runs no runner.
	other := filepath.Join(work, "other.tar.gz")
	b, _ := os.ReadFile(release)
	os.WriteFile(other, append([]byte("x"), b...), 0o600)
	root, out, code = guest(other)
	if code != 1 || !strings.Contains(out, "runner-up guest: failed: the runner's download does not match its pin") {
		t.Errorf("a guest given a download that does not match its pin ended with %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "home/runner/actions-runner/runner.env")); err == nil {
		t.Error("the guest ran a runner whose download does not match its pin")
	}
}

// A guest the container tool could not start ran no runner: the place says
// so, rather than finishing as though a job ran.
func TestContainerGuestThatNeverStartsFailsThePlace(t *testing.T) {
	standInContainer(t)
	t.Setenv("CONTAINER_FAIL", "1")
	var out strings.Builder
	err := appleContainerProvider{}.run(context.Background(), containerPlace(t), registration, &out)
	if err == nil || !strings.Contains(err.Error(), "no runner") {
		t.Errorf("a guest that never started reported %v (%s)", err, out.String())
	}
}

// Cleaning a place deletes a guest an agent killed outright left, and cleaning
// a place with no guest is no failure.
func TestContainerCleanDeletesALeftoverGuest(t *testing.T) {
	dir := standInContainer(t)
	a := containerPlace(t)
	if err := (appleContainerProvider{}).clean(context.Background(), a); err != nil {
		t.Fatalf("cleaning a place with no guest: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "guest.pid"), []byte("1"), 0o600)
	if err := (appleContainerProvider{}).clean(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deleted")); err != nil {
		t.Error("the leftover guest was not deleted")
	}
}

// The bootstrap takes exactly the program from its input, so the program,
// run in its place, reads the registration as its own next line; the guest
// program, run that way, takes its registration from standard input and,
// without one, fails so the agent knows no runner ran.
func TestContainerBootstrapLeavesTheRegistrationForTheGuest(t *testing.T) {
	py := python(t)
	dir := t.TempDir()
	program := `import os, sys
line = b""
while not line.endswith(b"\n"):
    line += os.read(0, 1)
print(sys.argv[1:], line.decode().strip())
`
	cmd := exec.Command(py, "-I", "-c", containerBootstrap(filepath.Join(dir, "opt")))
	cmd.Stdin = strings.NewReader(strconv.Itoa(len(program)) + "\n" + program + registration + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "['stdin'] "+registration {
		t.Errorf("the program the bootstrap ran said %q (%v)", out, err)
	}

	guest := filepath.Join(dir, "guest.py")
	os.WriteFile(guest, []byte(hypervGuest), 0o600)
	script := `
import importlib.util, os, sys
spec = importlib.util.spec_from_file_location("guest", sys.argv[1])
g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g)
print(g.registration(0, announce=False))
`
	cmd = exec.Command(py, "-I", "-c", script, guest)
	cmd.Stdin = strings.NewReader(registration + "\n")
	out, err = cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != registration {
		t.Errorf("the guest took %q (%v), want the registration", out, err)
	}

	cmd = exec.Command(py, "-I", guest, "stdin")
	out, err = cmd.CombinedOutput()
	if ExitCode(err) != 1 || !strings.Contains(string(out), "runner-up guest: failed: the agent closed its input") {
		t.Errorf("a guest with no registration said %q (%v)", out, err)
	}
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}
