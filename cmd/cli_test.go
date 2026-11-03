package cmd

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the fix that gave each command its own flags, clean -h exits and
// clear errors): every command has its own flags and shows only those on -h,
// exiting 0; a command refuses arguments it does not take; errors go to
// standard error; a missing setup file points at runner-up init; and the usage
// names every agent command.

// cliRun runs the built runner-up with args and returns what it wrote and its
// exit code.
func cliRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := exec.Command(runnerUp(t), args...)
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), c.ProcessState.ExitCode()
}

// cliSetup writes the setup runner-up init writes, before any machine add.
func cliSetup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := &config.Config{StateDir: filepath.Join(dir, "state"), Pools: []config.Pool{{Label: "runner-up"}},
		GitHub: config.GitHub{Repos: []string{"example/app"}}}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// coordinatorOnly reports whether a command is the coordinator's, which
// runner-up refuses on Windows before reading its flags.
func coordinatorOnly(name string) bool {
	return !slices.Contains([]string{"agent", "fork-guard", "setup", "service", "version"}, strings.Fields(name)[0])
}

// usageCommands are the commands `runner-up help` lists, each agent command
// on its own.
func usageCommands(t *testing.T) []string {
	t.Helper()
	help, _, code := cliRun(t, "help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z-]+(?: [a-z|-]+)?)`).FindAllStringSubmatch(help, -1) {
		words := strings.Fields(m[1])
		switch words[0] {
		case "agent":
			for _, sub := range strings.Split(words[1], "|") {
				names = append(names, "agent "+sub)
			}
		case "machine":
			names = append(names, "machine add")
		default:
			names = append(names, words[0])
		}
	}
	return names
}

func TestUsageNamesEveryAgentCommand(t *testing.T) {
	var listed []string
	for _, name := range usageCommands(t) {
		if sub, ok := strings.CutPrefix(name, "agent "); ok {
			listed = append(listed, sub)
		}
	}
	if !slices.Equal(listed, agent.AgentCommands) {
		t.Errorf("runner-up help lists agent commands %v; the agent takes %v", listed, agent.AgentCommands)
	}
	out, _, code := cliRun(t, "agent", "-h")
	if code != 0 || out != "usage: runner-up agent "+strings.Join(agent.AgentCommands, "|")+"\n" {
		t.Errorf("runner-up agent -h exited %d and said %q", code, out)
	}
}

func TestEveryCommandsHelpExitsZero(t *testing.T) {
	for _, name := range usageCommands(t) {
		if runtime.GOOS == "windows" && coordinatorOnly(name) {
			continue
		}
		_, stderr, code := cliRun(t, append(strings.Fields(name), "-h")...)
		if code != 0 {
			t.Errorf("runner-up %s -h exited %d: %s", name, code, stderr)
		}
	}
}

func TestCommandsShowOnlyTheirOwnFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the coordinator's commands run only on Linux and macOS")
	}
	// Only the coordinator's place and borrow runners take these.
	for _, name := range []string{"serve", "update", "status", "usage", "check-config"} {
		_, stderr, code := cliRun(t, name, "-h")
		if code != 0 || !strings.Contains(stderr, "-config") {
			t.Errorf("runner-up %s -h exited %d and said %q", name, code, stderr)
		}
		for _, flag := range []string{"-borrow", "-place", "-pool"} {
			if strings.Contains(stderr, flag) {
				t.Errorf("runner-up %s -h lists %s, which it does not take: %q", name, flag, stderr)
			}
		}
	}
}

func TestCommandsRefuseArgumentsTheyDoNotTake(t *testing.T) {
	setup := cliSetup(t)
	cases := [][]string{
		{"serve", "--config", setup},
		{"update", "--config", setup},
		{"status", "--config", setup},
		{"usage", "--config", setup},
		{"check-config", "--config", setup},
		{"init", "--config", filepath.Join(t.TempDir(), "new.json"), "--repo", "example/app"},
		{"machine", "add", "box", "--config", setup},
		{"drain", "--config", setup, "--place", "box-1", "--job", "build", "--label", "upgrade", "--minutes", "5"},
		{"setup", "--user", "ci"},
		{"fork-guard"},
		{"service"},
		{"version"},
		{"agent", "run", "--provider", "docker", "--place", "box-1"},
		{"agent", "clean", "--provider", "docker", "--place", "box-1"},
	}
	for _, sub := range agent.AgentCommands {
		if !slices.Contains([]string{"run", "clean", "measure"}, sub) {
			cases = append(cases, []string{"agent", sub})
		}
	}
	for _, args := range cases {
		if runtime.GOOS == "windows" && coordinatorOnly(args[0]) {
			continue
		}
		stdout, stderr, code := cliRun(t, append(args, "stray")...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "stray") {
			t.Errorf("runner-up %s stray exited %d, wrote %q and said %q", strings.Join(args, " "), code, stdout, stderr)
		}
	}
}

func TestMissingSetupPointsAtInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the coordinator's commands run only on Linux and macOS")
	}
	missing := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{
		{"serve", "--config", missing},
		{"status", "--config", missing},
		{"check-config", "--config", missing},
		{"machine", "add", "box", "--config", missing},
		{"drain", "--config", missing, "--place", "box-1", "--job", "build", "--label", "upgrade", "--minutes", "5"},
		{"borrow", "--config", missing, "--pool", "runner-up", "--label", "try", "--minutes", "5", "--", "true"},
	} {
		stdout, stderr, code := cliRun(t, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "runner-up init") {
			t.Errorf("runner-up %s exited %d, wrote %q and said %q", args[0], code, stdout, stderr)
		}
	}
}

func TestUpdateSaysItsErrorOnStandardError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the coordinator's commands run only on Linux and macOS")
	}
	stdout, stderr, code := cliRun(t, "update", "--config", cliSetup(t))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "names no release") {
		t.Errorf("runner-up update with no release exited %d, wrote %q and said %q", code, stdout, stderr)
	}
}
