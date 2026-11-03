package agent

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/runner-up/internal/quote"
)

// Every place starts its runner the same way, whichever provider makes it and
// whatever language its start script is in (Go, shell, Python or
// PowerShell). Two things are decided here, once, for all of them:
//
//   - The registration reaches the place only on standard input or a VM's
//     serial port, and the runner gets it only in registrationVar, which the
//     runner reads and removes before any job starts. No launch holds it: a
//     runnerLaunch is made without it.
//   - The fork guard is the runner's job-started hook unless the place allows
//     forks' pull requests (AgentRun.AllowForks).
//
// Each provider says only where its place keeps the hook file and what the
// file runs (its launch method), and renders the runnerLaunch into its own
// start: an environment, docker's -e flags, shell exports, the guest
// program's settings, or the account job's start script.

const (
	// registrationVar is the one variable the runner reads its one-time
	// registration from.
	registrationVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	// hookVar is the runner's job-started hook, the fork guard.
	hookVar = "ACTIONS_RUNNER_HOOK_JOB_STARTED"
)

// runnerLaunch is what runner-up hands a place's runner besides its
// registration.
type runnerLaunch struct {
	// Env is runner-up's variables for the runner, NAME=value: the place's
	// size and, unless forks are allowed, hookVar naming Hook.
	Env []string
	// Hook is the fork guard's hook file as the runner sees it, and Guard
	// that file's content. Both are empty when forks are allowed.
	//
	// A path may start with one variable its start script expands: $HOME/ in
	// a tart clone, whose user the agent does not know, or $dir\ in an
	// account job, whose folder the service picks. shellWord and psWord keep
	// it live.
	Hook, Guard string
}

// launch is every place's runnerLaunch: hook is where the place keeps the
// fork guard's hook file, as its runner sees it, and guard that file's
// content.
func (a *AgentRun) launch(hook, guard string) runnerLaunch {
	l := runnerLaunch{Env: a.SizeEnv()}
	if !a.AllowForks {
		l.Hook, l.Guard = hook, guard
		l.Env = append(l.Env, hookVar+"="+hook)
	}
	return l
}

// Where each kind of place keeps the fork guard, and what runs it.

// dockerGuest is where a container sees runner-up and its hook, mounted
// read-only from the machine.
const dockerGuest = "/opt/runner-up"

func (dockerProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(dockerGuest+"/fork-guard.sh", ForkGuardHook(dockerGuest+"/runner-up"))
}

// jobDir is a host place's job folder, removed after the job.
func (hostProvider) jobDir(a *AgentRun) string { return filepath.Join(a.Dir(), "host") }

// A host place's hook sits in its job folder. The runner runs the hook's
// path unquoted, which hostProvider.run checks.
func (p hostProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(filepath.Join(p.jobDir(a), "fork-guard.sh"), ForkGuardHook(a.Self))
}

// kvmGuest is where the agent copies runner-up and its hook into a KVM
// virtual machine, in the home folder of its user runner.
const kvmGuest = "/home/runner/.runner-up"

func (kvmProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(kvmGuest+"/fork-guard.sh", ForkGuardHook(kvmGuest+"/runner-up"))
}

// tartGuest is where a tart clone keeps runner-up and its hook: in its user's
// home folder, not under the shared folder's spaces.
const tartGuest = "$HOME/.runner-up"

func (tartProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(tartGuest+"/fork-guard.sh", ForkGuardHook(tartGuest+"/runner-up"))
}

// A Linux guest - a Hyper-V VM or an Apple container guest - cannot run
// runner-up's own program, so its hook runs the guest program's guard.
const (
	linuxGuestDir   = "/opt/runner-up"
	linuxGuestGuard = "#!/bin/bash\nexec /usr/bin/python3 " + linuxGuestDir + "/guest.py guard\n"
)

func (hypervProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(linuxGuestDir+"/fork-guard.sh", linuxGuestGuard)
}

