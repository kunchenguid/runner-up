package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// dockerEntry starts the runner inside the container: it reads the
// registration from standard input and gives it only to the runner, in the
// one variable the runner reads it from. The runner removes it from its
// environment before any job starts.
const dockerEntry = `read -r ` + registrationVar + ` && export ` + registrationVar + ` && exec "$@"`

func (p dockerProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	l := p.launch(a)
	hook := filepath.Join(a.Dir(), "fork-guard.sh")
	if l.Hook != "" {
		if err := os.WriteFile(hook, []byte(l.Guard), 0o755); err != nil {
			return err
		}
	}
	cmd := exec.Command("docker", dockerArgs(a, l, hook)...)
	cmd.Stdin = strings.NewReader(jit + "\n")
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

// dockerArgs run the place's container, with the fork guard's hook file,
// written on the machine at hook, mounted where l names it. -i keeps the
// container's standard input open: the registration arrives there, never in
// docker's arguments or environment, so the container's settings (docker
// inspect) never hold it.
func dockerArgs(a *AgentRun, l runnerLaunch, hook string) []string {
	image := a.Image
	if image == "" {
		image = DefaultDockerImage
	}
	args := []string{"run", "--rm", "-i", "--name", containerName(a.Place), "--label", "runner-up.place=" + a.Place,
		"--cpus", strconv.Itoa(a.Cores), "--memory", strconv.Itoa(a.MemoryMB) + "m"}
	if l.Hook != "" {
		args = append(args, "-v", dockerGuard(a.Self)+":"+dockerGuest+"/runner-up:ro", "-v", hook+":"+l.Hook+":ro")
	}
	for _, env := range l.Env {
		args = append(args, "-e", env)
	}
	return append(args, "--entrypoint", "/bin/sh", image, "-c", dockerEntry, "runner-up", "/home/runner/run.sh")
}

// dockerGuard is the runner-up a container runs as its fork guard: this
// program on Linux; elsewhere, such as Docker Desktop's Linux VM on a Mac, the
// Linux build beside it, named as the release names it (runner-up-linux-arm64).
func dockerGuard(self string) string {
	if runtime.GOOS == "linux" {
		return self
	}
	return filepath.Join(filepath.Dir(self), "runner-up-linux-"+runtime.GOARCH)
}

func (dockerProvider) clean(ctx context.Context, a *AgentRun) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", containerName(a.Place)).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %s", FirstLine(string(out), err))
	}
	return nil
}
