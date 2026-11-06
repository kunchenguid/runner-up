package agent

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (AGENTS.md: "Every runner starts with the fork guard as its
// job-started hook unless allowForkPullRequests is set"; the audit before
// sharing runner-up found this wiring written six times in four languages
// and asked for one launch path with one test over every place kind): for
// every provider, what its place is handed - docker's arguments, the host
// runner's environment, the shell a VM or clone runs, the Linux guest
// program, the account job's start script - sets the fork guard as the hook
// exactly when forks are not allowed, and carries the place's size.
func TestEveryPlaceStartsItsRunnerWithTheForkGuardUnlessForksAreAllowed(t *testing.T) {
	handed := map[string]func(a *AgentRun, l runnerLaunch) string{
		config.ProviderDocker: func(a *AgentRun, l runnerLaunch) string {
			return strings.Join(dockerArgs(a, l, filepath.Join(a.Dir(), "fork-guard.sh")), "\n")
		},
		config.ProviderHost: func(a *AgentRun, l runnerLaunch) string {
			return strings.Join(hostEnv(os.Environ(), l, "", filepath.Join(a.Dir(), "tool-cache")), "\n")
		},
		config.ProviderKVM:  func(a *AgentRun, l runnerLaunch) string { return kvmScript(l) },
		config.ProviderTart: func(a *AgentRun, l runnerLaunch) string { return tartScript(l) },
		config.ProviderAccount: func(a *AgentRun, l runnerLaunch) string {
			return string(accountJobFiles(`C:\runner-up\cache\actions-runner-win-x64-2.338.1.zip`, runnerWindowsSHA256["x64"], a.Self,
				JobRequest{Cores: a.Cores, MemoryMB: a.MemoryMB, AllowForks: a.AllowForks})[startFile])
		},
		config.ProviderHyperV:    linuxGuestHanded,
		config.ProviderContainer: linuxGuestHanded,
	}
	for _, name := range config.Providers {
		render, ok := handed[name]
		if !ok {
			t.Errorf("provider %s has no row: say what its place is handed", name)
			continue
		}
		p, err := providerFor(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, allow := range []bool{false, true} {
			a := &AgentRun{Provider: name, Place: "mini-1", Cores: 3, MemoryMB: 6144, AllowForks: allow,
				StateDir: t.TempDir(), Self: "/usr/local/bin/runner-up"}
			l := p.launch(a)
			text := render(a, l)
			if strings.Contains(text, hookVar) == allow {
				t.Errorf("%s, forks allowed %v: the hook is set %v in what the place is handed:\n%s", name, allow, !allow, text)
			}
			if got := slices.Contains(l.Env, hookVar+"="+l.Hook); got == allow {
				t.Errorf("%s, forks allowed %v: the runner's variables %v", name, allow, l.Env)
			}
			if allow && (l.Hook != "" || l.Guard != "") {
				t.Errorf("%s allows forks but still has a fork guard at %q: %q", name, l.Hook, l.Guard)
			}
			// The hook file runs runner-up's fork guard, or the Linux guest
			// program's, which decides as it does.
			if !allow && !strings.Contains(l.Guard, " fork-guard") && !strings.Contains(l.Guard, "guest.py guard") {
				t.Errorf("%s's hook at %q runs no fork guard: %q", name, l.Hook, l.Guard)
			}
			if !allow && !strings.Contains(text, "fork-guard.") {
				t.Errorf("%s's place is not handed its hook file:\n%s", name, text)
			}
			for _, want := range []string{"RUNNER_UP_CORES", "RUNNER_UP_MEMORY_MB", "6144"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s's place is not handed its size (%s):\n%s", name, want, text)
				}
			}
		}
	}
}

func linuxGuestHanded(a *AgentRun, l runnerLaunch) string {
	g, err := linuxGuest("x64", l)
	if err != nil {
		return err.Error()
	}
	return g
}

// Intent (the audit's finding that a new provider meant edits in five or
// more spots, Apple container places among them with no readiness check):
// `agent probe` answers for every provider this machine can run, and a Mac
// answers for Apple container places too, from the container tool's own
// status.
func TestProbeAnswersForEveryProviderOfItsSystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reads a Unix machine's providers")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "container")
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"#!/bin/sh\necho 'status running'\n", "ok"},
		{"#!/bin/sh\necho 'Error: apiserver is not running' >&2\nexit 1\n", "Apple's container service is not running (container system start): Error: apiserver is not running"},
	} {
		os.WriteFile(tool, []byte(tc.status), 0o755)
		pr := probe()
		for _, name := range config.Providers {
			got := pr.Ready(name)
			if slices.Contains(config.WindowsProviders, name) != (got == "") {
				t.Errorf("a %s machine answered %q for %s", runtime.GOOS, got, name)
			}
		}
		want := tc.want
		if runtime.GOOS != "darwin" {
			want = "Apple's container tool runs only on macOS"
		}
		if pr.Container != want {
			t.Errorf("with container system status %q, the probe said %q, want %q", tc.status, pr.Container, want)
		}
	}
	os.Remove(tool)
	if got := probe().Container; runtime.GOOS == "darwin" && !strings.HasPrefix(got, "Apple's container tool is not installed") {
		t.Errorf("without the container tool the probe said %q", got)
	}
}

// A container is Linux even when its machine is a Mac (Docker Desktop), so a
// docker place mounts runner-up for Linux as its fork guard, never a Mac build.
func TestDockerPlaceMountsALinuxForkGuard(t *testing.T) {
	a := &AgentRun{Provider: config.ProviderDocker, Place: "mini-1", Cores: 1, MemoryMB: 2048,
		StateDir: t.TempDir(), Self: filepath.Join("opt", "bin", "runner-up")}
	args := strings.Join(dockerArgs(a, dockerProvider{}.launch(a), "/x/fork-guard.sh"), "\n")
	want := a.Self + ":" + dockerGuest + "/runner-up:ro"
	if runtime.GOOS != "linux" {
		want = filepath.Join("opt", "bin", "runner-up-linux-"+runtime.GOARCH) + ":" + dockerGuest + "/runner-up:ro"
	}
	if !strings.Contains(args, want) {
		t.Errorf("docker args mount no Linux fork guard %q:\n%s", want, args)
	}
}

// Off Linux, a docker place whose Linux build of runner-up is missing beside
// the agent refuses before docker starts, naming the file to put there, rather
// than failing every job at Set up runner.
func TestDockerPlaceOffLinuxNamesAMissingLinuxBuild(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on Linux a docker place mounts the agent itself")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ntouch "+filepath.Join(bin, "ran")+"\n"), 0o755)
	a := &AgentRun{Provider: config.ProviderDocker, Place: "mini-1", Cores: 1, MemoryMB: 2048,
		StateDir: t.TempDir(), Self: filepath.Join(bin, "runner-up")}
	os.MkdirAll(a.Dir(), 0o700)
	want := filepath.Join(bin, "runner-up-linux-"+runtime.GOARCH)
	err := dockerProvider{}.run(t.Context(), a, "jit", io.Discard)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("run said %v, want an error naming %s", err, want)
	}
	if _, err := os.Stat(filepath.Join(bin, "ran")); err == nil {
		t.Error("docker started without the Linux build")
	}
}
