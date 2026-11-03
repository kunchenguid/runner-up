//go:build !windows

package agent

// Intent (DOP-251's approved plan, piece 1: "an Apple container provider ...
// Pass the registration only on the guest's standard input, never with -e or
// in a file"). An Apple container place starts one fresh Linux guest per job
// from its image, sized to the job; the registration reaches the guest only on
// its standard input, after runner-up's guest program, so `container inspect`
// and the image never hold it; the guest runs the pinned runner with the fork
// guard; a stopped place force-deletes its guest, and the next job deletes
// whatever an agent killed outright left.
//
// container is a stand-in that records what it was asked and, for run, the
// guest's standard input, then waits until its guest is deleted.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestContainerGuestGetsTheRegistrationOnlyOnItsInput(t *testing.T) {
	dir := standInContainer(t)
	a := containerPlace(t)
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- appleContainerProvider{}.run(ctx, a, registration, &out) }()
	waitFor(t, filepath.Join(dir, "guest.pid"))

	b, _ := os.ReadFile(filepath.Join(dir, "args"))
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
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
	if values["-c"] != containerBootstrap(containerGuestDir) || args[len(args)-3] != a.Image {
		t.Errorf("the guest does not start runner-up's bootstrap from the place's image: %q", args)
	}

	// Its input is the guest program, filled in for this job, then the
	// registration on one line.
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	size, rest, _ := strings.Cut(string(in), "\n")
	n, err := strconv.Atoi(size)
	if err != nil || n > len(rest) {
		t.Fatalf("the guest's input starts %q", size)
	}
	guest, want := rest[:n], linuxGuestOrFail(t, false, config.Share{Cores: 2, MemoryMB: 8192})
	if guest != want {
		t.Error("the guest's program is not runner-up's guest program for this job")
	}
	if rest[n:] != registration+"\n" {
		t.Errorf("after the program the guest reads %q, want the registration", rest[n:])
	}
	for _, s := range []string{runnerSHA256["linux-"+runnerArch()], `ALLOW_FORKS = "false" == "true"`, `"RUNNER_UP_CORES": "2"`} {
		if !strings.Contains(guest, s) {
			t.Errorf("the guest program lacks %q", s)
		}
	}

	// Stopping the place deletes its guest.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a stopped place reported %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the place did not stop")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "delete runner-up-mini-linux1") {
		t.Errorf("stopping the place did not delete its guest: %s", calls)
	}
}

func linuxGuestOrFail(t *testing.T, allowForks bool, size config.Share) string {
	t.Helper()
	g, err := linuxGuest(runnerArch(), allowForks, size)
	if err != nil {
		t.Skip(err)
	}
	return g
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
