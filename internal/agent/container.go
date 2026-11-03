package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// An Apple container place runs its job in a fresh Linux guest on a Mac, made
// with Apple's container tool from the place's image, sized to the job and
// removed after it. Image prerequisites are documented in README.md under
// "What you need". runner-up's Linux guest program (hyperv-guest.py) runs
// in it as root: the agent sends the program on the guest's standard input,
// then the registration on one more line, so neither the image nor
// `container inspect` ever holds the registration. Nothing of the Mac is
// shared with the guest.

// containerGuestDir is where the bootstrap writes the guest program, and
// where the guest keeps its fork guard.
const containerGuestDir = "/opt/runner-up"

// containerBootstrap is the guest's first program, given as an argument: it
// reads a length line and that many bytes of standard input, unbuffered so
// the registration after them stays unread, writes them to dir/guest.py and
// runs that in its place, reading the rest of standard input.
func containerBootstrap(dir string) string {
	return `import os, sys
def take(n):
    b = b""
    while len(b) < n:
        c = os.read(0, n - len(b))
        if not c:
            sys.exit("runner-up guest: no program on input")
        b += c
    return b
size = b""
while not size.endswith(b"\n"):
    size += take(1)
os.makedirs(` + strconv.Quote(dir) + `, exist_ok=True)
path = os.path.join(` + strconv.Quote(dir) + `, "guest.py")
with open(path, "wb") as f:
    f.write(take(int(size)))
os.execv(sys.executable, [sys.executable, path, "stdin"])
`
}

func appleContainerName(place string) string { return "runner-up-" + place }

// appleContainerArgs run the place's guest: no secret is among them.
func appleContainerArgs(a *AgentRun) []string {
	return []string{"run", "--interactive", "--rm", "--init",
		"--name", appleContainerName(a.Place), "--label", "runner-up.place=" + a.Place,
		"--cpus", strconv.Itoa(a.Cores), "--memory", strconv.Itoa(a.MemoryMB) + "M",
		"--user", "0:0", "--entrypoint", "/usr/bin/python3",
		a.Image, "-c", containerBootstrap(containerGuestDir)}
}

type appleContainerProvider struct{}

func (appleContainerProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	if a.Image == "" {
		return errors.New("an Apple container place needs --image, the image its guests start from")
	}
	guest, err := linuxGuest(runnerArch(), a.AllowForks, config.Share{Cores: a.Cores, MemoryMB: a.MemoryMB})
	if err != nil {
		return err
	}
	cmd := exec.Command("container", appleContainerArgs(a)...)
	cmd.Stdin = strings.NewReader(strconv.Itoa(len(guest)) + "\n" + guest + jit + "\n")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container: %w", err)
	}
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		exec.CommandContext(c, "container", "delete", "--force", appleContainerName(a.Place)).Run()
	}
	err = waitOrStop(ctx, cmd, stop, time.Minute)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		// The guest program ends well once its runner ran, so this is the
		// container tool's failure, such as a missing image, or the guest's
		// before the runner started.
		return fmt.Errorf("the guest ran no runner (exit status %d)", ExitCode(err))
	}
	return nil
}

func (appleContainerProvider) clean(ctx context.Context, a *AgentRun) error {
	out, err := exec.CommandContext(ctx, "container", "delete", "--force", appleContainerName(a.Place)).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "not found") {
		return fmt.Errorf("container delete: %s", FirstLine(string(out), err))
	}
	return nil
}
