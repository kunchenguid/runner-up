package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tartProvider runs each job in a new tart clone of the place's macOS image,
// deleted after the job. The clone sees one folder of its Mac, read-only: the
// runner's release archive and, with the fork guard on, this program. Its
// guest agent runs the runner with `tart exec`, which hands it the one-time
// registration on its input, never in a command line. tart is the program its
// makers publish; runner-up only runs it.
//
// Apple's Virtualization framework runs at most two macOS virtual machines at
// once on a Mac. The coordinator counts its own clones and the macOS VMs it
// did not start (internal/coordinator), and the agent waits for a free one
// before it clones, in case a VM started since the coordinator last looked.
type tartProvider struct{}

// MacOSVMLimit is the Virtualization framework's limit on macOS virtual
// machines running at once on one Mac.
const MacOSVMLimit = 2

// CloneName names a tart place's clone.
func CloneName(place string) string { return "runner-up-" + place }

// The folder of the Mac the clone sees, by the name tart mounts it under in
// the guest.
const (
	tartShareName  = "runner-up"
	tartGuestShare = "/Volumes/My Shared Files/" + tartShareName
)

// tartScript runs in the clone as its guest agent's user. Its input's first
// line is the registration. It copies the runner, and with the fork guard on
// this program, out of the shared folder, which macOS mounts once the clone's
// user has signed in, and starts the runner with the guard as its
// job-started hook. The runner runs the hook's path unquoted, so the guard
// lives in the home folder, never under the shared folder's spaces. A second
// argument is the place's tool cache in the clone.
const tartScript = `set -eu
IFS= read -r jit
share=$1
if [ -n "${2:-}" ]; then export RUNNER_TOOL_CACHE="$2"; fi
i=0
until [ -r "$share/runner.tar.gz" ]; do
  i=$((i+1)); [ "$i" -le 120 ] || { echo "the shared folder never appeared in the clone" >&2; exit 1; }
  sleep 1
done
rm -rf "$HOME/actions-runner" "$HOME/.runner-up"
mkdir -p "$HOME/actions-runner"
tar xzf "$share/runner.tar.gz" -C "$HOME/actions-runner"
if [ -e "$share/runner-up" ]; then
  mkdir -p "$HOME/.runner-up"
  cp "$share/runner-up" "$HOME/.runner-up/runner-up"
  chmod 0755 "$HOME/.runner-up/runner-up"
  printf '#!/bin/bash\nexec %s fork-guard\n' "$HOME/.runner-up/runner-up" > "$HOME/.runner-up/fork-guard.sh"
  chmod 0755 "$HOME/.runner-up/fork-guard.sh"
  export ACTIONS_RUNNER_HOOK_JOB_STARTED="$HOME/.runner-up/fork-guard.sh"
fi
cd "$HOME/actions-runner"
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
unset jit
exec ./run.sh
`

func (tartProvider) shareDir(a *AgentRun) string { return filepath.Join(a.Dir(), "share") }

func (p tartProvider) clean(ctx context.Context, a *AgentRun) error {
	vm := CloneName(a.Place)
	vms, err := tartVMs(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if v.Name != vm || v.Source != "local" {
			continue
		}
		if v.Running {
			tart(ctx, "stop", "--timeout", "10", vm)
		}
		if out, err := tart(ctx, "delete", vm); err != nil {
			return fmt.Errorf("tart delete %s: %s", vm, FirstLine(string(out), err))
		}
	}
	return os.RemoveAll(p.shareDir(a))
}

func (p tartProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	if msg := tartReady(); msg != "ok" {
		return errors.New(msg)
	}
	tarball, err := runnerTarball(ctx, filepath.Join(a.StateDir, "cache"))
	if err != nil {
		return err
	}
	share, err := ensureDir(p.shareDir(a))
	if err != nil {
		return err
	}
	if err := linkOrCopy(tarball, filepath.Join(share, "runner.tar.gz")); err != nil {
		return err
	}
	if !a.AllowForks {
		if err := linkOrCopy(a.Self, filepath.Join(share, "runner-up")); err != nil {
			return err
		}
	}
	vm := CloneName(a.Place)
	if err := waitForVMSlot(ctx, vm, a.BootTimeout, out); err != nil {
		return err
	}
	if b, err := tart(ctx, "clone", a.Image, vm); err != nil {
		return fmt.Errorf("tart clone %s: %s", a.Image, FirstLine(string(b), err))
	}
	set := []string{"set", vm, "--cpu", strconv.Itoa(a.Cores), "--memory", strconv.Itoa(a.MemoryMB)}
	if a.DiskGB > 0 {
		set = append(set, "--disk-size", strconv.Itoa(a.DiskGB))
	}
	if b, err := tart(ctx, set...); err != nil {
		return fmt.Errorf("tart set: %s", FirstLine(string(b), err))
	}

	boot := exec.Command("tart", "run", "--no-graphics", "--dir="+tartShareName+":"+share+":ro", vm)
	boot.Stdout, boot.Stderr = out, out
	boot.WaitDelay = 5 * time.Second
	if err := boot.Start(); err != nil {
		return fmt.Errorf("tart run: %w", err)
	}
	vmDone := make(chan struct{})
	go func() { boot.Wait(); close(vmDone) }()
	defer func() {
		boot.Process.Signal(syscall.SIGTERM)
		select {
		case <-vmDone:
		case <-time.After(30 * time.Second):
			boot.Process.Kill()
			<-vmDone
		}
	}()
	if err := waitForGuest(ctx, vm, a.BootTimeout, vmDone); err != nil {
		return err
	}
	fmt.Fprintf(out, "runner-up agent: clone %s of %s is up\n", vm, a.Image)

	// A kernel panic restarts the clone's macOS and ends the runner with it,
	// but its tart exec can go on waiting. So the job ends once the clone
	// reads a new boot session, or the clone stops.
	jobCtx, endJob := context.WithCancel(ctx)
	defer endJob()
	restarted := make(chan struct{})
	go func() {
		if WatchBootSession(jobCtx, func(c context.Context) string { return bootSession(c, vm) }, a.GuestCheck) {
			close(restarted)
			endJob()
		}
	}()
	go func() {
		select {
		case <-vmDone:
			endJob()
		case <-jobCtx.Done():
		}
	}()
	cmd := exec.Command("tart", "exec", "-i", vm, "/bin/bash", "-c", tartScript, "runner-up", tartGuestShare, a.ToolCache)
	cmd.Stdin = strings.NewReader(jit + "\n")
	cmd.Stdout, cmd.Stderr = out, out
	// Once tart exec has ended, its output is not waited for.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tart exec: %w", err)
	}
	waitOrStop(jobCtx, cmd, func() { cmd.Process.Signal(syscall.SIGTERM) }, 30*time.Second)
	select {
	case <-restarted:
		return fmt.Errorf("the clone's macOS restarted during the job")
	default:
	}
	select {
	case <-vmDone:
		if ctx.Err() == nil {
			return fmt.Errorf("the clone stopped while the runner ran")
		}
	default:
	}
	return nil
}

