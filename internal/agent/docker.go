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
	"time"
)

// DefaultDockerImage is GitHub's own runner image, pinned to the runner
// version every place uses.
const DefaultDockerImage = "ghcr.io/actions/actions-runner:" + runnerVersion

// dockerProvider runs each job in a new container, removed after the job. The
// container is the place: one runner, one job, nothing kept.
type dockerProvider struct{}

func containerName(place string) string { return "runner-up-" + place }

func (dockerProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	image := a.Image
	if image == "" {
		image = DefaultDockerImage
	}
	args := []string{"run", "--rm", "--name", containerName(a.Place), "--label", "runner-up.place=" + a.Place,
		"--cpus", strconv.Itoa(a.Cores), "--memory", strconv.Itoa(a.MemoryMB) + "m",
		// The runner reads its registration from this variable, named here
		// without a value so docker takes it from its own environment, never
		// from an argument anyone on the machine could read. The runner removes
		// it from its environment before any job starts.
		"-e", "ACTIONS_RUNNER_INPUT_JITCONFIG"}
	if !a.AllowForks {
		hook := filepath.Join(a.Dir(), "fork-guard.sh")
		if err := os.WriteFile(hook, []byte(ForkGuardHook("/opt/runner-up/runner-up")), 0o755); err != nil {
			return err
		}
		args = append(args,
			"-v", a.Self+":/opt/runner-up/runner-up:ro",
			"-v", hook+":/opt/runner-up/fork-guard.sh:ro",
			"-e", "ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/runner-up/fork-guard.sh")
	}
	args = append(args, "--entrypoint", "/home/runner/run.sh", image)
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		exec.CommandContext(c, "docker", "rm", "-f", containerName(a.Place)).Run()
	}
	err := waitOrStop(ctx, cmd, stop, time.Minute)
	if ctx.Err() != nil {
		return nil
	}
	if code := ExitCode(err); code == 125 {
		// docker's own failure, such as a missing image: no runner ran.
		return fmt.Errorf("docker could not start the container")
	}
	return nil
}

func (dockerProvider) clean(ctx context.Context, a *AgentRun) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", containerName(a.Place)).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %s", FirstLine(string(out), err))
	}
	return nil
}
