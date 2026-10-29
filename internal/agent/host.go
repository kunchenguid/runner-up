//go:build !windows

package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hostProvider runs each job on the machine itself, as the agent's user: the
// runner is unpacked into a folder made for the job, runs in a process group
// of its own, and the group and the folder are removed after the job. Nothing
// else is fresh: the job sees the user's home folder, programs and network,
// and keeps whatever it changes outside its folder. Its cores and memory are
// only its share of the machine's budget. Its tool cache is kept between
// jobs, so a tool an action downloads is downloaded once.
type hostProvider struct{}

func (hostProvider) jobDir(a *AgentRun) string  { return filepath.Join(a.Dir(), "host") }
func (hostProvider) pidFile(a *AgentRun) string { return filepath.Join(a.Dir(), "host.pid") }

func (hostProvider) toolCache(a *AgentRun) string {
	if a.ToolCache != "" {
		return a.ToolCache
	}
	return filepath.Join(a.Dir(), "tool-cache")
}

func (p hostProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	tarball, err := runnerTarball(ctx, filepath.Join(a.StateDir, "cache"))
	if err != nil {
		return err
	}
	dir := p.jobDir(a)
	runner, err := ensureDir(filepath.Join(dir, "runner"))
	if err != nil {
		return err
	}
	if b, err := exec.CommandContext(ctx, "tar", "xzf", tarball, "-C", runner).CombinedOutput(); err != nil {
		return fmt.Errorf("unpacking the runner: %s", FirstLine(string(b), err))
	}
	tools, err := ensureDir(p.toolCache(a))
	if err != nil {
		return err
	}
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit, "RUNNER_TOOL_CACHE=" + tools}
	if !a.AllowForks {
		// The runner runs its job-started hook's path unquoted.
		hook := filepath.Join(dir, "fork-guard.sh")
		if strings.ContainsAny(hook, " \t\n") {
			return fmt.Errorf("the fork guard's path %q has a space, which the runner cannot run: pass the agent a --state-dir without one", hook)
		}
		if err := os.WriteFile(hook, []byte(ForkGuardHook(a.Self)), 0o755); err != nil {
			return err
		}
		env = append(env, "ACTIONS_RUNNER_HOOK_JOB_STARTED="+hook)
	}
	cmd := exec.Command(filepath.Join(runner, "run.sh"))
	cmd.Dir = runner
	// The runner's own variables come only from this place.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ACTIONS_RUNNER_") && !strings.HasPrefix(kv, "RUNNER_TOOL_CACHE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A process the job leaves running keeps the runner's output open; once
	// the runner has ended, that output is not waited for.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the runner: %w", err)
	}
	pgid := cmd.Process.Pid
	os.WriteFile(p.pidFile(a), []byte(strconv.Itoa(pgid)+"\n"), 0o600)
	waitOrStop(ctx, cmd, func() { syscall.Kill(-pgid, syscall.SIGTERM) }, 30*time.Second)
	// Whatever the job left running in the group goes with it. The group's
	// number cannot be reused while any of it lives.
	syscall.Kill(-pgid, syscall.SIGKILL)
	return nil
}

func (p hostProvider) clean(ctx context.Context, a *AgentRun) error {
	if b, err := os.ReadFile(p.pidFile(a)); err == nil {
		if pgid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pgid > 1 {
			killGroup(pgid, p.jobDir(a))
		}
	}
	os.Remove(p.pidFile(a))
	return os.RemoveAll(p.jobDir(a))
}

// killGroup kills process group pgid, left by an agent killed outright, if
// one of its processes runs from dir: the group a runner of this place leads,
// never another group that reused its number.
func killGroup(pgid int, dir string) {
	b, err := exec.Command("ps", "-A", "-ww", "-o", "pgid=", "-o", "command=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if g, _ := strconv.Atoi(f[0]); g == pgid && strings.Contains(line, dir) {
			syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
	}
}

// hostReady says "ok" when this machine can run host places, or why not.
func hostReady() string {
	if msg := runnerReady(); msg != "ok" {
		return msg
	}
	if _, err := exec.LookPath("tar"); err != nil {
		return "tar is not installed"
	}
	return "ok"
}
