package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// tartLinuxProvider runs each job in a new tart clone of the place's Linux
// image on a Mac with Apple silicon, deleted after the job. The clone runs
// runner-up's Linux guest program (hyperv-guest.py) as root, as an Apple
// container guest does: the agent sends the program, then the registration
// on one more line, on tart exec's standard input, so neither the image, the
// clone's settings nor any command line ever holds the registration. Nothing
// of the Mac is shared with the clone.
//
// The image needs tart's guest agent, python3, sudo when the guest agent does
// not run as root, and a user and group named runner with /home/runner
// present, as an Apple container place's image does (README.md, "Linux tart
// images"). Linux clones do not count toward macOS's limit of two virtual
// machines, so a tart-linux place never waits for a macOS VM to stop.
type tartLinuxProvider struct{}

// tartLinuxBootID is where Linux keeps the UUID its kernel makes at each boot.
var tartLinuxBootID = []string{"/bin/cat", "/proc/sys/kernel/random/boot_id"}

// tartLinuxStart runs the bootstrap, its first argument, as root: directly
// when the guest agent already runs as root, else through sudo.
const tartLinuxStart = `if [ "$(id -u)" = 0 ]; then exec /usr/bin/python3 -c "$1"; fi
exec sudo -n /usr/bin/python3 -c "$1"`

// tartLinuxArgs are tart exec's arguments for the clone's guest program: no
// secret is among them.
func tartLinuxArgs(vm string) []string {
	return []string{"exec", "-i", vm, "/bin/sh", "-c", tartLinuxStart, "runner-up", containerBootstrap(linuxGuestDir)}
}

func (p tartLinuxProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	if msg := p.ready(); msg != "ok" {
		return fmt.Errorf("%s", msg)
	}
	guest, err := linuxGuest(runnerArch(), p.launch(a))
	if err != nil {
		return err
	}
	vm := CloneName(a.Place)
	c := tartClone{vm: vm, system: "Linux", bootID: tartLinuxBootID}
	cmd := exec.Command("tart", tartLinuxArgs(vm)...)
	cmd.Stdin = strings.NewReader(strconv.Itoa(len(guest)) + "\n" + guest + jit + "\n")
	exit, err := c.runJob(ctx, a, cmd, out)
	if err != nil || ctx.Err() != nil {
		return err
	}
	if exit != nil {
		// The guest program ends well once its runner ran, so this is the
		// clone's failure before the runner started, such as an image
		// without python3 or a user named runner.
		return fmt.Errorf("the clone ran no runner (exit status %d)", ExitCode(exit))
	}
	return nil
}

func (tartLinuxProvider) clean(ctx context.Context, a *AgentRun) error {
	return tartProvider{}.clean(ctx, a)
}

// tartLinuxReady says "ok" when this Mac can run tart-linux places: it can
// run tart places, and GitHub publishes a Linux runner for its architecture.
func tartLinuxReady() string {
	if msg := tartReady(); msg != "ok" {
		return msg
	}
	if _, ok := runnerSHA256["linux-"+runnerArch()]; !ok {
		return "no Linux runner release for " + runnerArch()
	}
	return "ok"
}