// tart runs tart with args, bounded by two minutes, and returns its output.
func tart(ctx context.Context, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return exec.CommandContext(c, "tart", args...).CombinedOutput()
}

// tartReady says "ok" when this machine can run tart places, or why not.
func tartReady() string {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "tart places run only on Macs with Apple silicon"
	}
	if _, err := exec.LookPath("tart"); err != nil {
		return "tart is not installed (brew install cirruslabs/cli/tart)"
	}
	return "ok"
}

type tartVM struct {
	Name    string
	Source  string
	Running bool
}

func tartVMs(ctx context.Context) ([]tartVM, error) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "tart", "list", "--format", "json")
	cmd.Stderr = io.Discard
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	var vms []tartVM
	if err := json.Unmarshal(b, &vms); err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	return vms, nil
}

// runningMacVMs names the macOS virtual machines tart runs on this Mac: only
// those count toward the framework's limit. A running VM whose system cannot
// be read counts, since it may be one.
func runningMacVMs(ctx context.Context) ([]string, error) {
	vms, err := tartVMs(ctx)
	if err != nil {
		return nil, err
	}
	running := []string{}
	for _, vm := range vms {
		if vm.Source != "local" || !vm.Running {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		b, err := exec.CommandContext(c, "tart", "get", vm.Name, "--format", "json").Output()
		cancel()
		var got struct{ OS string }
		if err == nil && json.Unmarshal(b, &got) == nil && got.OS != "" && got.OS != "darwin" {
			continue
		}
		running = append(running, vm.Name)
	}
	return running, nil
}

// waitForVMSlot waits, up to timeout, until fewer than macOSVMLimit macOS
// VMs other than vm run on this Mac.
func waitForVMSlot(ctx context.Context, vm string, timeout time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	said := false
	for {
		vms, err := runningMacVMs(ctx)
		if err != nil {
			return err
		}
		var others []string
		for _, v := range vms {
			if v != vm {
				others = append(others, v)
			}
		}
		if len(others) < MacOSVMLimit {
			return nil
		}
		if !said {
			fmt.Fprintf(out, "runner-up agent: waiting for a macOS VM to stop: macOS runs at most %d at once, and %s run\n",
				MacOSVMLimit, strings.Join(others, ", "))
			said = true
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("macOS already ran %d VMs (%s) for %s", len(others), strings.Join(others, ", "), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForGuest waits until the clone's guest agent answers, up to timeout.
func waitForGuest(ctx context.Context, vm string, timeout time.Duration, vmDone <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := exec.CommandContext(c, "tart", "exec", vm, "/usr/bin/true").Run()
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the clone did not come up within %s", timeout)
		}
		select {
		case <-vmDone:
			return fmt.Errorf("the clone stopped while booting")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// bootSessionUUID is how macOS prints kern.bootsessionuuid: a UUID the kernel
// makes afresh at each boot. kern.boottime would not do: macOS moves it
// whenever the clock is set, as a new clone's clock is soon after boot.
var bootSessionUUID = regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// bootSession reads the clone's boot session, or "" when it cannot.
func bootSession(ctx context.Context, vm string) string {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	b, err := exec.CommandContext(c, "tart", "exec", vm, "/usr/sbin/sysctl", "-n", "kern.bootsessionuuid").Output()
	if now := strings.TrimSpace(string(b)); err == nil && bootSessionUUID.MatchString(now) {
		return now
	}
	return ""
}

// WatchBootSession reads the clone's boot session every interval until ctx
// ends, and reports true once a reading differs from the first: the clone's
// macOS restarted, and its runner went with it. A reading that fails, or is
// no UUID, counts for nothing, so a clone too busy to answer is never taken
// for a restarted one.
func WatchBootSession(ctx context.Context, read func(context.Context) string, interval time.Duration) bool {
	first := ""
	for {
		if now := read(ctx); bootSessionUUID.MatchString(now) {
			if first == "" {
				first = now
			} else if now != first {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// linkOrCopy puts src at dst, as a hard link where it can.
func linkOrCopy(src, dst string) error {
	os.Remove(dst)
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
