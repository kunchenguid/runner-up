// Package agent runs on each machine: it makes a place's container, virtual
// machine or host folder, runs one job in it and removes it.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/version"
)

// The agent is this same program on each machine, run by the coordinator
// over SSH (or directly on the coordinator's own machine) as
// `runner-up agent run`. It reads the runner's one-time registration from its
// first line of input, makes a fresh place with its provider, runs the runner
// there for one job and removes the place whatever happened. Its input stays
// open while the coordinator's place runner wants the runner; when it closes -
// the place runner ended, gave up, or the SSH connection dropped - the agent
// stops the runner and cleans up. Before making a place it removes anything
// left of the same place by an agent that was killed outright.

// provider makes a machine's places.
type provider interface {
	// run makes a fresh place, starts the runner in it with registration jit,
	// writes the runner's output to out and returns once the runner has ended
	// or ctx is done. The caller cleans up after it.
	run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error
	// clean removes everything left of the place, running or not.
	clean(ctx context.Context, a *AgentRun) error
}

type AgentRun struct {
	Provider string
	Place    string
	Cores    int
	MemoryMB int
	DiskGB   int
	Image    string
	// Switch is the Hyper-V switch a Hyper-V place joins.
	Switch string
	// ToolCache is the runner's tool cache for a host or tart place.
	ToolCache   string
	BootTimeout time.Duration
	// GuestCheck is how often a tart place reads its clone's boot session.
	GuestCheck time.Duration
	AllowForks bool
	// StateDir is the agent's own folder on its machine.
	StateDir string
	// Self is this program, which a place runs as its fork guard.
	Self string
}

func (a *AgentRun) Dir() string { return filepath.Join(a.StateDir, "places", a.Place) }

func providerFor(name string) (provider, error) {
	switch name {
	case config.ProviderDocker:
		return dockerProvider{}, nil
	case config.ProviderKVM:
		return kvmProvider{}, nil
	case config.ProviderTart:
		return tartProvider{}, nil
	case config.ProviderHost:
		return hostProvider{}, nil
	case config.ProviderAccount:
		return accountProvider{}, nil
	case config.ProviderHyperV:
		return hypervProvider{}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}

func AgentMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: runner-up agent probe|vms|run|clean")
		return 2
	}
	switch args[0] {
	case "probe":
		b, _ := json.MarshalIndent(probe(), "", "  ")
		fmt.Println(string(b))
		return 0
	case "vms":
		// The macOS virtual machines running here, for the coordinator's
		// count against the framework's limit.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		vms, err := runningMacVMs(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return ExitFailed
		}
		b, _ := json.Marshal(vms)
		fmt.Println(string(b))
		return 0
	case "run", "clean":
	default:
		fmt.Fprintln(os.Stderr, "unknown agent command", args[0])
		return 2
	}
	fs := flag.NewFlagSet("agent "+args[0], flag.ContinueOnError)
	a := &AgentRun{}
	fs.StringVar(&a.Provider, "provider", "", "docker, kvm, tart, host, account or hyperv")
	fs.StringVar(&a.Place, "place", "", "the place's id")
	fs.IntVar(&a.Cores, "cores", 2, "cores the place gets")
	fs.IntVar(&a.MemoryMB, "memory", 4096, "memory the place gets, in MB")
	fs.IntVar(&a.DiskGB, "disk-gb", 0, "a KVM place's disk size, 40 GB by default, or the size a tart clone or Hyper-V disk grows to")
	fs.StringVar(&a.Image, "image", "", "the runner image, a KVM or Hyper-V place's base disk image, or the tart image a tart place clones")
	fs.StringVar(&a.Switch, "switch", config.DefaultSwitch, "the Hyper-V switch a Hyper-V place joins")
	fs.StringVar(&a.ToolCache, "tool-cache", "", "the runner's tool cache: a host place's, kept between jobs (in the place's folder by default), or a folder inside a tart clone")
	boot := fs.Int("boot-timeout", 300, "seconds a KVM, tart or Hyper-V place may take to boot")
	guestCheck := fs.Int("guest-check", 30, "seconds between readings of a tart clone's boot session")
	fs.BoolVar(&a.AllowForks, "allow-forks", false, "run jobs from other repositories' pull requests")
	fs.StringVar(&a.StateDir, "state-dir", "", "the agent's folder; ~/.local/state/runner-up/agent by default, or %LOCALAPPDATA%\\runner-up\\agent on Windows")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	a.BootTimeout = time.Duration(*boot) * time.Second
	a.GuestCheck = time.Duration(max(*guestCheck, 1)) * time.Second
	if a.StateDir == "" {
		a.StateDir = filepath.Join(config.DefaultStateDir(), "agent")
	}
	if a.Place == "" || strings.ContainsAny(a.Place, "/ \t\n") {
		fmt.Fprintln(os.Stderr, "a plain --place is needed")
		return 2
	}
	p, err := providerFor(a.Provider)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if a.Self, err = os.Executable(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitFailed
	}
	if args[0] == "clean" {
		return agentClean(a, p)
	}
	return agentRunPlace(a, p, os.Stdin, os.Stdout)
}