func (appleContainerProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(linuxGuestDir+"/fork-guard.sh", linuxGuestGuard)
}

// An account job's hook is a PowerShell script in its folder: the runner
// runs a .ps1 hook with pwsh, or powershell without it. The service, not the
// agent, writes it, with a.Self the service's own program.
func (accountProvider) launch(a *AgentRun) runnerLaunch {
	return a.launch(`$dir\`+guardFile, accountGuardScript(a.Self))
}

// Renderings of a runnerLaunch for the start scripts.

// shellWord quotes v for a POSIX shell, keeping a leading $HOME/ for the
// shell to expand.
func shellWord(v string) string {
	if rest, ok := strings.CutPrefix(v, "$HOME/"); ok {
		return `"$HOME"/` + quote.ShQuote(rest)
	}
	return quote.ShQuote(v)
}

// shellLaunch is l as shell commands: they write the hook file, when there is
// one, and export runner-up's variables.
func shellLaunch(l runnerLaunch) string {
	var b strings.Builder
	if l.Hook != "" {
		b.WriteString("mkdir -p " + shellWord(path.Dir(l.Hook)) + "\n")
		b.WriteString("printf '%s' " + quote.ShQuote(l.Guard) + " > " + shellWord(l.Hook) + "\n")
		b.WriteString("chmod 0755 " + shellWord(l.Hook) + "\n")
	}
	for _, kv := range l.Env {
		name, value, _ := strings.Cut(kv, "=")
		b.WriteString("export " + name + "=" + shellWord(value) + "\n")
	}
	return b.String()
}

// hostEnv is the host runner's environment: the agent's own, without any
// runner variable of its own, then this place's. The registration came on
// the agent's standard input; only the runner's environment holds it, in the
// variable the runner reads and removes.
func hostEnv(agentEnv []string, l runnerLaunch, jit, tools string) []string {
	var env []string
	for _, kv := range agentEnv {
		if !strings.HasPrefix(kv, "ACTIONS_RUNNER_") && !strings.HasPrefix(kv, "RUNNER_TOOL_CACHE=") && !strings.HasPrefix(kv, "RUNNER_UP_") {
			env = append(env, kv)
		}
	}
	env = append(env, l.Env...)
	return append(env, registrationVar+"="+jit, "RUNNER_TOOL_CACHE="+tools)
}

// kvmScript starts the runner in a KVM virtual machine, over SSH. The
// registration goes in on the session's input, never in a command line; the
// runner removes the variable from its environment before any job starts.
// Without ICU, .NET runs in its invariant mode.
func kvmScript(l runnerLaunch) string {
	return "read -r " + registrationVar + "; export " + registrationVar + "\n" + shellLaunch(l) +
		"/sbin/ldconfig -p 2>/dev/null | grep -q libicuuc || export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1\n" +
		"exec ./actions-runner/run.sh\n"
}

// psWord is v as a PowerShell expression, keeping a leading $dir\ for the
// script to expand.
func psWord(v string) string {
	if rest, ok := strings.CutPrefix(v, `$dir\`); ok {
		return "(Join-Path $dir " + psQuote(rest) + ")"
	}
	return psQuote(v)
}

// psLaunch is l's variables as PowerShell; the service writes the hook file
// itself.
func psLaunch(l runnerLaunch) string {
	var lines []string
	for _, kv := range l.Env {
		name, value, _ := strings.Cut(kv, "=")
		lines = append(lines, "$env:"+name+" = "+psWord(value))
	}
	return strings.Join(lines, "\n")
}

// guestLaunch is l as the Linux guest program's settings: one JSON object,
// which is also a Python literal since it holds only strings.
func guestLaunch(l runnerLaunch) string {
	env := map[string]string{}
	for _, kv := range l.Env {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	b, _ := json.Marshal(struct {
		Env   map[string]string `json:"env"`
		Hook  string            `json:"hook"`
		Guard string            `json:"guard"`
	}{env, l.Hook, l.Guard})
	return string(b)
}