// agentRunPlace is `agent run`.
func agentRunPlace(a *AgentRun, p provider, in io.Reader, stdout io.Writer) int {
	// Once the SSH connection is gone, writing output fails; the agent must
	// carry on and clean up rather than die of the broken pipe.
	signal.Ignore(syscall.SIGPIPE)
	out := &quietWriter{w: stdout}
	say := func(format string, v ...any) { fmt.Fprintf(out, "runner-up agent: "+format+"\n", v...) }

	r := bufio.NewReader(in)
	jit, err := r.ReadString('\n')
	jit = strings.TrimSpace(jit)
	if jit == "" {
		say("no runner registration on input (%v)", err)
		return ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		io.Copy(io.Discard, r)
		cancel()
	}()

	unlock, err := LockPlace(ctx, a)
	if err != nil {
		say("%v", err)
		return ExitFailed
	}
	defer unlock()
	cleanUp := func() {
		c, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		if err := p.clean(c, a); err != nil {
			say("could not clean up place %s: %v", a.Place, err)
		}
	}
	cleanUp()
	defer cleanUp()
	say("making place %s with %s (%d cores, %d MB)", a.Place, a.Provider, a.Cores, a.MemoryMB)
	if err := p.run(ctx, a, jit, out); err != nil {
		say("place %s failed: %v", a.Place, err)
		return ExitFailed
	}
	if ctx.Err() != nil {
		say("place %s stopped", a.Place)
	} else {
		say("place %s finished", a.Place)
	}
	return ExitDone
}

func agentClean(a *AgentRun, p provider) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	unlock, err := LockPlace(ctx, a)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitFailed
	}
	defer unlock()
	if err := p.clean(ctx, a); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitFailed
	}
	return ExitDone
}

// LockPlace keeps two agents out of one place: a new run waits for an earlier
// agent still cleaning up, for up to two minutes. Its release deletes the
// lock file and the place's folder, if nothing else is left in it, so a
// finished place leaves nothing behind. The file is deleted while still
// locked, so an agent that opened it before then and locks it after finds it
// is no longer the place's lock file and tries again.
func LockPlace(ctx context.Context, a *AgentRun) (func(), error) {
	path := filepath.Join(a.Dir(), "agent.lock")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := os.MkdirAll(a.Dir(), 0o700); err != nil {
			return nil, err
		}
		f, err := lock.Busy(path)
		if errors.Is(err, os.ErrNotExist) {
			// The holder before deleted the folder in between.
			continue
		}
		if err != nil {
			return nil, err
		}
		if f != nil && isFileAt(f, path) {
			return func() {
				os.Remove(path)
				f.Close()
				os.Remove(a.Dir())
			}, nil
		}
		if f != nil {
			// The holder before deleted it: lock the file now at path.
			f.Close()
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("place %s is still held by another agent", a.Place)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("stopped while waiting for place %s", a.Place)
		case <-time.After(time.Second):
		}
	}
}

// isFileAt says whether the open file f is the one now at path.
func isFileAt(f *os.File, path string) bool {
	held, err1 := f.Stat()
	now, err2 := os.Stat(path)
	return err1 == nil && err2 == nil && os.SameFile(held, now)
}

// quietWriter drops write errors: output is for the coordinator's log, and a
// lost connection must not stop the clean-up.
type quietWriter struct{ w io.Writer }

func (q *quietWriter) Write(b []byte) (int, error) {
	q.w.Write(b)
	return len(b), nil
}

// waitOrStop waits for cmd. When ctx is done first, it calls stop, then kills
// the process if it has not ended within grace.
func waitOrStop(ctx context.Context, cmd *exec.Cmd, stop func(), grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	if stop != nil {
		stop()
	}
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		cmd.Process.Kill()
		return <-done
	}
}

// Probe is what `agent probe` reports about its machine, for `machine add`.
type Probe struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Cores    int    `json:"cores"`
	MemoryMB int    `json:"memoryMB"`
	Docker   string `json:"docker"`
	KVM      string `json:"kvm"`
	Tart     string `json:"tart"`
	Host     string `json:"host"`
	// Account and HyperV are for Windows machines.
	Account string `json:"account,omitempty"`
	HyperV  string `json:"hyperv,omitempty"`
}

// probe reads the machine: "ok" for each provider that can run here, or why
// not.
func probe() Probe {
	p := Probe{Version: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU(), MemoryMB: memoryMB()}
	if runtime.GOOS == "windows" {
		p.Account = accountReady()
		p.HyperV = hypervReady()
		p.Docker = "Windows machines run account and hyperv places"
		p.KVM, p.Tart, p.Host = p.Docker, p.Docker, p.Docker
		return p
	}
	p.Docker = "ok"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		p.Docker = FirstLine(string(out), err)
	}
	p.KVM = kvmReady()
	p.Tart = tartReady()
	p.Host = hostReady()
	return p
}

func FirstLine(out string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if line == "" && err != nil {
		line = err.Error()
	}
	return line
}

// ensureDir makes dir and returns it.
func ensureDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// Exit codes of run-place and agent run.
const (
	ExitDone      = 0
	ExitFailed    = 1
	ExitUnstarted = 3
)
